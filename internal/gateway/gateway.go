package gateway

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Barney241/pocketbase-cli/internal/pb"
)

const (
	largestRequestBody  = 1 << 20
	actingTokenSeconds  = 900
	renewBeforeExpiry   = time.Minute
	fileTokenLifetime   = time.Minute
	upstreamCallTimeout = 30 * time.Second
	enforcedByGateway   = "gateway"
)

type Config struct {
	Upstream        *url.URL
	Collection      string
	Identity        string
	Password        string
	StaticToken     string
	AccessKey       string
	AllowedHosts    []string
	UpstreamHeaders map[string]string
	Log             io.Writer
}

type Gateway struct {
	config     Config
	proxy      *httputil.ReverseProxy
	upstream   *http.Client
	mutex      sync.Mutex
	loginToken string
	acting     map[string]string
	fileTokens map[string]fileToken
}

type fileToken struct {
	value   string
	expires time.Time
}

var (
	secretFields = []string{"tokenkey", "password"}
	plainTopic   = regexp.MustCompile(`^[A-Za-z0-9_]+(/[A-Za-z0-9_*]+)?$`)
)

const (
	realtimePath = "/api/realtime"
	logsPath     = "/api/logs"
	redacted     = "REDACTED"
)

var (
	tokenParameter = regexp.MustCompile(`token=[^&"\\\s]+`)
	signedToken    = regexp.MustCompile(`eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+`)
)

func redactTokens(response *http.Response) error {
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		return err
	}
	body = signedToken.ReplaceAll(body, []byte(redacted))
	body = tokenParameter.ReplaceAll(body, []byte("token="+redacted))
	response.Body = io.NopCloser(bytes.NewReader(body))
	response.ContentLength = int64(len(body))
	response.Header.Set("Content-Length", strconv.Itoa(len(body)))
	return nil
}

type contextKey string

const (
	authTokenKey contextKey = "authToken"
	fileTokenKey contextKey = "fileToken"
)

func New(config Config) *Gateway {
	gateway := &Gateway{
		config:     config,
		upstream:   &http.Client{Timeout: upstreamCallTimeout},
		acting:     map[string]string{},
		fileTokens: map[string]fileToken{},
	}
	gateway.proxy = &httputil.ReverseProxy{
		Rewrite:        gateway.rewrite,
		ModifyResponse: gateway.sanitizeResponse,
		FlushInterval:  -1,
		ErrorHandler: func(writer http.ResponseWriter, _ *http.Request, err error) {
			refuse(writer, http.StatusBadGateway, "the upstream PocketBase did not answer: "+err.Error())
		},
	}
	return gateway
}

func (g *Gateway) CheckCredentials(ctx context.Context) error {
	_, err := g.tokenFor(ctx, "")
	return err
}

func (g *Gateway) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	recorder := &statusRecorder{ResponseWriter: writer, status: http.StatusOK}
	g.serve(recorder, request)
	fmt.Fprintf(g.config.Log, "%s %s %s -> %d\n", time.Now().Format("15:04:05"), request.Method, request.URL.RequestURI(), recorder.status)
}

func (g *Gateway) serve(writer http.ResponseWriter, request *http.Request) {
	if reason := g.refusalReason(request); reason != "" {
		refuse(writer, http.StatusForbidden, reason)
		return
	}
	if request.URL.Path == pb.GuardStatusPath {
		writeJSON(writer, http.StatusOK, pb.GuardStatus{ReadOnly: true, EnforcedBy: enforcedByGateway})
		return
	}
	ctx := request.Context()
	authToken, err := g.tokenFor(ctx, request.Header.Get(pb.ImpersonateHeader))
	if err != nil {
		refuse(writer, http.StatusBadGateway, err.Error())
		return
	}
	ctx = context.WithValue(ctx, authTokenKey, authToken)
	if strings.HasPrefix(request.URL.Path, "/api/files/") {
		token, err := g.fileTokenFor(ctx, authToken)
		if err != nil {
			refuse(writer, http.StatusBadGateway, err.Error())
			return
		}
		ctx = context.WithValue(ctx, fileTokenKey, token)
	}
	g.proxy.ServeHTTP(writer, request.WithContext(ctx))
}

