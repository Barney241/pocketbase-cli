package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Barney241/pocketbase-cli/internal/cli"
	"github.com/Barney241/pocketbase-cli/internal/gateway"
)

const (
	envTestServer = "PBCTL_TEST_SERVER"
	adminEmail    = "admin@test.dev"
	guardedEmail  = "ro@test.dev"
	guardedByHook = "pb_hooks file"
	guardedByGo   = "pbguard Go module"
	testPassword  = "Test1234!pbctl"
	exitBlocked   = 3
	exitAuth      = 4
)

type instance struct {
	t   *testing.T
	url string
}

type result struct {
	code   int
	stdout string
	stderr string
}

func startPocketBase(t *testing.T, guardKind string) *instance {
	t.Helper()
	binary := os.Getenv(envTestServer)
	if binary == "" {
		t.Skipf("set %s to a built testdata/pbserver binary to run the integration tests (see `make integration`)", envTestServer)
	}
	workDir := t.TempDir()
	dataDir := filepath.Join(workDir, "pb_data")
	hooksDir := filepath.Join(workDir, "pb_hooks")
	t.Setenv("PBCTL_CONFIG", filepath.Join(workDir, "config.json"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(workDir, "cache"))
	t.Setenv("PBCTL_PROFILE", "")
	t.Setenv("PBCTL_READ_ONLY", "")
	t.Setenv("PBCTL_URL", "")
	t.Setenv("PBCTL_OUTPUT", "")

	for _, email := range []string{adminEmail, guardedEmail} {
		if out, err := exec.Command(binary, "superuser", "upsert", email, testPassword, "--dir", dataDir).CombinedOutput(); err != nil {
			t.Fatalf("create superuser %s: %v\n%s", email, err, out)
		}
	}
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	serverEnv := append(os.Environ(), "PBSERVER_HOOKS_DIR="+hooksDir)
	if guardKind == guardedByGo {
		serverEnv = append(serverEnv, "PBSERVER_GO_GUARD="+guardedEmail)
	} else {
		hook := run(t, "", "guard", "hook", "--superuser", guardedEmail)
		if err := os.WriteFile(filepath.Join(hooksDir, "pbctl_readonly.pb.js"), []byte(hook.stdout), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	address := freeAddress(t)
	server := exec.Command(binary, "serve", "--http", address, "--dir", dataDir, "--dev=false")
	server.Env = serverEnv
	serverLog := &bytes.Buffer{}
	server.Stdout, server.Stderr = serverLog, serverLog
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		server.Process.Kill()
		server.Wait()
		if t.Failed() {
			t.Logf("server log:\n%s", serverLog.String())
		}
	})
	pocketBase := &instance{t: t, url: "http://" + address}
	pocketBase.waitUntilHealthy()
	pocketBase.writeProfiles(map[string]map[string]any{
		"admin":   {"url": pocketBase.url, "identity": adminEmail, "password": testPassword, "read_only": false},
		"locked":  {"url": pocketBase.url, "identity": adminEmail, "password": testPassword, "read_only": true},
		"guarded": {"url": pocketBase.url, "identity": guardedEmail, "password": testPassword, "read_only": false},
	})
	return pocketBase
}

func freeAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().String()
}

func (i *instance) waitUntilHealthy() {
	i.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		response, err := http.Get(i.url + "/api/health")
		if err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	i.t.Fatal("the test PocketBase never became healthy")
}

func (i *instance) writeProfiles(profiles map[string]map[string]any) {
	i.t.Helper()
	raw, err := json.Marshal(map[string]any{"current": "admin", "profiles": profiles})
	if err != nil {
		i.t.Fatal(err)
	}
	if err := os.WriteFile(os.Getenv("PBCTL_CONFIG"), raw, 0o600); err != nil {
		i.t.Fatal(err)
	}
}

func (i *instance) addProfile(name string, profile map[string]any) {
	i.t.Helper()
	file := map[string]any{}
	raw, err := os.ReadFile(os.Getenv("PBCTL_CONFIG"))
	if err != nil {
		i.t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		i.t.Fatal(err)
	}
	file["profiles"].(map[string]any)[name] = profile
	updated, _ := json.Marshal(file)
	if err := os.WriteFile(os.Getenv("PBCTL_CONFIG"), updated, 0o600); err != nil {
		i.t.Fatal(err)
	}
}

