package gateway

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestTheGatewayPermitsReadsAndPlainRealtimeOnly(t *testing.T) {
	cases := []struct {
		method string
		path   string
		want   bool
	}{
		{"GET", "/api/collections/posts/records", true},
		{"HEAD", "/api/files/posts/abc/a.pdf", true},
		{"GET", "/api/logs", true},
		{"GET", "/api/backups", true},
		{"POST", "/api/realtime", true},
		{"GET", "/api/backups/a.zip", false},
		{"GET", "/api/backups/", false},
		{"GET", "/api//backups/a.zip", false},
		{"GET", "/api/../api/backups/a.zip", false},
		{"GET", "/_/", false},
		{"GET", "/custom", false},
		{"GET", "/", false},
		{"POST", "/api/collections/meta/dry-run-view", false},
		{"POST", "/api/files/token", false},
		{"POST", "/api/collections/_superusers/auth-with-password", false},
		{"POST", "/api/collections/_superusers/auth-refresh", false},
		{"POST", "/api/collections/users/impersonate/abc", false},
		{"POST", "/api/sql", false},
		{"POST", "/api/batch", false},
		{"POST", "/api/realtime/", false},
		{"PATCH", "/api/settings", false},
		{"PUT", "/api/collections/import", false},
		{"DELETE", "/api/logs", false},
		{"OPTIONS", "/api/health", false},
		{"CONNECT", "/api/health", false},
	}
	for _, testCase := range cases {
		if got := Permits(testCase.method, testCase.path); got != testCase.want {
			t.Errorf("Permits(%s %s) = %v, want %v", testCase.method, testCase.path, got, testCase.want)
		}
	}
}

func TestRealtimeTopicsWithOptionsAreNotPlain(t *testing.T) {
	for _, body := range []string{
		`{"clientId":"c","subscriptions":["posts/*"]}`,
		`{"clientId":"c","subscriptions":["posts","posts/abc123"]}`,
		`{"clientId":"c","subscriptions":[]}`,
	} {
		if !PlainSubscriptions([]byte(body)) {
			t.Errorf("%s was refused", body)
		}
	}
	for _, body := range []string{
		`{"subscriptions":["_superusers/*?options=%7B%22query%22%3A%7B%22filter%22%3A%22tokenKey~'a'%22%7D%7D"]}`,
		`{"subscriptions":["posts/*?x"]}`,
		`{"subscriptions":["posts/*?options=1"]}`,
		`{"subscriptions":["posts/a/b"]}`,
		`{"subscriptions":"posts/*"}`,
		`not json`,
	} {
		if PlainSubscriptions([]byte(body)) {
			t.Errorf("%s was accepted", body)
		}
	}
}

func TestSecretFieldNamesAreSpottedInAnyCase(t *testing.T) {
	for _, text := range []string{"tokenKey ~ 'a'", "author.TOKENKEY != ''", "-password", "@collection._superusers.password ?~ '$2a'"} {
		if !NamesSecretField(text) {
			t.Errorf("%q passed", text)
		}
	}
	for _, text := range []string{"title ~ 'token'", "-created", "status = 'open'"} {
		if NamesSecretField(text) {
			t.Errorf("%q was refused", text)
		}
	}
}

func signedLikeToken(lifetime time.Duration) string {
	claims, _ := json.Marshal(map[string]any{"exp": time.Now().Add(lifetime).Unix(), "id": "abc"})
	return "header." + base64.RawURLEncoding.EncodeToString(claims) + ".signature"
}

type upstreamRecorder struct {
	logins        int
	authorization []string
	proxyTokens   []string
	fileTokens    []string
	queries       []url.Values
}

func (u *upstreamRecorder) handler() http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Access-Control-Allow-Origin", "*")
		writer.Header().Set("Set-Cookie", "session=1")
		u.proxyTokens = append(u.proxyTokens, request.Header.Get("X-Proxy-Token"))
		switch {
		case strings.HasSuffix(request.URL.Path, "/auth-with-password"):
			u.logins++
			fmt.Fprintf(writer, `{"token":%q}`, signedLikeToken(time.Hour))
		case request.URL.Path == "/api/files/token":
			fmt.Fprint(writer, `{"token":"upstream-file-token"}`)
		default:
			u.authorization = append(u.authorization, request.Header.Get("Authorization"))
			u.fileTokens = append(u.fileTokens, request.URL.Query().Get("token"))
			u.queries = append(u.queries, request.URL.Query())
			body, _ := io.ReadAll(request.Body)
			fmt.Fprintf(writer, `{"path":%q,"cookie":%q,"body":%q}`, request.URL.Path, request.Header.Get("Cookie"), body)
		}
	})
}

func startGateway(t *testing.T, accessKey string) (*httptest.Server, *upstreamRecorder) {
	t.Helper()
	recorder := &upstreamRecorder{}
	upstreamServer := httptest.NewServer(recorder.handler())
	t.Cleanup(upstreamServer.Close)
	upstream, _ := url.Parse(upstreamServer.URL)
	gatewayServer := httptest.NewUnstartedServer(nil)
	gatewayServer.Config.Handler = New(Config{
		Upstream:        upstream,
		Collection:      "_superusers",
		Identity:        "admin@example.com",
		Password:        "secret",
		AccessKey:       accessKey,
		AllowedHosts:    []string{gatewayServer.Listener.Addr().String()},
		UpstreamHeaders: map[string]string{"X-Proxy-Token": "proxy-token"},
		Log:             io.Discard,
	})
	gatewayServer.Start()
	t.Cleanup(gatewayServer.Close)
	return gatewayServer, recorder
}

