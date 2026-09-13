package github

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"syscall"
)

// ErrorKind classifies a failure. Classification is derived from Go error
// types and HTTP status codes, never from remote text: parsing stderr or
// response bodies is what makes error handling drift between platforms.
type ErrorKind string

// Redirect sentinels. net/http reports a refused redirect as a *url.Error
// wrapping whatever CheckRedirect returned, so the policy returns these two
// values instead of anonymous errors: classifyTransport can then recognise the
// refusal, and callers can match it with errors.Is.
var (
	// ErrRedirectToAnotherHost is returned instead of replaying the
	// Authorization header to a host the credential was not issued for.
	ErrRedirectToAnotherHost = errors.New("refusing to follow a redirect to another host")
	// ErrTooManyRedirects is returned once the chain exceeds maxRedirects.
	ErrTooManyRedirects = errors.New("stopped after too many redirects")
)

// Error kinds. The first group comes from the transport, the second from the
// response, the last two from the contracts this package enforces.
const (
	KindDNS          ErrorKind = "dns"
	KindTLS          ErrorKind = "tls"
	KindProxy        ErrorKind = "proxy"
	KindTimeout      ErrorKind = "timeout"
	KindRefused      ErrorKind = "refused"
	KindReset        ErrorKind = "reset"
	KindUnreachable  ErrorKind = "unreachable"
	KindAccessDenied ErrorKind = "access_denied"
	// KindRedirect means the client refused to follow a redirect: either it
	// pointed at another host or the chain ran past the limit. The status of
	// the redirect itself is not reported, because the request never left.
	KindRedirect ErrorKind = "redirect"
	KindUnknown  ErrorKind = "unknown"

	KindHTTP ErrorKind = "http"

	KindDecode ErrorKind = "decode"

	// KindIncomplete means the call succeeded but the result cannot be proven
	// complete (pagination budget, cursor or total_count shortfall).
	KindIncomplete ErrorKind = "incomplete"
	// KindContract means the response violates the documented shape of the
	// endpoint (missing field, duplicated cursor, SHA mismatch).
	KindContract ErrorKind = "contract"
	// KindProcess is a local subprocess failure (git), kept here so callers
	// can classify every failure with one type.
	KindProcess ErrorKind = "process"
)

// Error categories add the "why" that a status code alone cannot carry. They
// never leak remote text.
const (
	CategoryAuthentication   = "authentication"
	CategoryRepositoryAccess = "repository_access_or_visibility"
	CategoryRateLimited      = "rate_limited"
	CategoryGraphQL          = "graphql"
)

// Error is the single failure type of this package. Every field is safe to
// print: Method, a redacted endpoint template, the HTTP status and a short
// local detail. Tokens, repo names, raw URLs and response bodies are never
// part of it - the wrapped cause is kept for errors.As/errors.Is only and is
// deliberately excluded from Error().
type Error struct {
	Kind     ErrorKind
	Method   string
	Endpoint string
	Status   int
	Category string
	Detail   string

	cause error
}

// Error implements error without ever rendering the cause: net/http errors
// embed the full URL, which would leak the owner/repo names.
func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString("github: ")
	if e.Method != "" {
		b.WriteString(e.Method)
		b.WriteString(" ")
	}
	if e.Endpoint != "" {
		b.WriteString(e.Endpoint)
		b.WriteString(": ")
	}
	b.WriteString(string(e.Kind))
	if e.Status != 0 {
		fmt.Fprintf(&b, " status=%d", e.Status)
	}
	if e.Category != "" {
		fmt.Fprintf(&b, " category=%s", e.Category)
	}
	if e.Detail != "" {
		fmt.Fprintf(&b, " (%s)", e.Detail)
	}
	return b.String()
}

// Unwrap exposes the cause for errors.Is/errors.As. Callers must not print it.
func (e *Error) Unwrap() error { return e.cause }

// As extracts the typed error from a chain.
func As(err error) (*Error, bool) {
	var target *Error
	if errors.As(err, &target) {
		return target, true
	}
	return nil, false
}