func (i *instance) token(email string) string {
	i.t.Helper()
	credentials, _ := json.Marshal(map[string]string{"identity": email, "password": testPassword})
	response, err := http.Post(i.url+"/api/collections/_superusers/auth-with-password", "application/json", bytes.NewReader(credentials))
	if err != nil {
		i.t.Fatal(err)
	}
	defer response.Body.Close()
	var payload struct {
		Token string `json:"token"`
	}
	json.NewDecoder(response.Body).Decode(&payload)
	if payload.Token == "" {
		i.t.Fatalf("no token for %s (status %d)", email, response.StatusCode)
	}
	return payload.Token
}

func run(t *testing.T, stdin string, arguments ...string) result {
	t.Helper()
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	code := cli.Run(ctx, "test", arguments, strings.NewReader(stdin), stdout, stderr)
	return result{code: code, stdout: stdout.String(), stderr: stderr.String()}
}

func mustRun(t *testing.T, arguments ...string) string {
	t.Helper()
	outcome := run(t, "", arguments...)
	if outcome.code != 0 {
		t.Fatalf("pbctl %s exited %d\nstdout: %s\nstderr: %s", strings.Join(arguments, " "), outcome.code, outcome.stdout, outcome.stderr)
	}
	return outcome.stdout
}

func seedPosts(t *testing.T) (firstID string) {
	t.Helper()
	definition := `{"name":"posts","type":"base","listRule":"owner = @request.auth.id","viewRule":"","fields":[
		{"name":"title","type":"text","required":true},
		{"name":"views","type":"number"},
		{"name":"owner","type":"text"},
		{"name":"doc","type":"file","maxSelect":1,"protected":true}]}`
	mustRun(t, "collections", "create", "-d", definition)
	mustRun(t, "collections", "create", "-d", `{"name":"members","type":"auth"}`)
	mustRun(t, "settings", "update", "batch.enabled:=true", "batch.maxRequests:=100", "--yes")
	firstID = strings.TrimSpace(mustRun(t, "records", "create", "posts", "title=O'Brien's plan", "views:=7", "-q"))
	mustRun(t, "records", "create", "posts", "title=second", "views:=1")
	return firstID
}

func TestRecordsCanBeWrittenReadFilteredAndCounted(t *testing.T) {
	startPocketBase(t, guardedByHook)
	firstID := seedPosts(t)

	listed := mustRun(t, "records", "list", "posts", "-f", "title = {:t}", "--param", "t=O'Brien's plan", "--fields", "id,title,views")
	if !strings.Contains(listed, "id\ttitle\tviews") || !strings.Contains(listed, firstID+"\tO'Brien's plan\t7") {
		t.Fatalf("unexpected table:\n%s", listed)
	}
	if capped := mustRun(t, "records", "list", "posts", "--limit", "1", "--fields", "id", "-o", "jsonl"); strings.Count(capped, "\n") != 1 {
		t.Fatalf("--limit 1 returned more than one row:\n%s", capped)
	}
	if count := strings.TrimSpace(mustRun(t, "records", "count", "posts", "-f", "views > 5")); count != "1" {
		t.Fatalf("count = %q, want 1", count)
	}
	mustRun(t, "records", "update", "posts", firstID, "views+:=3")
	fetched := mustRun(t, "records", "get", "posts", firstID, "-o", "json")
	if !strings.Contains(fetched, `"views":10`) {
		t.Fatalf("the + modifier did not add to views: %s", fetched)
	}

	unconfirmed := run(t, "", "records", "delete", "posts", firstID)
	if unconfirmed.code != 6 {
		t.Fatalf("delete without --yes exited %d, want 6", unconfirmed.code)
	}
	preview := mustRun(t, "records", "delete", "posts", firstID, "--dry-run")
	if !strings.Contains(preview, `"dryRun":true`) || !strings.Contains(preview, `"method":"DELETE"`) {
		t.Fatalf("unexpected dry run: %s", preview)
	}
	if count := strings.TrimSpace(mustRun(t, "records", "count", "posts")); count != "2" {
		t.Fatalf("a dry run or an unconfirmed delete removed a record: count = %s", count)
	}
	mustRun(t, "records", "delete", "posts", firstID, "--yes")
	missing := run(t, "", "records", "get", "posts", firstID)
	if missing.code != 5 {
		t.Fatalf("get of a deleted record exited %d, want 5", missing.code)
	}
}

