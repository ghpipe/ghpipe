package github

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"testing"
)

// testToken is a deliberately recognisable value: several tests assert that it
// never reaches an error string.
const testToken = "ghp_secret_token_value"

// testBaseURL is the API host the fake transports impersonate. Tests build
// pagination links against it exactly as GitHub would.
const testBaseURL = DefaultBaseURL

// fakeTransport is the injectable stand-in for the GitHub API host. It records
// every request it is handed and answers from a per-test responder, so no test
// has to bind a socket: the socket layer is not what this package's contracts
// are about, and a sandbox that denies listen(2) can still run the suite.
type fakeTransport struct {
	respond func(*http.Request) (*http.Response, error)

	mu       sync.Mutex
	requests []*http.Request
}

func (f *fakeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	f.requests = append(f.requests, req)
	f.mu.Unlock()
	return f.respond(req)
}

// calls reports how many requests reached the transport. "Not retried" is a
// statement about this number, not about what some server happened to observe.
func (f *fakeTransport) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

// recorder captures what a handler writes so the fake transport can hand it
// back as an *http.Response. Header values are frozen when the handler commits
// a status, which is the point a real server stops accepting them.
type recorder struct {
	header http.Header
	frozen http.Header
	status int
	body   bytes.Buffer
}

func newRecorder() *recorder { return &recorder{header: make(http.Header)} }

func (r *recorder) Header() http.Header { return r.header }

func (r *recorder) WriteHeader(status int) {
	if r.status != 0 {
		return
	}
	r.status, r.frozen = status, r.header.Clone()
}

func (r *recorder) Write(p []byte) (int, error) {
	if r.status == 0 {
		r.WriteHeader(http.StatusOK)
	}
	return r.body.Write(p)
}

// response renders the captured exchange as the reply the transport returns.
func (r *recorder) response(req *http.Request) *http.Response {
	if r.status == 0 {
		r.WriteHeader(http.StatusOK)
	}
	return &http.Response{
		Status:        fmt.Sprintf("%d %s", r.status, http.StatusText(r.status)),
		StatusCode:    r.status,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        r.frozen,
		Body:          io.NopCloser(bytes.NewReader(r.body.Bytes())),
		ContentLength: int64(r.body.Len()),
		Request:       req,
	}
}

// servedBy adapts a handler into a transport responder: the handler sees the
// request the client actually built, and its status, headers and body become
// the response. It keeps the ordinary cases reading like the HTTP conversation
// they describe without any of it leaving the process.
func servedBy(handler http.Handler) func(*http.Request) (*http.Response, error) {
	return func(req *http.Request) (*http.Response, error) {
		rec := newRecorder()
		handler.ServeHTTP(rec, req)
		return rec.response(req), nil
	}
}

// newTestClient returns a client wired to a fake transport, plus the transport
// itself so a test can inspect what was sent.
func newTestClient(t *testing.T, respond func(*http.Request) (*http.Response, error)) (*Client, *fakeTransport) {
	t.Helper()
	transport := &fakeTransport{respond: respond}
	client := New(
		WithBaseURL(testBaseURL),
		WithToken(testToken),
		WithHTTPClient(&http.Client{Transport: transport}),
	)
	return client, transport
}

func testClient(t *testing.T, handler http.Handler) *Client {
	t.Helper()
	client, _ := newTestClient(t, servedBy(handler))
	return client
}