func TestTheGatewayReplacesClientCredentialsWithItsOwn(t *testing.T) {
	gatewayServer, upstream := startGateway(t, "")
	for range 2 {
		request, _ := http.NewRequest(http.MethodGet, gatewayServer.URL+"/api/files/posts/abc/a.pdf?token=client-token&thumb=10x10", nil)
		request.Header.Set("Authorization", "client-auth")
		request.Header.Set("Cookie", "client=1")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != http.StatusOK || bytes.Contains(body, []byte("client=1")) {
			t.Fatalf("status %d body %s", response.StatusCode, body)
		}
		if response.Header.Get("Access-Control-Allow-Origin") != "" || response.Header.Get("Set-Cookie") != "" {
			t.Fatalf("upstream CORS or cookie headers passed through: %v", response.Header)
		}
	}
	if upstream.logins != 1 {
		t.Fatalf("the gateway logged in %d times for two requests, want 1", upstream.logins)
	}
	for index, authorization := range upstream.authorization {
		if authorization == "client-auth" || authorization == "" {
			t.Fatalf("request %d reached upstream with authorization %q", index, authorization)
		}
		if upstream.fileTokens[index] != "upstream-file-token" || upstream.queries[index].Get("thumb") != "10x10" {
			t.Fatalf("request %d reached upstream with query %v", index, upstream.queries[index])
		}
	}
}

func TestTheGatewayRefusesWritesSecretFiltersBrowsersAndStrangers(t *testing.T) {
	gatewayServer, upstream := startGateway(t, "open-sesame")
	attempts := map[string]func() *http.Request{
		"a write": func() *http.Request {
			return keyed(http.NewRequest(http.MethodPost, gatewayServer.URL+"/api/collections/posts/records", strings.NewReader(`{}`)))
		},
		"a secret filter": func() *http.Request {
			return keyed(http.NewRequest(http.MethodGet, gatewayServer.URL+"/api/collections/_superusers/records?filter="+url.QueryEscape("tokenKey ~ 'a'"), nil))
		},
		"a subscription with options": func() *http.Request {
			request := keyed(http.NewRequest(http.MethodPost, gatewayServer.URL+"/api/realtime", strings.NewReader(`{"subscriptions":["posts/*?options=x"]}`)))
			request.Header.Set("Content-Type", "application/json")
			return request
		},
		"a subscription smuggled into the query string": func() *http.Request {
			request := keyed(http.NewRequest(http.MethodPost, gatewayServer.URL+"/api/realtime?clientId=c&subscriptions="+url.QueryEscape("posts/*?options=x"), strings.NewReader(`{}`)))
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			return request
		},
		"a subscription sent as a form": func() *http.Request {
			request := keyed(http.NewRequest(http.MethodPost, gatewayServer.URL+"/api/realtime", strings.NewReader("clientId=c&subscriptions=posts")))
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			return request
		},
		"a missing access key": func() *http.Request {
			request, _ := http.NewRequest(http.MethodGet, gatewayServer.URL+"/api/health", nil)
			return request
		},
		"a browser origin": func() *http.Request {
			request := keyed(http.NewRequest(http.MethodGet, gatewayServer.URL+"/api/health", nil))
			request.Header.Set("Origin", "https://evil.example")
			return request
		},
		"a browser fetch": func() *http.Request {
			request := keyed(http.NewRequest(http.MethodGet, gatewayServer.URL+"/api/health", nil))
			request.Header.Set("Sec-Fetch-Site", "cross-site")
			return request
		},
		"a rebound host": func() *http.Request {
			request := keyed(http.NewRequest(http.MethodGet, gatewayServer.URL+"/api/health", nil))
			request.Host = "evil.example"
			return request
		},
	}
	for name, build := range attempts {
		response, err := http.DefaultClient.Do(build())
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusForbidden {
			t.Errorf("%s returned %d, want 403", name, response.StatusCode)
		}
	}
	if len(upstream.authorization) != 0 {
		t.Fatalf("refused requests reached upstream: %d", len(upstream.authorization))
	}
	plainSubscription := keyed(http.NewRequest(http.MethodPost, gatewayServer.URL+"/api/realtime", strings.NewReader(`{"clientId":"c","subscriptions":["posts/*"]}`)))
	plainSubscription.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(plainSubscription)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), `posts/*`) {
		t.Fatalf("a plain subscription was not forwarded intact: %d %s", response.StatusCode, body)
	}
}

func keyed(request *http.Request, _ error) *http.Request {
	request.Header.Set("X-Pbctl-Gateway-Key", "open-sesame")
	return request
}

func TestTheGatewaySendsItsUpstreamHeadersOnLoginsAndReadsWhateverTheClientSends(t *testing.T) {
	gatewayServer, upstream := startGateway(t, "")
	request, _ := http.NewRequest(http.MethodGet, gatewayServer.URL+"/api/collections/posts/records", nil)
	request.Header.Set("X-Proxy-Token", "from-the-client")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK || len(upstream.proxyTokens) != 2 {
		t.Fatalf("want a login and a read upstream, got status %d and %d upstream calls", response.StatusCode, len(upstream.proxyTokens))
	}
	for _, sent := range upstream.proxyTokens {
		if sent != "proxy-token" {
			t.Errorf("upstream received X-Proxy-Token %q, want the gateway's own", sent)
		}
	}
}