// KindOf reports the kind of a typed error.
func KindOf(err error) (ErrorKind, bool) {
	if e, ok := As(err); ok {
		return e.Kind, true
	}
	return "", false
}

// IsKind reports whether err is a typed error of any of the given kinds.
func IsKind(err error, kinds ...ErrorKind) bool {
	e, ok := As(err)
	if !ok {
		return false
	}
	for _, k := range kinds {
		if e.Kind == k {
			return true
		}
	}
	return false
}

// IsContract reports whether err means "the result is not trustworthy" -
// either a shape violation or a proof of incompleteness. Both must reach the
// caller as a failure, never as a partial result.
func IsContract(err error) bool {
	return IsKind(err, KindContract, KindIncomplete)
}

// IsRateLimited reports whether the failure was GitHub throttling us.
func IsRateLimited(err error) bool {
	e, ok := As(err)
	return ok && e.Category == CategoryRateLimited
}

// endpointLiterals are the path segments that are part of the API vocabulary
// rather than caller data. Anything else is a value and gets redacted.
var endpointLiterals = map[string]struct{}{
	"app": {}, "app-manifests": {}, "installations": {}, "installation": {},
	"access_tokens": {},
	"repositories":  {}, "graphql": {}, "search": {},
	"user": {}, "users": {}, "orgs": {}, "teams": {}, "repos": {},
	"issues": {}, "pulls": {}, "comments": {}, "reviews": {}, "events": {},
	"commits": {}, "branches": {}, "contents": {}, "compare": {},
	"labels": {}, "milestones": {}, "timeline": {}, "lock": {},
	"merge": {}, "merges": {}, "files": {}, "assignees": {}, "collaborators": {},
	"check-runs": {}, "checks": {}, "statuses": {}, "status": {},
	"rules": {}, "rulesets": {}, "protection": {},
	"git": {}, "ref": {}, "refs": {}, "matching-refs": {}, "heads": {}, "tags": {},
	"actions": {}, "runs": {}, "workflows": {}, "workflow": {}, "jobs": {}, "artifacts": {},
	"deployments": {}, "environments": {}, "releases": {}, "keys": {}, "hooks": {},
	"requested_reviewers": {}, "pages": {}, "traffic": {}, "stats": {},
	"contributors": {}, "languages": {}, "topics": {}, "transfer": {}, "archive": {},
	"tarball": {}, "zipball": {}, "license": {}, "notifications": {},
	"subscription": {}, "subscribers": {}, "stargazers": {}, "forks": {},
}

// valueIntroducingPrefixes maps a first path segment to how many of the
// following segments are caller data rather than API vocabulary. Without this
// a repo literally named "issues" (or an owner named "user") would survive
// redaction, so the positional rule always wins over the vocabulary.
var valueIntroducingPrefixes = map[string]int{
	"repos":         2, // /repos/{owner}/{repo}/...
	"orgs":          1,
	"users":         1,
	"teams":         1,
	"installations": 1,
	"app-manifests": 1,
}

// RedactEndpoint turns a path (or an absolute URL) into the endpoint template
// that is safe to log, for example:
//
//	/repos/ghpipe/ghpipe/pulls/7 -> /repos/{value}/{value}/pulls/{value}
//
// Query strings are collapsed to "?{query}" because their values carry
// caller data too (branch names, search terms, refs).
func RedactEndpoint(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	path, query := splitPathQuery(raw)
	segments := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for i, segment := range segments {
		switch {
		case segment == "":
		case isPositionalValue(segments, i):
			segments[i] = "{value}"
		default:
			if _, ok := endpointLiterals[segment]; !ok {
				segments[i] = "{value}"
			}
		}
	}
	redacted := "/" + strings.Join(segments, "/")
	if query != "" {
		redacted += "?{query}"
	}
	return redacted
}

func splitPathQuery(raw string) (string, string) {
	if u, err := url.Parse(raw); err == nil && u.Scheme != "" {
		return u.EscapedPath(), u.RawQuery
	}
	if idx := strings.IndexByte(raw, '?'); idx >= 0 {
		return raw[:idx], raw[idx+1:]
	}
	return raw, ""
}