func TestImportListAllAndLongValuesAreCutOnlyInTables(t *testing.T) {
	startPocketBase(t, guardedByHook)
	seedPosts(t)
	rows := &strings.Builder{}
	for index := range 120 {
		fmt.Fprintf(rows, "{\"title\":\"row %d %s\",\"views\":%d}\n", index, strings.Repeat("x", 200), index)
	}
	imported := run(t, rows.String(), "records", "import", "posts", "-")
	if imported.code != 0 {
		t.Fatalf("import failed: %s", imported.stderr)
	}
	everything := mustRun(t, "records", "list", "posts", "--all", "-o", "jsonl", "--fields", "id,title")
	if lines := strings.Count(everything, "\n"); lines != 122 {
		t.Fatalf("--all returned %d rows, want 122", lines)
	}
	if strings.Contains(everything, "…(+") {
		t.Fatal("jsonl output was truncated")
	}
	page := run(t, "", "records", "list", "posts", "-n", "5", "-f", "title ~ 'row'", "--fields", "id,title")
	if !strings.Contains(page.stdout, "…(+") {
		t.Fatalf("table output kept a 200 character value whole:\n%s", page.stdout)
	}
	if !strings.Contains(page.stderr, "rows 1-5 of 120; next: --page 2") {
		t.Fatalf("missing the paging note: %s", page.stderr)
	}
}

func TestSchemaLogsSettingsAndFilesAreReadable(t *testing.T) {
	startPocketBase(t, guardedByHook)
	seedPosts(t)

	schema := mustRun(t, "collections", "show", "posts")
	for _, expected := range []string{"posts (base)", "title text required", "doc file", "list: owner = @request.auth.id", "view: (anyone)", "create: (superusers only)"} {
		if !strings.Contains(schema, expected) {
			t.Fatalf("schema is missing %q:\n%s", expected, schema)
		}
	}
	if listed := mustRun(t, "collections", "list"); !strings.Contains(listed, "posts\tbase\tid,title,views,owner,doc") {
		t.Fatalf("unexpected collection list:\n%s", listed)
	}
	if sample := mustRun(t, "collections", "dry-run-view", "SELECT id, title FROM posts"); !strings.Contains(sample, "second") {
		t.Fatalf("dry-run-view returned no rows:\n%s", sample)
	}
	if settings := mustRun(t, "settings", "get", "batch"); !strings.Contains(settings, "batch.enabled: true") {
		t.Fatalf("unexpected settings:\n%s", settings)
	}
	mustRun(t, "logs", "list", "--level", "info", "--since", "1h", "--status", "2xx")
	mustRun(t, "logs", "stats")
	mustRun(t, "crons", "list")
	mustRun(t, "collections", "scaffolds")
	mustRun(t, "auth", "methods", "members")
	if status := mustRun(t, "status"); !strings.Contains(status, adminEmail) || !strings.Contains(status, "mode: writable") {
		t.Fatalf("unexpected status:\n%s", status)
	}

	attachment := filepath.Join(t.TempDir(), "plan.txt")
	os.WriteFile(attachment, []byte("the protected plan"), 0o600)
	recordID := strings.TrimSpace(mustRun(t, "records", "create", "posts", "title=with file", "--file", "doc="+attachment, "-q"))
	files := mustRun(t, "files", "list", "posts", recordID)
	fields := strings.Fields(strings.Split(strings.TrimSpace(files), "\n")[1])
	if len(fields) != 3 || fields[0] != "doc" || fields[2] != "true" {
		t.Fatalf("unexpected file list:\n%s", files)
	}
	saved := filepath.Join(t.TempDir(), "downloaded.txt")
	mustRun(t, "files", "download", "posts", recordID, fields[1], "--to", saved)
	if content, _ := os.ReadFile(saved); string(content) != "the protected plan" {
		t.Fatalf("downloaded %q", content)
	}
}

