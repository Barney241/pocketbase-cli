package pb

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Barney241/pocketbase-cli/internal/config"
	"github.com/Barney241/pocketbase-cli/internal/guard"
)

const (
	GatewayKeyHeader  = "X-Pbctl-Gateway-Key"
	ImpersonateHeader = "X-Pbctl-As"
	GuardStatusPath   = "/api/pbctl/guard"
	defaultTimeout    = 30 * time.Second
	maxErrorBodyBytes = 64 << 10
	impersonationTTL  = 900
	tokenExpiryLeeway = 60 * time.Second
	contentTypeJSON   = "application/json"
)

type Options struct {
	ProfileName    string
	Profile        *config.Profile
	ReadOnlySource string
	DryRun         bool
	Timeout        time.Duration
	Impersonate    string
	Version        string
}

type Request struct {
	Method      string
	Path        string
	Query       url.Values
	Body        []byte
	BodyStream  io.Reader
	ContentType string
	Anonymous   bool
	Streaming   bool
}

type Client struct {
	options     Options
	base        *url.URL
	guarded     *http.Client
	credentials *http.Client
	baseToken   string
	actingToken string
}

type GuardStatus struct {
	ReadOnly   bool   `json:"readOnly"`
	EnforcedBy string `json:"enforcedBy"`
}

func New(options Options) (*Client, error) {
	base, err := options.Profile.BaseURL()
	if err != nil {
		return nil, err
	}
	if options.Timeout <= 0 {
		options.Timeout = defaultTimeout
	}
	return &Client{
		options:     options,
		base:        base,
		guarded:     guardedHTTPClient(base, options.ReadOnlySource, false),
		credentials: guardedHTTPClient(base, options.ReadOnlySource, true),
	}, nil
}

func guardedHTTPClient(base *url.URL, readOnlySource string, allowImpersonation bool) *http.Client {
	return &http.Client{Transport: &guard.Transport{
		Next:               http.DefaultTransport,
		BasePath:           base.Path,
		ReadOnlySource:     readOnlySource,
		AllowImpersonation: allowImpersonation,
	}}
}

func (c *Client) BaseURL() string {
	return c.base.String()
}

func (c *Client) Host() string {
	return c.base.Hostname()
}

func (c *Client) URL(path string, query url.Values) string {
	target := *c.base
	target.Path = strings.TrimRight(c.base.Path, "/") + path
	target.RawPath = ""
	target.RawQuery = query.Encode()
	return target.String()
}

func (c *Client) JSON(ctx context.Context, method, path string, query url.Values, body, out any) error {
	request := Request{Method: method, Path: path, Query: query}
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode the request body: %w", err)
		}
		request.Body = encoded
		request.ContentType = contentTypeJSON
	}
	response, err := c.Do(ctx, request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if out == nil || response.StatusCode == http.StatusNoContent {
		_, err = io.Copy(io.Discard, response.Body)
		return err
	}
	decoder := json.NewDecoder(response.Body)
	decoder.UseNumber()
	if err := decoder.Decode(out); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("decode the response of %s %s: %w", method, path, err)
	}
	return nil
}

func (c *Client) Do(ctx context.Context, request Request) (*http.Response, error) {
	isWrite := guard.Classify(request.Method, request.Path) == guard.Write
	if isWrite && c.options.ReadOnlySource != "" {
		return nil, &guard.BlockedError{Method: request.Method, Path: request.Path, Source: c.options.ReadOnlySource}
	}
	if isWrite && c.options.DryRun {
		return nil, &DryRunError{Method: request.Method, URL: c.URL(request.Path, request.Query), Body: request.Body}
	}
	response, err := c.send(ctx, request)
	if err != nil {
		return nil, err
	}
	if response.StatusCode == http.StatusUnauthorized && c.canRetryWithFreshToken(request) {
		response.Body.Close()
		c.forgetTokens()
		response, err = c.send(ctx, request)
		if err != nil {
			return nil, err
		}
	}
	if response.StatusCode >= 400 {
		defer response.Body.Close()
		return nil, apiErrorFrom(request.Method, request.Path, response)
	}
	return response, nil
}

func (c *Client) canRetryWithFreshToken(request Request) bool {
	profile := c.options.Profile
	return !request.Anonymous && request.BodyStream == nil && profile.ResolvedToken() == "" && profile.HasCredentials()
}

func (c *Client) send(ctx context.Context, request Request) (*http.Response, error) {
	cancel := func() {}
	if !request.Streaming {
		ctx, cancel = context.WithTimeout(ctx, c.options.Timeout)
	}
	httpRequest, err := c.newHTTPRequest(ctx, request)
	if err != nil {
		cancel()
		return nil, err
	}
	if !request.Anonymous {
		if err := c.authorize(ctx, httpRequest); err != nil {
			cancel()
			return nil, err
		}
	}
	response, err := c.guarded.Do(httpRequest)
	if err != nil {
		cancel()
		return nil, describeTransportError(err, c.base.String())
	}
	response.Body = &cancelOnClose{ReadCloser: response.Body, cancel: cancel}
	return response, nil
}

func (c *Client) newHTTPRequest(ctx context.Context, request Request) (*http.Request, error) {
	var body io.Reader
	switch {
	case request.BodyStream != nil:
		body = request.BodyStream
	case request.Body != nil:
		body = bytes.NewReader(request.Body)
	}
	httpRequest, err := http.NewRequestWithContext(ctx, request.Method, c.URL(request.Path, request.Query), body)
	if err != nil {
		return nil, err
	}
	if request.ContentType != "" {
		httpRequest.Header.Set("Content-Type", request.ContentType)
	}
	httpRequest.Header.Set("User-Agent", "pbctl/"+c.options.Version)
	if key := c.options.Profile.GatewayKey; key != "" {
		httpRequest.Header.Set(GatewayKeyHeader, key)
	}
	return httpRequest, nil
}

func (c *Client) authorize(ctx context.Context, httpRequest *http.Request) error {
	if !c.options.Profile.HasCredentials() {
		if c.options.Impersonate != "" {
			httpRequest.Header.Set(ImpersonateHeader, c.options.Impersonate)
		}
		return nil
	}
	token, err := c.Token(ctx)
	if err != nil {
		return err
	}
	httpRequest.Header.Set("Authorization", token)
	return nil
}

func (c *Client) GuardStatus(ctx context.Context) (*GuardStatus, error) {
	status := &GuardStatus{}
	err := c.JSON(ctx, http.MethodGet, GuardStatusPath, nil, nil, status)
	var apiError *APIError
	if errors.As(err, &apiError) && apiError.Status == http.StatusNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return status, nil
}

func (c *Client) FileToken(ctx context.Context) (string, error) {
	var payload struct {
		Token string `json:"token"`
	}
	if err := c.JSON(ctx, http.MethodPost, "/api/files/token", nil, nil, &payload); err != nil {
		return "", err
	}
	return payload.Token, nil
}

type DryRunError struct {
	Method string
	URL    string
	Body   []byte
}

func (e *DryRunError) Error() string {
	return fmt.Sprintf("dry run: %s %s was not sent", e.Method, e.URL)
}

type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelOnClose) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}

func describeTransportError(err error, baseURL string) error {
	var blocked *guard.BlockedError
	if errors.As(err, &blocked) {
		return blocked
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%s did not answer in time (raise --timeout): %w", baseURL, err)
	}
	return fmt.Errorf("cannot reach %s: %w", baseURL, err)
}
