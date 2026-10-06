package guard

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOnlySafeMethodsAndAShortListOfPostsCountAsReads(t *testing.T) {
	cases := []struct {
		method string
		path   string
		want   Class
	}{
		{"GET", "/api/collections/posts/records", Read},
		{"get", "/api/logs", Read},
		{"HEAD", "/api/files/posts/abc/file.pdf", Read},
		{"OPTIONS", "/api/anything", Read},
		{"POST", "/api/collections/_superusers/auth-with-password", Read},
		{"POST", "/api/collections/users/auth-refresh", Read},
		{"POST", "/api/collections/meta/dry-run-view", Read},
		{"POST", "/api/files/token", Read},
		{"POST", "/api/realtime", Read},
		{"POST", "/api/collections/posts/records", Write},
		{"POST", "/api/batch", Write},
		{"POST", "/api/sql", Write},
		{"POST", "/api/backups", Write},
		{"POST", "/api/backups/a.zip/restore", Write},
		{"POST", "/api/crons/job", Write},
		{"POST", "/api/settings/test/email", Write},
		{"POST", "/api/collections/users/impersonate/abc", Write},
		{"POST", "/api/collections/users/request-password-reset", Write},
		{"POST", "/api/collections/users/auth-with-otp", Write},
		{"POST", "/api/collections/../files/token", Write},
		{"POST", "/api/collections/a/b/auth-refresh", Write},
		{"POST", "/api/files/token/", Write},
		{"POST", "/api/files/token%2F..%2Fx", Write},
		{"POST", "/api/realtime/extra", Write},
		{"POST", "/custom/route", Write},
		{"PATCH", "/api/collections/posts/records/abc", Write},
		{"PUT", "/api/collections/import", Write},
		{"DELETE", "/api/logs", Write},
		{"CONNECT", "/api/realtime", Write},
		{"TRACE", "/api/health", Write},
		{"MADEUP", "/api/health", Write},
	}
	for _, testCase := range cases {
		if got := Classify(testCase.method, testCase.path); got != testCase.want {
			t.Errorf("Classify(%s %s) = %v, want %v", testCase.method, testCase.path, got, testCase.want)
		}
	}
}

func TestAReadOnlyTransportNeverLetsAWriteReachTheServer(t *testing.T) {
	reached := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		reached = append(reached, request.Method+" "+request.URL.Path)
		if request.URL.Path == "/pb/redirect" {
			http.Redirect(writer, request, "/pb/api/collections/posts/records", http.StatusTemporaryRedirect)
		}
	}))
	defer server.Close()
	client := &http.Client{Transport: &Transport{Next: http.DefaultTransport, BasePath: "/pb", ReadOnlySource: "the test"}}

	for _, attempt := range []struct{ method, path string }{
		{"POST", "/pb/api/collections/posts/records"},
		{"DELETE", "/pb/api/collections/posts/records/abc"},
		{"POST", "/pb/api/collections/users/impersonate/abc"},
		{"POST", "/api/files/token/../../pb/api/sql"},
	} {
		request, _ := http.NewRequest(attempt.method, server.URL+attempt.path, nil)
		_, err := client.Do(request)
		var blocked *BlockedError
		if !errors.As(err, &blocked) {
			t.Errorf("%s %s was not blocked: %v", attempt.method, attempt.path, err)
		}
	}
	if len(reached) != 0 {
		t.Fatalf("blocked requests reached the server: %v", reached)
	}

	for _, attempt := range []struct{ method, path string }{
		{"GET", "/pb/api/collections/posts/records"},
		{"POST", "/pb/api/files/token"},
	} {
		request, _ := http.NewRequest(attempt.method, server.URL+attempt.path, nil)
		response, err := client.Do(request)
		if err != nil {
			t.Fatalf("%s %s was refused: %v", attempt.method, attempt.path, err)
		}
		response.Body.Close()
	}
	if len(reached) != 2 {
		t.Fatalf("reads did not reach the server: %v", reached)
	}
}

func TestImpersonationPassesOnlyThroughTheTransportThatAllowsIt(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()
	target := server.URL + "/api/collections/users/impersonate/abc"

	strict := &http.Client{Transport: &Transport{Next: http.DefaultTransport, ReadOnlySource: "the test"}}
	request, _ := http.NewRequest(http.MethodPost, target, nil)
	if _, err := strict.Do(request); err == nil {
		t.Fatal("the strict transport let an impersonation request through")
	}

	internal := &http.Client{Transport: &Transport{Next: http.DefaultTransport, ReadOnlySource: "the test", AllowImpersonation: true}}
	request, _ = http.NewRequest(http.MethodPost, target, nil)
	response, err := internal.Do(request)
	if err != nil {
		t.Fatalf("the internal transport refused an impersonation request: %v", err)
	}
	response.Body.Close()
	request, _ = http.NewRequest(http.MethodPost, server.URL+"/api/collections/users/records", nil)
	if _, err := internal.Do(request); err == nil {
		t.Fatal("the internal transport let a record write through")
	}
}

func TestAWritableTransportSendsEverything(t *testing.T) {
	reached := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached++ }))
	defer server.Close()
	client := &http.Client{Transport: &Transport{Next: http.DefaultTransport}}
	request, _ := http.NewRequest(http.MethodDelete, server.URL+"/api/collections/posts/records/abc", nil)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if reached != 1 {
		t.Fatal("a writable transport dropped a request")
	}
}
