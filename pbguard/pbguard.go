package pbguard

import (
	"net/http"
	"net/url"
	pathpkg "path"
	"regexp"
	"slices"
	"strings"

	"github.com/pocketbase/pocketbase/core"
)

const StatusPath = "/api/pbctl/guard"

var (
	plainTopic       = regexp.MustCompile(`^[A-Za-z0-9_]+(/[A-Za-z0-9_*]+)?$`)
	authRefreshRoute = regexp.MustCompile(`^/api/collections/[^/]+/auth-refresh$`)
	secretFields     = []string{"tokenkey", "password"}
)

type guard struct {
	readOnlyEmails []string
}

func Bind(se *core.ServeEvent, readOnlySuperuserEmails ...string) {
	g := &guard{}
	for _, email := range readOnlySuperuserEmails {
		g.readOnlyEmails = append(g.readOnlyEmails, strings.ToLower(strings.TrimSpace(email)))
	}
	se.Router.GET(StatusPath, func(e *core.RequestEvent) error {
		return e.JSON(http.StatusOK, map[string]any{"readOnly": g.isReadOnly(e.Auth), "enforcedBy": "server hook"})
	})
	se.Router.BindFunc(g.enforce)
}

func (g *guard) isReadOnly(record *core.Record) bool {
	return record != nil && record.IsSuperuser() && slices.Contains(g.readOnlyEmails, strings.ToLower(record.Email()))
}

func (g *guard) enforce(e *core.RequestEvent) error {
	defer keepFileTokenOutOfLogs(e.Request.URL)
	method, path := e.Request.Method, pathpkg.Clean(e.Request.URL.Path)
	isRead := method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions
	if strings.HasPrefix(path, "/api/backups/") && isRead {
		owner, _ := e.App.FindAuthRecordByToken(e.Request.URL.Query().Get("token"), core.TokenTypeFile)
		if g.isReadOnly(owner) {
			return e.ForbiddenError("Refused by pbctl guard: a read-only superuser cannot download backups.", nil)
		}
		return e.Next()
	}
	if !g.isReadOnly(e.Auth) {
		return e.Next()
	}
	switch {
	case isRead:
		if queryNamesSecretField(e.Request.URL.Query()) {
			return e.ForbiddenError("Refused by pbctl guard: a read-only superuser cannot filter or sort by password or tokenKey.", nil)
		}
		return e.Next()
	case method == http.MethodPost && path == "/api/realtime":
		return g.screenSubscription(e)
	case method == http.MethodPost && (path == "/api/files/token" || authRefreshRoute.MatchString(path)):
		return e.Next()
	}
	return e.ForbiddenError("Refused by pbctl guard: this superuser is read-only; "+method+" "+path+" was refused.", nil)
}

func (g *guard) screenSubscription(e *core.RequestEvent) error {
	form := struct {
		Subscriptions []string `form:"subscriptions" json:"subscriptions"`
	}{}
	if err := e.BindBody(&form); err != nil {
		return e.BadRequestError("Refused by pbctl guard: unreadable subscription.", err)
	}
	for _, topic := range form.Subscriptions {
		if !plainTopic.MatchString(topic) {
			return e.ForbiddenError("Refused by pbctl guard: a read-only superuser can only subscribe to <collection> or <collection>/<id>.", nil)
		}
	}
	return e.Next()
}

func queryNamesSecretField(query url.Values) bool {
	for _, values := range query {
		for _, value := range values {
			lowered := strings.ToLower(value)
			for _, field := range secretFields {
				if strings.Contains(lowered, field) {
					return true
				}
			}
		}
	}
	return false
}

func keepFileTokenOutOfLogs(requested *url.URL) {
	query := requested.Query()
	if !query.Has("token") {
		return
	}
	query.Set("token", "REDACTED")
	requested.RawQuery = query.Encode()
}
