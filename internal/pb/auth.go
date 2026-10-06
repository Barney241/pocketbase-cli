package pb

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Barney241/pocketbase-cli/internal/config"
)

type authResponse struct {
	Token string `json:"token"`
	MfaID string `json:"mfaId"`
}

func (c *Client) Token(ctx context.Context) (string, error) {
	if c.options.Impersonate == "" {
		return c.loginToken(ctx)
	}
	if c.actingToken != "" {
		return c.actingToken, nil
	}
	cacheKey := "as:" + c.options.Impersonate
	if cached := c.cachedToken(cacheKey); cached != "" {
		c.actingToken = cached
		return cached, nil
	}
	token, err := c.impersonate(ctx, c.options.Impersonate)
	if err != nil {
		return "", err
	}
	c.actingToken = token
	c.storeToken(cacheKey, token)
	return token, nil
}

func (c *Client) loginToken(ctx context.Context) (string, error) {
	if c.baseToken != "" {
		return c.baseToken, nil
	}
	profile := c.options.Profile
	if token := profile.ResolvedToken(); token != "" {
		c.baseToken = token
		return token, nil
	}
	if cached := c.cachedToken("login"); cached != "" {
		c.baseToken = cached
		return cached, nil
	}
	token, err := c.Login(ctx, profile.Collection(), profile.Identity, profile.ResolvedPassword())
	if err != nil {
		return "", err
	}
	c.baseToken = token
	c.storeToken("login", token)
	return token, nil
}

func (c *Client) Login(ctx context.Context, collection, identity, password string) (string, error) {
	credentials := map[string]string{"identity": identity, "password": password}
	encoded, err := json.Marshal(credentials)
	if err != nil {
		return "", err
	}
	path := "/api/collections/" + url.PathEscape(collection) + "/auth-with-password"
	response, err := c.Do(ctx, Request{Method: http.MethodPost, Path: path, Body: encoded, ContentType: contentTypeJSON, Anonymous: true})
	if err != nil {
		return "", explainLoginFailure(err, collection, identity)
	}
	defer response.Body.Close()
	var payload authResponse
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return "", fmt.Errorf("decode the login response: %w", err)
	}
	if payload.Token == "" {
		return "", fmt.Errorf("login as %s returned no token", identity)
	}
	return payload.Token, nil
}

func explainLoginFailure(err error, collection, identity string) error {
	var apiError *APIError
	if !errors.As(err, &apiError) {
		return err
	}
	if _, needsSecondFactor := apiError.Data["mfaId"]; needsSecondFactor {
		return &AuthError{Reason: fmt.Sprintf("%s in %s has MFA enabled; store a token in the profile instead of a password", identity, collection)}
	}
	if apiError.Status == http.StatusNotFound && collection == config.SuperusersCollection {
		return &AuthError{Reason: "the server has no _superusers collection; pbctl needs PocketBase 0.23 or newer"}
	}
	return &AuthError{Reason: fmt.Sprintf("login as %s in %s failed: %s", identity, collection, apiError.Message)}
}

func (c *Client) impersonate(ctx context.Context, target string) (string, error) {
	collection, id, found := strings.Cut(target, "/")
	if !found || collection == "" || id == "" {
		return "", fmt.Errorf("--as expects <collection>/<record id>, got %q", target)
	}
	loginToken, err := c.loginToken(ctx)
	if err != nil {
		return "", err
	}
	body := fmt.Sprintf(`{"duration":%d}`, impersonationTTL)
	path := "/api/collections/" + url.PathEscape(collection) + "/impersonate/" + url.PathEscape(id)
	httpRequest, err := c.newHTTPRequest(ctx, Request{Method: http.MethodPost, Path: path, Body: []byte(body), ContentType: contentTypeJSON})
	if err != nil {
		return "", err
	}
	httpRequest.Header.Set("Authorization", loginToken)
	response, err := c.credentials.Do(httpRequest)
	if err != nil {
		return "", describeTransportError(err, c.base.String())
	}
	defer response.Body.Close()
	if response.StatusCode >= 400 {
		return "", &AuthError{Reason: fmt.Sprintf("cannot act as %s: %s", target, apiErrorFrom(http.MethodPost, path, response).Message)}
	}
	var payload authResponse
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return "", fmt.Errorf("decode the impersonation response: %w", err)
	}
	return payload.Token, nil
}

func (c *Client) forgetTokens() {
	c.baseToken = ""
	c.actingToken = ""
	for _, key := range []string{"login", "as:" + c.options.Impersonate} {
		if location := c.tokenCachePath(key); location != "" {
			os.Remove(location)
		}
	}
}

func (c *Client) tokenCachePath(key string) string {
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	profile := c.options.Profile
	digest := sha256.Sum256([]byte(strings.Join([]string{c.base.String(), profile.Collection(), profile.Identity, key}, "\x00")))
	return filepath.Join(cacheDir, "pbctl", "tokens", hex.EncodeToString(digest[:12]))
}

func (c *Client) cachedToken(key string) string {
	location := c.tokenCachePath(key)
	if location == "" {
		return ""
	}
	raw, err := os.ReadFile(location)
	if err != nil {
		return ""
	}
	token := strings.TrimSpace(string(raw))
	if TokenExpiry(token).Before(time.Now().Add(tokenExpiryLeeway)) {
		return ""
	}
	return token
}

func (c *Client) storeToken(key, token string) {
	location := c.tokenCachePath(key)
	if location == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(location), 0o700); err != nil {
		return
	}
	os.WriteFile(location, []byte(token), 0o600)
}

func TokenExpiry(token string) time.Time {
	claims := TokenClaims(token)
	expiry, ok := claims["exp"].(float64)
	if !ok {
		return time.Time{}
	}
	return time.Unix(int64(expiry), 0)
}

func TokenClaims(token string) map[string]any {
	claims := map[string]any{}
	segments := strings.Split(token, ".")
	if len(segments) != 3 {
		return claims
	}
	payload, err := base64.RawURLEncoding.DecodeString(segments[1])
	if err != nil {
		return claims
	}
	json.Unmarshal(payload, &claims)
	return claims
}

type AuthError struct {
	Reason string
}

func (e *AuthError) Error() string {
	return e.Reason
}