// assertRedacted fails when an error string carries anything a log must not
// contain: the credential, the repository identity, a raw URL or remote text.
func assertRedacted(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("assertRedacted called with a nil error")
	}
	text := err.Error()
	for _, leak := range []string{testToken, "ghpipe/ghpipe", "http://", "https://", "Not Found", "boom"} {
		if strings.Contains(text, leak) {
			t.Errorf("error text leaks %q: %s", leak, text)
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Type alias so a failed dial can report the proxy address verbatim.
type stringAddr string

func (a stringAddr) Network() string { return "tcp" }
func (a stringAddr) String() string  { return string(a) }

// deadlineError is a net.Error that reports an expired deadline, which is what
// net/http hands back when a request runs out of time.
type deadlineError struct{}

var _ net.Error = deadlineError{}

func (deadlineError) Error() string   { return "i/o timeout" }
func (deadlineError) Timeout() bool   { return true }
func (deadlineError) Temporary() bool { return true }

// AC3: the five fixed headers are actually sent.
func TestGetSendsTheFixedHeaders(t *testing.T) {
	var received http.Header
	client := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received = r.Header.Clone()
		_, _ = io.WriteString(w, `{}`)
	}))

	if err := client.Get(context.Background(), "/repos/ghpipe/ghpipe", nil); err != nil {
		t.Fatalf("Get: %v", err)
	}

	want := map[string]string{
		"Authorization":        "Bearer " + testToken,
		"Accept":               DefaultAccept,
		"X-GitHub-Api-Version": DefaultAPIVersion,
		"Cache-Control":        "no-cache",
	}
	for header, value := range want {
		if got := received.Get(header); got != value {
			t.Errorf("%s = %q, want %q", header, got, value)
		}
	}
	if ua := received.Get("User-Agent"); !strings.HasPrefix(ua, "ghpipe/") {
		t.Errorf("User-Agent = %q, want a ghpipe/<version> value", ua)
	}
}

// AC2: 404 is a typed HTTP error whose endpoint is the redacted template.
func TestHTTPErrorIsTypedAndRedacted(t *testing.T) {
	client := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"message":"Not Found"}`)
	}))

	err := client.Get(context.Background(), "/repos/ghpipe/ghpipe/issues/7", nil)
	if err == nil {
		t.Fatal("expected an error for status 404")
	}
	typed, ok := As(err)
	if !ok {
		t.Fatalf("error is not a *github.Error: %v", err)
	}
	if typed.Kind != KindHTTP {
		t.Errorf("Kind = %q, want %q", typed.Kind, KindHTTP)
	}
	if typed.Status != http.StatusNotFound {
		t.Errorf("Status = %d, want %d", typed.Status, http.StatusNotFound)
	}
	if typed.Category != CategoryRepositoryAccess {
		t.Errorf("Category = %q, want %q", typed.Category, CategoryRepositoryAccess)
	}
	const wantEndpoint = "/repos/{value}/{value}/issues/{value}"
	if typed.Endpoint != wantEndpoint {
		t.Errorf("Endpoint = %q, want %q", typed.Endpoint, wantEndpoint)
	}
	assertRedacted(t, err)
}

// AC2: a deadline is a transport failure of kind timeout, not a decode or
// contract failure. The deadline is raised by the transport itself, so the test
// asserts the classification without waiting on a timer.
func TestTimeoutIsClassifiedAsTimeout(t *testing.T) {
	client, transport := newTestClient(t, func(*http.Request) (*http.Response, error) {
		return nil, deadlineError{}
	})

	err := client.Get(context.Background(), "/repos/ghpipe/ghpipe", nil)
	kind, ok := KindOf(err)
	if !ok || kind != KindTimeout {
		t.Fatalf("kind = %q (typed=%v), want %q; err = %v", kind, ok, KindTimeout, err)
	}
	if got := transport.calls(); got != 1 {
		t.Errorf("transport saw %d attempts, want 1", got)
	}
	assertRedacted(t, err)
}

// AC2: a body that is not JSON is a decode error, never a contract error.
func TestInvalidJSONIsADecodeError(t *testing.T) {
	client := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "this is not json")
	}))

	var out map[string]any
	err := client.Get(context.Background(), "/repos/ghpipe/ghpipe/issues", &out)
	if kind, ok := KindOf(err); !ok || kind != KindDecode {
		t.Fatalf("kind = %q (typed=%v), want %q; err = %v", kind, ok, KindDecode, err)
	}
	assertRedacted(t, err)
}

// AC2: 5xx is not retried - the transport must be asked exactly once.
func TestServerErrorsAreNotRetried(t *testing.T) {
	client, transport := newTestClient(t, servedBy(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"message":"boom"}`)
	})))

	err := client.Get(context.Background(), "/repos/ghpipe/ghpipe/issues", nil)
	if typed, ok := As(err); !ok || typed.Status != http.StatusInternalServerError {
		t.Fatalf("err = %v, want a typed 500", err)
	}
	if got := transport.calls(); got != 1 {
		t.Fatalf("transport saw %d attempts, want exactly 1", got)
	}
	assertRedacted(t, err)
}