func writeAttempts(recordID string) [][]string {
	return [][]string{
		{"records", "create", "posts", "title=blocked"},
		{"records", "update", "posts", recordID, "title=changed"},
		{"records", "delete", "posts", recordID, "--yes"},
		{"records", "import", "posts", "-"},
		{"records", "batch", "-d", `[{"method":"DELETE","url":"/api/collections/posts/records/` + recordID + `"}]`, "--yes"},
		{"collections", "create", "-d", `{"name":"extra","type":"base"}`},
		{"collections", "update", "posts", "listRule=", "--yes"},
		{"collections", "truncate", "posts", "--yes"},
		{"collections", "delete", "posts", "--yes"},
		{"collections", "import", "-", "--yes"},
		{"settings", "update", "meta.appName=changed", "--yes"},
		{"settings", "test-email", "--to", "someone@test.dev"},
		{"backups", "create", "blocked.zip"},
		{"backups", "delete", "blocked.zip", "--yes"},
		{"backups", "restore", "blocked.zip", "--yes"},
		{"crons", "run", "__pbLogsCleanup__"},
		{"logs", "truncate", "--yes"},
		{"sql", "DELETE FROM posts", "--yes"},
		{"sql", "SELECT 1"},
		{"auth", "impersonate", "_superusers", "anyid"},
		{"auth", "request-password-reset", "_superusers", "email=" + adminEmail},
		{"api", "POST", "/api/collections/posts/records", "title=blocked"},
		{"api", "PATCH", "/api/collections/posts/records/" + recordID, "title=changed"},
		{"api", "PUT", "/api/collections/import", "--yes"},
		{"api", "DELETE", "/api/collections/posts/records/" + recordID, "--yes"},
		{"api", "POST", "/api/collections/posts/../posts/records", "title=blocked"},
	}
}

const importStdin = "[{\"title\":\"blocked\"}]"

func TestAReadOnlyClientSendsNoWriteWhateverTurnsReadOnlyOn(t *testing.T) {
	startPocketBase(t, guardedByHook)
	recordID := seedPosts(t)
	before := mustRun(t, "records", "list", "posts", "-o", "json") + mustRun(t, "collections", "show", "posts", "-o", "json") + mustRun(t, "settings", "get", "-o", "json")

	modes := map[string]func([]string) []string{
		"profile": func(arguments []string) []string { return append([]string{"-p", "locked"}, arguments...) },
		"flag":    func(arguments []string) []string { return append([]string{"-p", "admin", "--read-only"}, arguments...) },
		"env":     func(arguments []string) []string { return append([]string{"-p", "admin"}, arguments...) },
	}
	for mode, withMode := range modes {
		t.Run(mode, func(t *testing.T) {
			if mode == "env" {
				t.Setenv("PBCTL_READ_ONLY", "1")
			}
			for _, attempt := range writeAttempts(recordID) {
				outcome := run(t, importStdin, withMode(attempt)...)
				if outcome.code != exitBlocked {
					t.Errorf("pbctl %s exited %d, want %d\nstderr: %s", strings.Join(attempt, " "), outcome.code, exitBlocked, outcome.stderr)
				}
			}
			mustRun(t, withMode([]string{"records", "list", "posts"})...)
			mustRun(t, withMode([]string{"collections", "dry-run-view", "SELECT id FROM posts"})...)
			mustRun(t, withMode([]string{"records", "list", "posts", "--as", "_superusers/" + superuserID(t)})...)
		})
	}

	after := mustRun(t, "records", "list", "posts", "-o", "json") + mustRun(t, "collections", "show", "posts", "-o", "json") + mustRun(t, "settings", "get", "-o", "json")
	if before != after {
		t.Fatalf("data changed although every write was refused\nbefore: %s\nafter:  %s", before, after)
	}
}

func superuserID(t *testing.T) string {
	t.Helper()
	listed := mustRun(t, "-p", "admin", "records", "list", "_superusers", "-f", "email = '"+adminEmail+"'", "--fields", "id", "-o", "jsonl")
	var record struct {
		ID string `json:"id"`
	}
	json.Unmarshal([]byte(listed), &record)
	return record.ID
}