func (g *Gateway) refusalReason(request *http.Request) string {
	switch {
	case !g.hostAllowed(request.Host):
		return "read-only gateway: unexpected Host header " + request.Host
	case request.Header.Get("Origin") != "" || request.Header.Get("Sec-Fetch-Site") != "":
		return "read-only gateway: requests from web pages are refused"
	case g.config.AccessKey != "" && subtle.ConstantTimeCompare([]byte(request.Header.Get(pb.GatewayKeyHeader)), []byte(g.config.AccessKey)) != 1:
		return "read-only gateway: missing or wrong access key"
	case !Permits(request.Method, request.URL.Path):
		return fmt.Sprintf("read-only gateway: %s %s is not allowed", request.Method, request.URL.Path)
	case queryNamesSecretField(request.URL.Query()):
		return "read-only gateway: filters and sorts that mention password or tokenKey are refused, because they can leak login secrets"
	case request.Method == http.MethodPost:
		return g.screenSubscription(request)
	}
	return ""
}

func (g *Gateway) hostAllowed(host string) bool {
	for _, allowed := range g.config.AllowedHosts {
		if strings.EqualFold(allowed, host) {
			return true
		}
	}
	return false
}

func Permits(method, requestPath string) bool {
	if requestPath != path.Clean(requestPath) || !strings.HasPrefix(requestPath, "/api/") {
		return false
	}
	switch method {
	case http.MethodGet, http.MethodHead:
		return !strings.HasPrefix(requestPath, "/api/backups/")
	case http.MethodPost:
		return requestPath == realtimePath
	}
	return false
}

func NamesSecretField(text string) bool {
	lowered := strings.ToLower(text)
	for _, field := range secretFields {
		if strings.Contains(lowered, field) {
			return true
		}
	}
	return false
}

func queryNamesSecretField(query url.Values) bool {
	for _, values := range query {
		for _, value := range values {
			if NamesSecretField(value) {
				return true
			}
		}
	}
	return false
}

func PlainSubscriptions(body []byte) bool {
	var subscription struct {
		Subscriptions []string `json:"subscriptions"`
	}
	if err := json.Unmarshal(body, &subscription); err != nil {
		return false
	}
	for _, topic := range subscription.Subscriptions {
		if !plainTopic.MatchString(topic) {
			return false
		}
	}
	return true
}

func (g *Gateway) screenSubscription(request *http.Request) string {
	isJSON := strings.HasPrefix(strings.ToLower(request.Header.Get("Content-Type")), "application/json")
	if request.URL.RawQuery != "" || !isJSON {
		return "read-only gateway: a realtime subscription must be a JSON body without a query string"
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, largestRequestBody))
	if err != nil || !PlainSubscriptions(body) {
		return "read-only gateway: realtime topics must be <collection> or <collection>/<id>, without options"
	}
	request.Body = io.NopCloser(bytes.NewReader(body))
	request.ContentLength = int64(len(body))
	return ""
}

func (g *Gateway) rewrite(proxied *httputil.ProxyRequest) {
	proxied.SetURL(g.config.Upstream)
	proxied.Out.Host = g.config.Upstream.Host
	for header := range proxied.Out.Header {
		if isClientCredentialHeader(header) {
			proxied.Out.Header.Del(header)
		}
	}
	proxied.Out.Header.Del("Accept-Encoding")
	for name, value := range g.config.UpstreamHeaders {
		proxied.Out.Header.Set(name, value)
	}
	query := proxied.Out.URL.Query()
	query.Del("token")
	if token, _ := proxied.In.Context().Value(fileTokenKey).(string); token != "" {
		query.Set("token", token)
	}
	proxied.Out.URL.RawQuery = query.Encode()
	if token, _ := proxied.In.Context().Value(authTokenKey).(string); token != "" {
		proxied.Out.Header.Set("Authorization", token)
	}
}

func isClientCredentialHeader(header string) bool {
	lowered := strings.ToLower(header)
	return lowered == "authorization" || lowered == "cookie" || strings.HasPrefix(lowered, "x-pbctl-") || strings.HasPrefix(lowered, "x-forwarded-")
}