// An empty body is not an error: DELETE answers 204 and bodyless 2xx responses
// are normal.
func TestEmptyBodyIsNotAnError(t *testing.T) {
	client := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	var out map[string]any
	if err := client.Delete(context.Background(), "/repos/ghpipe/ghpipe/issues/7/labels/bug", &out); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if out != nil {
		t.Errorf("out = %v, want nil", out)
	}
}

// Transport failures are classified from Go error types, never from text.
func TestTransportErrorsAreClassifiedByType(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want ErrorKind
	}{
		{"dns", &net.DNSError{Err: "no such host", Name: "api.github.com"}, KindDNS},
		{"refused", &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}, KindRefused},
		{"reset", &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}, KindReset},
		{"unreachable", &net.OpError{Op: "dial", Net: "tcp", Err: syscall.EHOSTUNREACH}, KindUnreachable},
		{"access_denied", &net.OpError{Op: "dial", Net: "tcp", Err: syscall.EACCES}, KindAccessDenied},
		{"tls", x509.UnknownAuthorityError{}, KindTLS},
		{"timeout", &url.Error{Op: "Get", URL: "https://api.github.com", Err: context.DeadlineExceeded}, KindTimeout},
		// net/http wraps a CheckRedirect failure in a *url.Error, which is the
		// shape the real redirect path produces.
		{"redirect to another host", &url.Error{Op: "Get", URL: "https://api.github.com", Err: ErrRedirectToAnotherHost}, KindRedirect},
		{"too many redirects", &url.Error{Op: "Get", URL: "https://api.github.com", Err: ErrTooManyRedirects}, KindRedirect},
		{"unknown", fmt.Errorf("something else"), KindUnknown},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			httpClient := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return nil, tc.err
			})}
			client := New(WithBaseURL(DefaultBaseURL), WithToken(testToken), WithHTTPClient(httpClient))

			err := client.Get(context.Background(), "/repos/ghpipe/ghpipe", nil)
			kind, ok := KindOf(err)
			if !ok || kind != tc.want {
				t.Fatalf("kind = %q (typed=%v), want %q; err = %v", kind, ok, tc.want, err)
			}
			assertRedacted(t, err)
		})
	}
}

// A dial failure against the configured proxy is reported as a proxy problem,
// so the operator does not go looking at api.github.com.
func TestProxyFailureIsClassifiedAsProxy(t *testing.T) {
	const proxyHost = "proxy.internal:3128"
	transport := &http.Transport{
		Proxy: func(*http.Request) (*url.URL, error) {
			return url.Parse("http://" + proxyHost)
		},
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return nil, &net.OpError{
				Op:   "dial",
				Net:  network,
				Addr: stringAddr(addr),
				Err:  syscall.ECONNREFUSED,
			}
		},
	}
	client := New(
		WithHTTPClient(&http.Client{Transport: transport}),
		WithBaseURL("https://api.github.com"),
		WithToken(testToken),
	)

	err := client.Get(context.Background(), "/repos/ghpipe/ghpipe", nil)
	if kind, ok := KindOf(err); !ok || kind != KindProxy {
		t.Fatalf("kind = %q (typed=%v), want %q; err = %v", kind, ok, KindProxy, err)
	}
	assertRedacted(t, err)
}