func TestAServerGuardedSuperuserCannotWriteWithAnyClient(t *testing.T) {
	for _, guardKind := range []string{guardedByHook, guardedByGo} {
		t.Run(guardKind, func(t *testing.T) {
			pocketBase := startPocketBase(t, guardKind)
			recordID := seedPosts(t)
			mustRun(t, "backups", "create", "kept.zip")

			if status := mustRun(t, "-p", "guarded", "status"); !strings.Contains(status, "enforcement: server hook") {
				t.Fatalf("status does not report server enforcement:\n%s", status)
			}
			if listed := mustRun(t, "-p", "guarded", "records", "list", "posts", "--fields", "id,title"); !strings.Contains(listed, "second") {
				t.Fatalf("a guarded superuser cannot read:\n%s", listed)
			}
			mustRun(t, "-p", "guarded", "records", "watch", "posts", "--duration", "300ms")
			mustRun(t, "-p", "guarded", "backups", "list")
			refusals := append(writeAttempts(recordID),
				[]string{"backups", "download", "kept.zip", "--to", filepath.Join(t.TempDir(), "stolen.zip")},
				[]string{"collections", "dry-run-view", "SELECT id, tokenKey FROM _superusers"},
				[]string{"records", "list", "_superusers", "-f", "tokenKey ~ 'a'"},
				[]string{"records", "list", "_superusers", "-s", "password"},
				[]string{"records", "watch", "_superusers", "-f", "tokenKey ~ 'a'", "--duration", "300ms"},
			)
			for _, attempt := range refusals {
				outcome := run(t, importStdin, append([]string{"-p", "guarded"}, attempt...)...)
				if outcome.code != exitBlocked || !strings.Contains(outcome.stderr, "pbctl guard") {
					t.Errorf("pbctl %s exited %d, want %d with a guard message\nstderr: %s", strings.Join(attempt, " "), outcome.code, exitBlocked, outcome.stderr)
				}
			}

			request, _ := http.NewRequest(http.MethodPost, pocketBase.url+"/api/collections/posts/records", strings.NewReader(`{"title":"raw http"}`))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Authorization", pocketBase.token(guardedEmail))
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if response.StatusCode != http.StatusForbidden {
				t.Errorf("a raw POST as the guarded superuser returned %d, want 403", response.StatusCode)
			}
			if count := strings.TrimSpace(mustRun(t, "records", "count", "posts")); count != "2" {
				t.Fatalf("a guarded superuser changed the data: count = %s", count)
			}
			openAProtectedFileWithAToken(t, "admin")
			logged := waitForLoggedFileRequest(t, "guarded")
			if strings.Contains(logged, "eyJ") || !strings.Contains(logged, "token=REDACTED") {
				t.Fatalf("a file token is readable in the logs:\n%s", logged)
			}
			mustRun(t, "backups", "download", "kept.zip", "--to", filepath.Join(t.TempDir(), "kept.zip"))
		})
	}
}

func TestTheGatewayServesReadsAndHoldsTheCredential(t *testing.T) {
	pocketBase := startPocketBase(t, guardedByHook)
	recordID := seedPosts(t)
	mustRun(t, "backups", "create", "kept.zip")
	memberID := strings.TrimSpace(mustRun(t, "records", "create", "members", "email=ann@test.dev", "password="+testPassword, "passwordConfirm="+testPassword, "-q"))
	mustRun(t, "records", "update", "posts", recordID, "owner="+memberID)
	attachment := filepath.Join(t.TempDir(), "plan.txt")
	os.WriteFile(attachment, []byte("through the gateway"), 0o600)
	mustRun(t, "records", "update", "posts", recordID, "--file", "doc="+attachment)

	upstream, _ := url.Parse(pocketBase.url)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	accessLog := &bytes.Buffer{}
	handler := gateway.New(gateway.Config{Upstream: upstream, Collection: "_superusers", Identity: adminEmail, Password: testPassword, AllowedHosts: []string{listener.Addr().String()}, Log: accessLog})
	server := &httptest.Server{Listener: listener, Config: &http.Server{Handler: handler}}
	server.Start()
	t.Cleanup(server.Close)
	pocketBase.addProfile("gw", map[string]any{"url": server.URL, "read_only": false})

	if status := mustRun(t, "-p", "gw", "status"); !strings.Contains(status, "enforcement: gateway") {
		t.Fatalf("status does not report the gateway:\n%s", status)
	}
	if listed := mustRun(t, "-p", "gw", "records", "list", "posts", "--fields", "id,title"); !strings.Contains(listed, "second") {
		t.Fatalf("no rows through the gateway:\n%s", listed)
	}
	asMember := mustRun(t, "-p", "gw", "--as", "members/"+memberID, "records", "list", "posts", "--fields", "id,title")
	if !strings.Contains(asMember, recordID) || strings.Contains(asMember, "second") {
		t.Fatalf("--as through the gateway did not apply the member's list rule:\n%s", asMember)
	}
	files := mustRun(t, "-p", "gw", "files", "list", "posts", recordID)
	filename := strings.Fields(strings.Split(strings.TrimSpace(files), "\n")[1])[1]
	saved := filepath.Join(t.TempDir(), "via-gateway.txt")
	mustRun(t, "-p", "gw", "files", "download", "posts", recordID, filename, "--to", saved)
	if content, _ := os.ReadFile(saved); string(content) != "through the gateway" {
		t.Fatalf("downloaded %q through the gateway", content)
	}
	mustRun(t, "-p", "gw", "records", "watch", "posts", "--duration", "300ms")
	openAProtectedFileWithAToken(t, "admin")
	if logged := waitForLoggedFileRequest(t, "gw"); strings.Contains(logged, "eyJ") || !strings.Contains(logged, "token=REDACTED") {
		t.Fatalf("a file token is readable in the logs through the gateway:\n%s", logged)
	}

	refusals := append(writeAttempts(recordID),
		[]string{"backups", "download", "kept.zip", "--to", filepath.Join(t.TempDir(), "stolen.zip")},
		[]string{"collections", "dry-run-view", "SELECT id, tokenKey FROM _superusers"},
		[]string{"records", "list", "_superusers", "-f", "tokenKey ~ 'a'"},
		[]string{"records", "watch", "_superusers", "-f", "tokenKey ~ 'a'", "--duration", "300ms"},
		[]string{"auth", "login", "_superusers", "identity=" + adminEmail, "password=" + testPassword},
		[]string{"auth", "refresh", "_superusers"},
		[]string{"files", "url", "posts", recordID, filename, "--token"},
	)
	for _, attempt := range refusals {
		outcome := run(t, importStdin, append([]string{"-p", "gw"}, attempt...)...)
		if outcome.code != exitBlocked || !strings.Contains(outcome.stderr, "read-only gateway") {
			t.Errorf("pbctl %s exited %d, want %d with a gateway message\nstderr: %s", strings.Join(attempt, " "), outcome.code, exitBlocked, outcome.stderr)
		}
	}

	fromWebPage, _ := http.NewRequest(http.MethodGet, server.URL+"/api/collections/posts/records", nil)
	fromWebPage.Header.Set("Origin", "https://evil.example")
	rebound, _ := http.NewRequest(http.MethodGet, server.URL+"/api/collections/posts/records", nil)
	rebound.Host = "evil.example"
	for name, request := range map[string]*http.Request{"a web page": fromWebPage, "a rebound host": rebound} {
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusForbidden {
			t.Errorf("a request from %s returned %d, want 403", name, response.StatusCode)
		}
	}
	plain, err := http.Get(server.URL + "/api/collections/posts/records")
	if err != nil {
		t.Fatal(err)
	}
	plain.Body.Close()
	for header := range plain.Header {
		if strings.HasPrefix(strings.ToLower(header), "access-control-") {
			t.Errorf("the gateway passed the CORS header %s on", header)
		}
	}
	if count := strings.TrimSpace(mustRun(t, "records", "count", "posts")); count != "2" {
		t.Fatalf("data changed through the gateway: count = %s", count)
	}
}

