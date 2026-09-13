// Package github is ghpipe's single in-process GitHub transport. Every remote
// call - REST or GraphQL - goes through it, so headers, timeouts, error
// classification and the pagination completeness contract are implemented once
// instead of per command.
//
// Design rules this package encodes (docs/design.md 7.1, 7.2, 7.4):
//
//   - no retry: a failed request is a failed request; an unknown outcome must
//     be reconciled by re-reading remote evidence, never by replaying a write;
//   - no response caching and no response body on disk;
//   - fixed headers and Bearer authorization (the scheme App JWTs require);
//   - every error is typed and redacted, never a remote message.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ghpipe/ghpipe/internal/version"
)

// Defaults for the transport.
const (
	// DefaultBaseURL is the public GitHub REST/GraphQL entry point.
	DefaultBaseURL = "https://api.github.com"
	// DefaultAPIVersion is pinned so behaviour cannot shift under us.
	DefaultAPIVersion = "2022-11-28"
	// DefaultAccept is the REST media type; GraphQL overrides it per request.
	DefaultAccept = "application/vnd.github+json"
	// DefaultTokenScheme is the scheme App JWTs require.
	DefaultTokenScheme = "Bearer"
	// defaultRequestTimeout bounds one request, not the whole command.
	defaultRequestTimeout = 60 * time.Second
	// maxResponseBytes caps one response so a hostile or broken endpoint cannot
	// exhaust memory. Exceeding it is a contract failure: we were told to read
	// more than we are willing to trust.
	maxResponseBytes = 64 << 20
	// maxRedirects mirrors net/http's default ceiling.
	maxRedirects = 10
)

// Client performs authenticated calls against one GitHub API host. It holds no
// mutable per-call state, so one client can be reused for a whole command.
type Client struct {
	http       *http.Client
	base       *url.URL
	baseErr    error
	token      string
	scheme     string
	userAgent  string
	apiVersion string
	accept     string
	proxyHost  string
}

// Option customises a Client. Everything a test needs to replace - the HTTP
// client, the base URL, the token - is injectable.
type Option func(*Client)

// New returns a client with production defaults: 60s per request, fixed
// headers, no retries.
func New(opts ...Option) *Client {
	base, _ := url.Parse(DefaultBaseURL)
	c := &Client{
		http:       &http.Client{Timeout: defaultRequestTimeout},
		base:       base,
		scheme:     DefaultTokenScheme,
		userAgent:  version.Command + "/" + version.Version,
		apiVersion: DefaultAPIVersion,
		accept:     DefaultAccept,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(c)
		}
	}
	c.hardenRedirects()
	return c
}

// WithHTTPClient injects the transport; tests substitute httptest's client.
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) {
		if h != nil {
			c.http = h
		}
	}
}

// WithBaseURL points the client at another API host (GitHub Enterprise or a
// test server). An unparsable value surfaces as a precondition failure on the
// first call instead of a panic here.
func WithBaseURL(raw string) Option {
	return func(c *Client) {
		u, err := url.Parse(strings.TrimSpace(raw))
		if err != nil || u.Scheme == "" || u.Host == "" {
			c.base, c.baseErr = nil, errors.New("base URL must be an absolute http(s) URL")
			return
		}
		c.base, c.baseErr = u, nil
		if transport, ok := c.http.Transport.(*http.Transport); ok && transport != nil {
			c.proxyHost = proxyHostFor(transport, u)
		}
	}
}

// WithToken sets the credential. It never appears in errors or logs.
func WithToken(token string) Option {
	return func(c *Client) { c.token = strings.TrimSpace(token) }
}

// WithTokenScheme overrides the Authorization scheme.
func WithTokenScheme(scheme string) Option {
	return func(c *Client) {
		if strings.TrimSpace(scheme) != "" {
			c.scheme = strings.TrimSpace(scheme)
		}
	}
}

// WithUserAgent overrides the User-Agent header.
func WithUserAgent(ua string) Option {
	return func(c *Client) {
		if strings.TrimSpace(ua) != "" {
			c.userAgent = strings.TrimSpace(ua)
		}
	}
}

// WithAPIVersion overrides X-GitHub-Api-Version.
func WithAPIVersion(v string) Option {
	return func(c *Client) {
		if strings.TrimSpace(v) != "" {
			c.apiVersion = strings.TrimSpace(v)
		}
	}
}

// WithAccept overrides the default Accept media type.
func WithAccept(a string) Option {
	return func(c *Client) {
		if strings.TrimSpace(a) != "" {
			c.accept = strings.TrimSpace(a)
		}
	}
}