// Following a pagination link to another host would hand the token to that
// host, so it is refused as a contract violation.
func TestCrossHostPaginationLinkIsRefused(t *testing.T) {
	client, transport := newTestClient(t, servedBy(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `[]`)
	})))

	err := client.Get(context.Background(), "https://elsewhere.example/repos/ghpipe/ghpipe", nil)
	if kind, ok := KindOf(err); !ok || kind != KindContract {
		t.Fatalf("kind = %q (typed=%v), want %q; err = %v", kind, ok, KindContract, err)
	}
	if got := transport.calls(); got != 0 {
		t.Fatalf("the request reached the transport %d time(s), want 0", got)
	}
	assertRedacted(t, err)
}

// A redirect answered by the API host must not become a second request: the
// credential was not issued for the host in the Location header. The refusal is
// classified as a redirect, not as an unidentifiable transport failure.
func TestRedirectToAnotherHostIsRefusedAndClassified(t *testing.T) {
	var authorized []string
	client, transport := newTestClient(t, servedBy(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorized = append(authorized, r.Header.Get("Authorization"))
		w.Header().Set("Location", "https://elsewhere.example/repos/ghpipe/ghpipe")
		w.WriteHeader(http.StatusFound)
	})))

	err := client.Get(context.Background(), "/repos/ghpipe/ghpipe", nil)
	if kind, ok := KindOf(err); !ok || kind != KindRedirect {
		t.Fatalf("kind = %q (typed=%v), want %q; err = %v", kind, ok, KindRedirect, err)
	}
	if !errors.Is(err, ErrRedirectToAnotherHost) {
		t.Errorf("err = %v, want it to wrap %v", err, ErrRedirectToAnotherHost)
	}
	// The call count is the proof that the Authorization header was never
	// replayed to the host named by the redirect.
	if got := transport.calls(); got != 1 {
		t.Fatalf("the transport saw %d requests, want 1: the credential must not be replayed", got)
	}
	if want := DefaultTokenScheme + " " + testToken; len(authorized) != 1 || authorized[0] != want {
		t.Fatalf("the API host received Authorization %q, want exactly one %q", authorized, want)
	}
	assertRedacted(t, err)
}

// A redirect chain that never settles is refused for the same reason: the walk
// is bounded, and the bound is reported as a redirect failure.
func TestTooManyRedirectsIsClassified(t *testing.T) {
	client, transport := newTestClient(t, servedBy(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", testBaseURL+"/loop")
		w.WriteHeader(http.StatusFound)
	})))

	err := client.Get(context.Background(), "/loop", nil)
	if kind, ok := KindOf(err); !ok || kind != KindRedirect {
		t.Fatalf("kind = %q (typed=%v), want %q; err = %v", kind, ok, KindRedirect, err)
	}
	if !errors.Is(err, ErrTooManyRedirects) {
		t.Errorf("err = %v, want it to wrap %v", err, ErrTooManyRedirects)
	}
	if got := transport.calls(); got != maxRedirects {
		t.Errorf("the transport saw %d requests, want %d: the chain stops at the limit", got, maxRedirects)
	}
	assertRedacted(t, err)
}

// A typed error must survive a JSON round trip of its shape: kind, endpoint and
// status are the machine-readable part of the contract.
func TestErrorFieldsAreMachineReadable(t *testing.T) {
	err := pageFailure(KindIncomplete, "/repos/ghpipe/ghpipe/issues/7", "collected 1 of 5 reported items")
	encoded, marshalErr := json.Marshal(struct {
		Kind     string `json:"kind"`
		Endpoint string `json:"endpoint"`
		Detail   string `json:"detail"`
	}{string(err.Kind), err.Endpoint, err.Detail})
	if marshalErr != nil {
		t.Fatalf("Marshal: %v", marshalErr)
	}
	if !strings.Contains(string(encoded), `"incomplete"`) ||
		!strings.Contains(string(encoded), `"/repos/{value}/{value}/issues/{value}"`) {
		t.Fatalf("encoded error = %s", encoded)
	}
}
