package guard

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"
)

type Class int

const (
	Read Class = iota
	Write
)

const ExitCodeBlocked = 3

var readOnlyPostRoutes = []*regexp.Regexp{
	regexp.MustCompile(`^/api/collections/[^/]+/auth-with-password$`),
	regexp.MustCompile(`^/api/collections/[^/]+/auth-refresh$`),
	regexp.MustCompile(`^/api/collections/meta/dry-run-view$`),
	regexp.MustCompile(`^/api/files/token$`),
	regexp.MustCompile(`^/api/realtime$`),
}

var impersonateRoute = regexp.MustCompile(`^/api/collections/[^/]+/impersonate/[^/]+$`)

func Classify(method, path string) Class {
	switch strings.ToUpper(method) {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return Read
	case http.MethodPost:
		if isPlainPath(path) && matchesAny(readOnlyPostRoutes, path) {
			return Read
		}
	}
	return Write
}

func IsImpersonation(method, path string) bool {
	return strings.EqualFold(method, http.MethodPost) && isPlainPath(path) && impersonateRoute.MatchString(path)
}

func isPlainPath(path string) bool {
	for segment := range strings.SplitSeq(path, "/") {
		if segment == "." || segment == ".." {
			return false
		}
	}
	return !strings.ContainsAny(path, "\\%")
}

func matchesAny(routes []*regexp.Regexp, path string) bool {
	for _, route := range routes {
		if route.MatchString(path) {
			return true
		}
	}
	return false
}

type BlockedError struct {
	Method string
	Path   string
	Source string
}

func (e *BlockedError) Error() string {
	return fmt.Sprintf("read-only: %s %s was not sent (read-only is set by %s)", e.Method, e.Path, e.Source)
}

type Transport struct {
	Next               http.RoundTripper
	BasePath           string
	ReadOnlySource     string
	AllowImpersonation bool
}

func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.ReadOnlySource != "" {
		path := t.relativePath(req.URL.EscapedPath())
		if !t.permits(req.Method, path) {
			return nil, &BlockedError{Method: req.Method, Path: path, Source: t.ReadOnlySource}
		}
	}
	return t.Next.RoundTrip(req)
}

func (t *Transport) permits(method, path string) bool {
	if Classify(method, path) == Read {
		return true
	}
	return t.AllowImpersonation && IsImpersonation(method, path)
}

func (t *Transport) relativePath(path string) string {
	base := strings.TrimRight(t.BasePath, "/")
	if base != "" && strings.HasPrefix(path, base+"/") {
		return path[len(base):]
	}
	return path
}