// Request is one API call. Path is either relative to the base URL or an
// absolute URL on the same host - the latter is how pagination follows Link
// headers.
type Request struct {
	Method string
	Path   string
	// Body is marshalled as JSON when non-nil, which keeps "no body"
	// distinguishable from "empty body".
	Body   any
	Header http.Header
	// Accept overrides the client-level Accept header (GraphQL needs
	// application/json).
	Accept string
}

// Response carries status, headers and the raw body. The body is never
// persisted outside the call.
type Response struct {
	Status int
	Header http.Header
	Body   []byte
}

// Do performs exactly one attempt. It never retries - not for 5xx, not for
// rate limiting - because an unknown write outcome must be reconciled with
// journal evidence rather than replayed.
func (c *Client) Do(ctx context.Context, req Request) (Response, error) {
	method := strings.ToUpper(strings.TrimSpace(req.Method))
	if method == "" {
		method = http.MethodGet
	}
	if ctx == nil {
		ctx = context.Background()
	}
	target, err := c.resolve(req.Path)
	if err != nil {
		return Response{}, err
	}

	var payload []byte
	if req.Body != nil {
		payload, err = json.Marshal(req.Body)
		if err != nil {
			return Response{}, &Error{
				Kind:     KindContract,
				Method:   method,
				Endpoint: RedactEndpoint(req.Path),
				Detail:   "request body cannot be serialised",
			}
		}
	}
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}

	hreq, err := http.NewRequestWithContext(ctx, method, target.String(), body)
	if err != nil {
		return Response{}, &Error{
			Kind:     KindContract,
			Method:   method,
			Endpoint: RedactEndpoint(req.Path),
			Detail:   "request cannot be built",
		}
	}
	accept := c.accept
	if strings.TrimSpace(req.Accept) != "" {
		accept = strings.TrimSpace(req.Accept)
	}
	hreq.Header.Set("Accept", accept)
	hreq.Header.Set("X-GitHub-Api-Version", c.apiVersion)
	hreq.Header.Set("Cache-Control", "no-cache")
	hreq.Header.Set("User-Agent", c.userAgent)
	if c.token != "" {
		hreq.Header.Set("Authorization", c.scheme+" "+c.token)
	}
	if payload != nil {
		hreq.Header.Set("Content-Type", "application/json")
	}
	for name, values := range req.Header {
		for _, value := range values {
			hreq.Header.Set(name, value)
		}
	}

	resp, err := c.http.Do(hreq)
	if err != nil {
		return Response{}, c.transportError(method, req.Path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if readErr != nil {
		return Response{}, c.transportError(method, req.Path, readErr)
	}
	if len(raw) > maxResponseBytes {
		return Response{}, &Error{
			Kind:     KindContract,
			Method:   method,
			Endpoint: RedactEndpoint(req.Path),
			Status:   resp.StatusCode,
			Detail:   "response body exceeds the size limit",
		}
	}

	out := Response{Status: resp.StatusCode, Header: resp.Header.Clone(), Body: raw}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return out, httpStatusError(method, req.Path, resp)
	}
	return out, nil
}

// Get issues a GET and decodes the JSON body into out. A nil out, or an empty
// body, is not an error: 204 responses are legitimate.
func (c *Client) Get(ctx context.Context, path string, out any) error {
	return c.do(ctx, Request{Method: http.MethodGet, Path: path}, out)
}

// Post issues a POST with a JSON body.
func (c *Client) Post(ctx context.Context, path string, body, out any) error {
	return c.do(ctx, Request{Method: http.MethodPost, Path: path, Body: body}, out)
}

// Patch issues a PATCH with a JSON body.
func (c *Client) Patch(ctx context.Context, path string, body, out any) error {
	return c.do(ctx, Request{Method: http.MethodPatch, Path: path, Body: body}, out)
}

// Put issues a PUT with a JSON body.
func (c *Client) Put(ctx context.Context, path string, body, out any) error {
	return c.do(ctx, Request{Method: http.MethodPut, Path: path, Body: body}, out)
}

// Delete issues a DELETE. GitHub usually answers 204, so out may be nil.
func (c *Client) Delete(ctx context.Context, path string, out any) error {
	return c.do(ctx, Request{Method: http.MethodDelete, Path: path}, out)
}

func (c *Client) do(ctx context.Context, req Request, out any) error {
	resp, err := c.Do(ctx, req)
	if err != nil {
		return err
	}
	return decodeInto(req.Method, req.Path, resp, out)
}

// decodeInto maps shape failures onto KindDecode without echoing the body.
func decodeInto(method, path string, resp Response, out any) error {
	if out == nil {
		return nil
	}
	if len(bytes.TrimSpace(resp.Body)) == 0 {
		return nil
	}
	if err := json.Unmarshal(resp.Body, out); err != nil {
		return decodeError(method, path, resp.Status)
	}
	return nil
}