func (g *Gateway) sanitizeResponse(response *http.Response) error {
	for header := range response.Header {
		lowered := strings.ToLower(header)
		if lowered == "set-cookie" || strings.HasPrefix(lowered, "access-control-") {
			response.Header.Del(header)
		}
	}
	response.Header.Set("X-Pbctl-Gateway", "read-only")
	if strings.HasPrefix(response.Request.URL.Path, strings.TrimRight(g.config.Upstream.Path, "/")+logsPath) {
		if err := redactTokens(response); err != nil {
			return err
		}
	}
	if response.StatusCode == http.StatusUnauthorized {
		g.forgetTokens()
	}
	return nil
}

func (g *Gateway) forgetTokens() {
	g.mutex.Lock()
	defer g.mutex.Unlock()
	g.loginToken = ""
	g.acting = map[string]string{}
	g.fileTokens = map[string]fileToken{}
}

func (g *Gateway) tokenFor(ctx context.Context, actAs string) (string, error) {
	g.mutex.Lock()
	defer g.mutex.Unlock()
	login, err := g.currentLoginToken(ctx)
	if err != nil || actAs == "" {
		return login, err
	}
	if cached := g.acting[actAs]; cached != "" && stillValid(cached) {
		return cached, nil
	}
	collection, id, found := strings.Cut(actAs, "/")
	if !found || collection == "" || id == "" {
		return "", fmt.Errorf("%s expects <collection>/<record id>", pb.ImpersonateHeader)
	}
	endpoint := "/api/collections/" + url.PathEscape(collection) + "/impersonate/" + url.PathEscape(id)
	token, err := g.requestToken(ctx, endpoint, login, map[string]any{"duration": actingTokenSeconds})
	if err != nil {
		return "", fmt.Errorf("cannot act as %s: %w", actAs, err)
	}
	g.acting[actAs] = token
	return token, nil
}

func (g *Gateway) currentLoginToken(ctx context.Context) (string, error) {
	if g.config.StaticToken != "" {
		return g.config.StaticToken, nil
	}
	if g.config.Identity == "" {
		return "", nil
	}
	if g.loginToken != "" && stillValid(g.loginToken) {
		return g.loginToken, nil
	}
	endpoint := "/api/collections/" + url.PathEscape(g.config.Collection) + "/auth-with-password"
	token, err := g.requestToken(ctx, endpoint, "", map[string]any{"identity": g.config.Identity, "password": g.config.Password})
	if err != nil {
		return "", fmt.Errorf("upstream login as %s failed: %w", g.config.Identity, err)
	}
	g.loginToken = token
	return token, nil
}

func stillValid(token string) bool {
	return pb.TokenExpiry(token).After(time.Now().Add(renewBeforeExpiry))
}

func (g *Gateway) fileTokenFor(ctx context.Context, authToken string) (string, error) {
	if authToken == "" {
		return "", nil
	}
	g.mutex.Lock()
	defer g.mutex.Unlock()
	if cached, found := g.fileTokens[authToken]; found && cached.expires.After(time.Now()) {
		return cached.value, nil
	}
	token, err := g.requestToken(ctx, "/api/files/token", authToken, nil)
	if err != nil {
		return "", fmt.Errorf("cannot get a file token: %w", err)
	}
	g.fileTokens[authToken] = fileToken{value: token, expires: time.Now().Add(fileTokenLifetime)}
	return token, nil
}

func (g *Gateway) requestToken(ctx context.Context, endpoint, authorization string, body map[string]any) (string, error) {
	encoded, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	target := *g.config.Upstream
	target.Path = strings.TrimRight(target.Path, "/") + endpoint
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), bytes.NewReader(encoded))
	if err != nil {
		return "", err
	}
	request.Header.Set("Content-Type", "application/json")
	for name, value := range g.config.UpstreamHeaders {
		request.Header.Set(name, value)
	}
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	response, err := g.upstream.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	var payload struct {
		Token   string `json:"token"`
		Message string `json:"message"`
	}
	json.NewDecoder(io.LimitReader(response.Body, largestRequestBody)).Decode(&payload)
	if response.StatusCode >= 400 || payload.Token == "" {
		return "", fmt.Errorf("%d %s", response.StatusCode, payload.Message)
	}
	return payload.Token, nil
}

func refuse(writer http.ResponseWriter, status int, message string) {
	writeJSON(writer, status, map[string]any{"status": status, "message": message, "data": map[string]any{}})
}

func writeJSON(writer http.ResponseWriter, status int, payload any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	json.NewEncoder(writer).Encode(payload)
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}
