package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type recordedRequests struct {
	mutex sync.Mutex
	seen  []string
}

func (r *recordedRequests) add(request *http.Request) {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	r.seen = append(r.seen, request.Method+" "+request.URL.Path)
}

func (r *recordedRequests) writes() []string {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	writes := []string{}
	for _, entry := range r.seen {
		if !strings.HasPrefix(entry, "GET ") {
			writes = append(writes, entry)
		}
	}
	return writes
}

func startFakePocketBase(t *testing.T) (*httptest.Server, *recordedRequests) {
	t.Helper()
	requests := &recordedRequests{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.add(request)
		writer.Header().Set("Content-Type", "application/json")
		if request.Method != http.MethodGet {
			writer.Write([]byte(`{"id":"new1","title":"made"}`))
			return
		}
		writer.Write([]byte(`{"page":1,"perPage":20,"totalItems":1,"totalPages":1,"items":[{"id":"abc","title":"Hello"}]}`))
	}))
	t.Cleanup(server.Close)
	return server, requests
}

func useProfile(t *testing.T, url string, readOnly bool) {
	t.Helper()
	directory := t.TempDir()
	t.Setenv("PBCTL_CONFIG", filepath.Join(directory, "config.json"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(directory, "cache"))
	for _, name := range []string{"PBCTL_PROFILE", "PBCTL_READ_ONLY", "PBCTL_URL", "PBCTL_OUTPUT"} {
		t.Setenv(name, "")
	}
	raw, err := json.Marshal(map[string]any{
		"current":  "main",
		"profiles": map[string]any{"main": map[string]any{"url": url, "token": "static-token", "read_only": readOnly}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(os.Getenv("PBCTL_CONFIG"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func connectMCP(t *testing.T, readOnly bool) *mcp.ClientSession {
	t.Helper()
	application := &app{version: "test"}
	if err := application.selectProfile(); err != nil {
		t.Fatal(err)
	}
	if (application.readOnlySource != "") != readOnly {
		t.Fatalf("read-only source = %q, want read-only %v", application.readOnlySource, readOnly)
	}
	server := newMCPServer(&mcpBridge{version: "test", pinned: application.pinnedArguments()}, !readOnly)
	serverSide, clientSide := mcp.NewInMemoryTransports()
	if _, err := server.Connect(t.Context(), serverSide, nil); err != nil {
		t.Fatal(err)
	}
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "test"}, nil).Connect(t.Context(), clientSide, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

func toolNames(t *testing.T, session *mcp.ClientSession) []string {
	t.Helper()
	listed, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, tool := range listed.Tools {
		names = append(names, tool.Name)
	}
	return names
}

func callTool(t *testing.T, session *mcp.ClientSession, name string, arguments map[string]any) (string, bool) {
	t.Helper()
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: arguments})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	text := ""
	for _, content := range result.Content {
		if part, isText := content.(*mcp.TextContent); isText {
			text += part.Text
		}
	}
	return text, result.IsError
}

func TestMCPOffersNoWritingToolsOnAReadOnlyProfile(t *testing.T) {
	server, requests := startFakePocketBase(t)
	useProfile(t, server.URL, true)
	session := connectMCP(t, true)

	names := toolNames(t, session)
	for _, writing := range []string{"records_create", "records_update", "records_delete", "api_write"} {
		if slices.Contains(names, writing) {
			t.Errorf("read-only server offers %s", writing)
		}
	}
	if !slices.Contains(names, "records_list") {
		t.Fatalf("records_list is missing from %v", names)
	}

	text, failed := callTool(t, session, "records_list", map[string]any{"collection": "posts", "fields": "id,title"})
	if failed || !strings.Contains(text, "abc\tHello") {
		t.Fatalf("records_list = %q (error %v)", text, failed)
	}
	if writes := requests.writes(); len(writes) != 0 {
		t.Fatalf("a read-only server sent %v", writes)
	}
}

func TestMCPArgumentsCannotSmuggleFlags(t *testing.T) {
	server, requests := startFakePocketBase(t)
	useProfile(t, server.URL, true)
	session := connectMCP(t, true)

	for _, hostile := range []string{"--read-only=false", "-pother", "--yes"} {
		callTool(t, session, "records_get", map[string]any{"collection": hostile, "id": "abc"})
	}
	callTool(t, session, "api_get", map[string]any{"path": "/api/collections/posts/records", "query": map[string]any{"x": "--dry-run"}})
	text, failed := callTool(t, session, "api_get", map[string]any{"path": "--data={}"})
	if !failed {
		t.Fatalf("a flag-shaped path was accepted: %q", text)
	}
	if writes := requests.writes(); len(writes) != 0 {
		t.Fatalf("a read-only server sent %v", writes)
	}
}

func TestMCPWritesNeedAWritableProfileAndDeletesNeedConfirm(t *testing.T) {
	server, requests := startFakePocketBase(t)
	useProfile(t, server.URL, false)
	session := connectMCP(t, false)

	text, failed := callTool(t, session, "records_delete", map[string]any{"collection": "posts", "ids": []any{"abc"}, "confirm": false})
	if !failed || !strings.Contains(text, "confirm: true") {
		t.Fatalf("an unconfirmed delete answered %q (error %v)", text, failed)
	}
	if writes := requests.writes(); len(writes) != 0 {
		t.Fatalf("an unconfirmed delete sent %v", writes)
	}

	if text, failed := callTool(t, session, "records_create", map[string]any{"collection": "posts", "data": map[string]any{"title": "made"}}); failed {
		t.Fatalf("records_create failed: %q", text)
	}
	if text, failed := callTool(t, session, "records_delete", map[string]any{"collection": "posts", "ids": []any{"abc"}, "confirm": true}); failed {
		t.Fatalf("a confirmed delete failed: %q", text)
	}
	want := []string{"POST /api/collections/posts/records", "DELETE /api/collections/posts/records/abc"}
	if writes := requests.writes(); !slices.Equal(writes, want) {
		t.Fatalf("writes = %v, want %v", writes, want)
	}
}

func TestMCPResultsAreCapped(t *testing.T) {
	capped := capResult(strings.Repeat("row\n", mcpResultLimit))
	if len(capped) > mcpResultLimit+200 || !strings.Contains(capped, "[cut:") {
		t.Fatalf("result of %d characters was not capped", len(capped))
	}
}