func decodeError(method, path string, status int) *Error {
	return &Error{
		Kind:     KindDecode,
		Method:   method,
		Endpoint: RedactEndpoint(path),
		Status:   status,
		Detail:   "response body is not valid JSON",
	}
}

// resolve builds the absolute URL for a path, refusing absolute URLs that point
// at another host: following one would send the token elsewhere.
func (c *Client) resolve(rawPath string) (*url.URL, error) {
	if strings.TrimSpace(rawPath) == "" {
		return nil, &Error{Kind: KindContract, Detail: "empty request path"}
	}
	if c.baseErr != nil || c.base == nil {
		return nil, &Error{
			Kind:     KindContract,
			Endpoint: RedactEndpoint(rawPath),
			Detail:   "the client has no usable base URL",
		}
	}
	if strings.HasPrefix(rawPath, "http://") || strings.HasPrefix(rawPath, "https://") {
		u, err := url.Parse(rawPath)
		if err != nil {
			return nil, &Error{
				Kind:     KindContract,
				Endpoint: RedactEndpoint(rawPath),
				Detail:   "pagination link is not a valid URL",
			}
		}
		if !strings.EqualFold(u.Scheme, c.base.Scheme) || !strings.EqualFold(u.Host, c.base.Host) {
			return nil, &Error{
				Kind:     KindContract,
				Endpoint: RedactEndpoint(rawPath),
				Detail:   "pagination link points outside the configured API host",
			}
		}
		return u, nil
	}
	rel, err := url.Parse(rawPath)
	if err != nil {
		return nil, &Error{
			Kind:     KindContract,
			Endpoint: RedactEndpoint(rawPath),
			Detail:   "request path is not a valid URL reference",
		}
	}
	u := *c.base
	u.Path = strings.TrimSuffix(c.base.Path, "/") + rel.Path
	u.RawQuery = rel.RawQuery
	u.Fragment = ""
	u.RawFragment = ""
	return &u, nil
}

func (c *Client) transportError(method, path string, err error) *Error {
	// A cancelled context is a local decision, not a network condition.
	if errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		return &Error{
			Kind:     KindUnknown,
			Method:   method,
			Endpoint: RedactEndpoint(path),
			Detail:   "the request was cancelled",
			cause:    err,
		}
	}
	kind := classifyTransport(err, c.proxyHost)
	detail := transportDetail(kind)
	if kind == KindRedirect {
		detail = redirectDetail(err)
	}
	return &Error{
		Kind:     kind,
		Method:   method,
		Endpoint: RedactEndpoint(path),
		Detail:   detail,
		cause:    err,
	}
}

// httpStatusError classifies a non-2xx response. 401 is an authentication
// problem; 403/404 stay deliberately ambiguous between "does not exist" and
// "not visible", because GitHub uses 404 for both.
func httpStatusError(method, path string, resp *http.Response) *Error {
	e := &Error{
		Kind:     KindHTTP,
		Method:   method,
		Endpoint: RedactEndpoint(path),
		Status:   resp.StatusCode,
	}
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		e.Category = CategoryAuthentication
		e.Detail = "the credential was rejected"
	case http.StatusForbidden, http.StatusNotFound:
		e.Category = CategoryRepositoryAccess
		e.Detail = "the repository is not visible or the credential lacks access"
	}
	if isRateLimitedResponse(resp) {
		e.Category = CategoryRateLimited
		e.Detail = "the request was rate limited"
	}
	return e
}

func isRateLimitedResponse(resp *http.Response) bool {
	if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusTooManyRequests {
		return false
	}
	if resp.Header.Get("Retry-After") != "" {
		return true
	}
	return resp.Header.Get("X-RateLimit-Remaining") == "0"
}

// hardenRedirects stops net/http from replaying the Authorization header to
// another host. Only applied when the caller did not set a policy.
func (c *Client) hardenRedirects() {
	if c.http == nil || c.http.CheckRedirect != nil {
		return
	}
	c.http.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= maxRedirects {
			return ErrTooManyRedirects
		}
		if len(via) > 0 && !strings.EqualFold(req.URL.Host, via[0].URL.Host) {
			return ErrRedirectToAnotherHost
		}
		return nil
	}
}

// proxyHostFor reports the proxy address that would be used for target, so a
// connection failure can be attributed to the proxy rather than the API host.
func proxyHostFor(transport *http.Transport, target *url.URL) string {
	proxy := transport.Proxy
	if proxy == nil {
		proxy = http.ProxyFromEnvironment
	}
	u, err := proxy(&http.Request{URL: target})
	if err != nil || u == nil || u.Host == "" {
		return ""
	}
	return u.Host
}