func TestSchemaDiffReportsWhatDiffersBetweenTwoInstances(t *testing.T) {
	pocketBase := startPocketBase(t, guardedByHook)
	seedPosts(t)
	if same := run(t, "", "collections", "diff", "locked"); same.code != 0 || !strings.Contains(same.stderr, "schemas match") {
		t.Fatalf("an instance differs from itself: %s %s", same.stdout, same.stderr)
	}
	exported := mustRun(t, "collections", "export", "posts")
	if !strings.Contains(exported, `"name":"posts"`) {
		t.Fatalf("unexpected export: %s", exported)
	}
	_ = pocketBase
}

func openAProtectedFileWithAToken(t *testing.T, profile string) {
	t.Helper()
	attachment := filepath.Join(t.TempDir(), "secret.txt")
	os.WriteFile(attachment, []byte("secret"), 0o600)
	recordID := strings.TrimSpace(mustRun(t, "-p", profile, "records", "create", "posts", "title=holds a file", "--file", "doc="+attachment, "-q"))
	files := mustRun(t, "-p", profile, "files", "list", "posts", recordID)
	filename := strings.Fields(strings.Split(strings.TrimSpace(files), "\n")[1])[1]
	withToken := strings.TrimSpace(mustRun(t, "-p", profile, "files", "url", "posts", recordID, filename, "--token"))
	response, err := http.Get(withToken)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("a protected file did not open with its token: %d", response.StatusCode)
	}
	mustRun(t, "-p", profile, "records", "delete", "posts", recordID, "--yes")
}

func waitForLoggedFileRequest(t *testing.T, profile string) string {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		logged := mustRun(t, "-p", profile, "logs", "list", "-f", "data.url ~ '/api/files/' && data.url ~ 'token='", "-n", "100", "-o", "json")
		if strings.Contains(logged, "/api/files/") {
			return logged
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("the file download never appeared in the logs:\n%s", mustRun(t, "-p", profile, "logs", "list", "-n", "30"))
	return ""
}