func isPositionalValue(segments []string, i int) bool {
	if len(segments) == 0 {
		return false
	}
	count, ok := valueIntroducingPrefixes[segments[0]]
	if !ok {
		return false
	}
	return i >= 1 && i <= count
}

// classifyTransport maps a Go transport error onto an ErrorKind. No message
// text is inspected; only error types and syscall values are.
func classifyTransport(err error, proxyHost string) ErrorKind {
	if err == nil {
		return KindUnknown
	}
	// A refused redirect is a local policy decision, so it is recognised
	// before the network-shaped checks below: the sentinel arrives wrapped in
	// a *url.Error, which would otherwise fall through to KindUnknown.
	if errors.Is(err, ErrRedirectToAnotherHost) || errors.Is(err, ErrTooManyRedirects) {
		return KindRedirect
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return KindTimeout
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Timeout() {
		return KindTimeout
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return KindDNS
	}
	var (
		unknownAuthority x509.UnknownAuthorityError
		invalidCert      x509.CertificateInvalidError
		hostnameErr      x509.HostnameError
		recordHeader     tls.RecordHeaderError
	)
	if errors.As(err, &unknownAuthority) || errors.As(err, &invalidCert) ||
		errors.As(err, &hostnameErr) || errors.As(err, &recordHeader) {
		return KindTLS
	}
	if proxyHost != "" {
		// net/http may wrap the dial failure in further net.OpError layers, so
		// the whole chain is searched rather than only the outermost error.
		if errorChainTouchesAddr(err, proxyHost) {
			return KindProxy
		}
	}
	switch {
	// These nine values are the portable half of syscall: Go defines them on
	// every target we ship (the six-target build in this repository proves it)
	// and they are the only reliable way to tell "refused" from "reset"
	// without parsing text. No other platform capability is reached outside
	// internal/hostfs.
	case errors.Is(err, syscall.ECONNREFUSED):
		return KindRefused
	case errors.Is(err, syscall.ECONNRESET):
		return KindReset
	case errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETUNREACH):
		return KindUnreachable
	case errors.Is(err, syscall.EACCES), errors.Is(err, syscall.EPERM):
		return KindAccessDenied
	}
	return KindUnknown
}

// errorChainTouchesAddr reports whether any *net.OpError in the chain operated
// on the given address. Addresses are compared case-insensitively because host
// names are case-insensitive.
func errorChainTouchesAddr(err error, addr string) bool {
	for current := err; current != nil; current = errors.Unwrap(current) {
		opErr, ok := current.(*net.OpError)
		if !ok || opErr.Addr == nil {
			continue
		}
		if strings.EqualFold(opErr.Addr.String(), addr) {
			return true
		}
	}
	return false
}

// transportDetail is a fixed, local sentence per kind: it must never echo the
// error text, which can contain the full URL.
func transportDetail(kind ErrorKind) string {
	switch kind {
	case KindDNS:
		return "the API host could not be resolved"
	case KindTLS:
		return "the TLS handshake failed"
	case KindProxy:
		return "the configured proxy could not be reached"
	case KindTimeout:
		return "the request timed out"
	case KindRefused:
		return "the connection was refused"
	case KindReset:
		return "the connection was reset"
	case KindUnreachable:
		return "the API host is unreachable"
	case KindAccessDenied:
		return "the process is not allowed to open the connection"
	case KindRedirect:
		return "the redirect was refused"
	default:
		return "the request failed before a response was received"
	}
}

// redirectDetail says which redirect rule fired, so an operator can tell
// "the token was not replayed" from "the chain is looping" without reading
// unprintable error text.
func redirectDetail(err error) string {
	switch {
	case errors.Is(err, ErrRedirectToAnotherHost):
		return "the redirect points at another host and was refused to keep the credential off it"
	case errors.Is(err, ErrTooManyRedirects):
		return "the redirect chain ran past the redirect limit"
	default:
		return transportDetail(KindRedirect)
	}
}
