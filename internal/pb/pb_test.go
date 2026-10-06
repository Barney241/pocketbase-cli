package pb

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Barney241/pocketbase-cli/internal/config"
	"github.com/Barney241/pocketbase-cli/internal/guard"
)

func tokenExpiringIn(lifetime time.Duration) string {
	claims, _ := json.Marshal(map[string]any{"exp": time.Now().Add(lifetime).Unix()})
	return "h." + base64.RawURLEncoding.EncodeToString(claims) + ".s"
}

type fakePocketBase struct {
	logins   int
	writes   int
	rejectOn string
}

func (f *fakePocketBase) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	switch {
	case strings.HasSuffix(request.URL.Path, "/auth-with-password"):
		f.logins++
		fmt.Fprintf(writer, `{"token":%q}`, tokenExpiringIn(time.Hour)+fmt.Sprint(f.logins))
	case request.Header.Get("Authorization") == f.rejectOn:
		writer.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(writer, `{"status":401,"message":"The request requires valid record authorization token.","data":{}}`)
	case request.Method != http.MethodGet:
		f.writes++
		writer.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(writer, `{"status":400,"message":"Failed to create record.","data":{"title":{"code":"validation_required","message":"Cannot be blank."},"meta":{"size":{"code":"validation_max","message":"Too big."}}}}`)
	default:
		fmt.Fprint(writer, `{"ok":true}`)
	}
}

func newTestClient(t *testing.T, server *httptest.Server, readOnlySource string, dryRun bool) *Client {
	t.Helper()
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	client, err := New(Options{
		Profile:        &config.Profile{URL: server.URL, Identity: "admin@example.com", Password: "secret"},
		ReadOnlySource: readOnlySource,
		DryRun:         dryRun,
		Version:        "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestTheLoginTokenIsCachedAcrossClientsAndReplacedWhenRejected(t *testing.T) {
	fake := &fakePocketBase{}
	server := httptest.NewServer(fake)
	defer server.Close()
	cacheHome := t.TempDir()
	for range 3 {
		client := newTestClient(t, server, "", false)
		t.Setenv("XDG_CACHE_HOME", cacheHome)
		if err := client.JSON(context.Background(), http.MethodGet, "/api/health", nil, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	if fake.logins != 1 {
		t.Fatalf("three commands logged in %d times, want 1", fake.logins)
	}
	client := newTestClient(t, server, "", false)
	t.Setenv("XDG_CACHE_HOME", cacheHome)
	cached, err := client.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	fake.rejectOn = cached
	if err := client.JSON(context.Background(), http.MethodGet, "/api/health", nil, nil, nil); err != nil {
		t.Fatalf("a rejected cached token was not replaced: %v", err)
	}
	if fake.logins != 2 {
		t.Fatalf("logins = %d, want 2 after the cached token was rejected", fake.logins)
	}
}

func TestValidationErrorsAreFlattenedIntoOneLine(t *testing.T) {
	server := httptest.NewServer(&fakePocketBase{})
	defer server.Close()
	client := newTestClient(t, server, "", false)
	err := client.JSON(context.Background(), http.MethodPost, "/api/collections/posts/records", nil, map[string]any{}, nil)
	var apiError *APIError
	if !errors.As(err, &apiError) {
		t.Fatalf("err = %v", err)
	}
	want := "400 POST /api/collections/posts/records: Failed to create record. [meta.size: Too big.; title: Cannot be blank.]"
	if apiError.Error() != want {
		t.Fatalf("error = %q\nwant    %q", apiError.Error(), want)
	}
}

func TestReadOnlyAndDryRunStopAWriteBeforeItIsSent(t *testing.T) {
	fake := &fakePocketBase{}
	server := httptest.NewServer(fake)
	defer server.Close()

	readOnly := newTestClient(t, server, "the test", false)
	err := readOnly.JSON(context.Background(), http.MethodPost, "/api/collections/posts/records", nil, map[string]any{"title": "x"}, nil)
	var blocked *guard.BlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("a read-only client returned %v", err)
	}

	dryRun := newTestClient(t, server, "", true)
	err = dryRun.JSON(context.Background(), http.MethodPost, "/api/collections/posts/records", nil, map[string]any{"title": "x"}, nil)
	var preview *DryRunError
	if !errors.As(err, &preview) || string(preview.Body) != `{"title":"x"}` {
		t.Fatalf("a dry run returned %v", err)
	}
	if fake.writes != 0 || fake.logins != 0 {
		t.Fatalf("the server saw %d writes and %d logins", fake.writes, fake.logins)
	}
}

func TestTokensWithoutAReadableExpiryCountAsExpired(t *testing.T) {
	if !TokenExpiry(tokenExpiringIn(time.Hour)).After(time.Now()) {
		t.Fatal("a fresh token reads as expired")
	}
	for _, token := range []string{"", "not-a-jwt", "a.b.c", "a.!!!.c"} {
		if !TokenExpiry(token).IsZero() {
			t.Errorf("token %q has an expiry", token)
		}
	}
}
