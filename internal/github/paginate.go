package github

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
)

// Pagination contract constants.
const (
	// DefaultMaxPages bounds how many pages one call will follow. Exceeding it
	// yields an incomplete result, never a truncated "success".
	DefaultMaxPages = 50
	// ContractPerPage is forced on every first-page request: GitHub only
	// guarantees the Link contract at the maximum page size.
	ContractPerPage = 100
)

// PaginateOptions tunes one pagination. The zero value is the contract
// default: at most DefaultMaxPages pages, automatic items field detection.
type PaginateOptions struct {
	// MaxPages overrides DefaultMaxPages when positive.
	MaxPages int
	// ItemsField names the array inside an object-shaped page. When empty the
	// well-known names are tried in order.
	ItemsField string
}

// Paginate walks every page of a REST list endpoint and appends each element to
// out, which must be a non-nil pointer to a slice.
//
// It returns a typed error and leaves out untouched whenever completeness
// cannot be proven: a duplicate page, a total_count shortfall, or an exhausted
// page budget all fail the call. Callers therefore either get every element or
// get nothing - a partially filled ledger is worse than no answer.
func (c *Client) Paginate(ctx context.Context, path string, out any, opts PaginateOptions) error {
	target, elemType, err := sliceTarget(out)
	if err != nil {
		return err
	}
	maxPages := opts.MaxPages
	if maxPages <= 0 {
		maxPages = DefaultMaxPages
	}

	collected := reflect.MakeSlice(target.Elem().Type(), 0, 0)
	seen := make(map[[sha256.Size]byte]int)
	var totalCount *int

	next := firstPagePath(path)
	for page := 1; next != ""; page++ {
		resp, err := c.Do(ctx, Request{Method: http.MethodGet, Path: next})
		if err != nil {
			return err
		}
		body := bytes.TrimSpace(resp.Body)
		if len(body) == 0 {
			return pageFailure(KindIncomplete, path, fmt.Sprintf("page %d was empty", page))
		}

		fingerprint := sha256.Sum256(canonicalJSON(body))
		if first, duplicate := seen[fingerprint]; duplicate {
			return pageFailure(KindContract, path,
				fmt.Sprintf("page %d repeats page %d byte for byte", page, first))
		}
		seen[fingerprint] = page

		items, pageTotal, err := splitPage(path, body, opts.ItemsField)
		if err != nil {
			return err
		}
		if pageTotal != nil {
			if totalCount != nil && *totalCount != *pageTotal {
				return pageFailure(KindContract, path, "total_count changed between pages")
			}
			totalCount = pageTotal
		}
		for _, raw := range items {
			value := reflect.New(elemType)
			if err := json.Unmarshal(raw, value.Interface()); err != nil {
				return pageFailure(KindDecode, path, fmt.Sprintf("page %d holds an element that does not match the target type", page))
			}
			collected = reflect.Append(collected, value.Elem())
		}

		link := nextPageLink(resp.Header)
		if link == "" {
			break
		}
		if page >= maxPages {
			return pageFailure(KindIncomplete, path,
				fmt.Sprintf("stopped after the %d page budget with more pages advertised", maxPages))
		}
		next = link
	}

	if totalCount != nil && collected.Len() < *totalCount {
		return pageFailure(KindIncomplete, path,
			fmt.Sprintf("collected %d of %d reported items", collected.Len(), *totalCount))
	}

	target.Elem().Set(collected)
	return nil
}

// firstPagePath enforces the request side of the contract: caller-supplied
// page/per_page are dropped and per_page is pinned, so the Link header is
// authoritative rather than a caller's guess.
func firstPagePath(path string) string {
	u, err := url.Parse(path)
	if err != nil {
		return path
	}
	query := u.Query()
	query.Del("page")
	query.Del("per_page")
	query.Set("per_page", strconv.Itoa(ContractPerPage))
	u.RawQuery = query.Encode()
	return u.String()
}

// nextPageLink extracts rel="next" from every Link header. It follows only
// that relation: "last" and "prev" are informational.
func nextPageLink(header http.Header) string {
	for _, value := range header.Values("Link") {
		for _, raw := range splitOutside(value, ',') {
			link, ok := parseLinkValue(raw)
			if !ok {
				continue
			}
			if link.hasRelation("next") {
				return link.target
			}
		}
	}
	return ""
}

// linkValue is one entry of a Link header: the target URI plus its parameters.
type linkValue struct {
	target string
	params map[string]string
}

// hasRelation reports whether rel carries the given relation type. RFC 8288
// relation types are case-insensitive and space-separated, so a "prev next"
// value is still a next link and "next-page" is not one.
func (l linkValue) hasRelation(relation string) bool {
	for _, value := range strings.Fields(l.params["rel"]) {
		if strings.EqualFold(value, relation) {
			return true
		}
	}
	return false
}

// parseLinkValue parses the RFC 8288 link-value grammar: a URI reference in
// angle brackets followed by optional "; name=value" parameters, where a value
// is a token or a quoted string. Anything without a "<...>" target is not a
// link-value and is skipped.
func parseLinkValue(raw string) (linkValue, bool) {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, "<") {
		return linkValue{}, false
	}
	end := strings.IndexByte(raw, '>')
	if end < 0 {
		return linkValue{}, false
	}
	link := linkValue{target: raw[1:end]}
	for _, part := range splitOutside(raw[end+1:], ';') {
		name, value, ok := splitParameter(part)
		if !ok {
			continue
		}
		if link.params == nil {
			link.params = make(map[string]string, 2)
		}
		if _, duplicate := link.params[name]; !duplicate {
			link.params[name] = value
		}
	}
	return link, true
}

// splitParameter splits "name=value" at its first "=". Names are folded to
// lower case here because link-header parameter names are case-insensitive.
func splitParameter(raw string) (name, value string, ok bool) {
	raw = strings.TrimSpace(raw)
	eq := strings.IndexByte(raw, '=')
	if eq <= 0 {
		return "", "", false
	}
	name = strings.ToLower(strings.TrimSpace(raw[:eq]))
	if name == "" {
		return "", "", false
	}
	value = strings.TrimSpace(raw[eq+1:])
	if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
		value = unquoteString(value)
	}
	return name, value, true
}

// unquoteString removes the surrounding quotes of an RFC 7230 quoted-string
// and resolves its backslash escapes, so title="a,b" carries a literal comma.
func unquoteString(value string) string {
	inner := value[1 : len(value)-1]
	if !strings.ContainsRune(inner, '\\') {
		return inner
	}
	var b strings.Builder
	b.Grow(len(inner))
	for i := 0; i < len(inner); i++ {
		if inner[i] == '\\' && i+1 < len(inner) {
			i++
		}
		b.WriteByte(inner[i])
	}
	return b.String()
}

// splitOutside splits value on sep, ignoring separators that sit inside a
// "<...>" URI reference or inside a quoted string. Splitting a Link header on
// every bare comma is what used to tear a next link such as
// "<https://api.github.com/issues?page=2&q=a,b>; rel=next" in half: the
// truncated half carried no rel parameter, so the second page was dropped
// while the walk still reported success.
func splitOutside(value string, sep byte) []string {
	parts := make([]string, 0, strings.Count(value, string(sep))+1)
	var (
		current strings.Builder
		inURI   bool
		inQuote bool
		escaped bool
	)
	for i := 0; i < len(value); i++ {
		ch := value[i]
		switch {
		case escaped:
			escaped = false
		case inQuote && ch == '\\':
			escaped = true
		case ch == '"':
			inQuote = !inQuote
		case !inQuote && ch == '<':
			inURI = true
		case !inQuote && ch == '>':
			inURI = false
		case !inURI && !inQuote && ch == sep:
			parts = append(parts, current.String())
			current.Reset()
			continue
		}
		current.WriteByte(ch)
	}
	return append(parts, current.String())
}

// splitPage separates the item array from the optional total_count. A bare
// array page is the common case; object pages (search, installation
// repositories) carry an items array plus a total.
func splitPage(path string, body []byte, itemsField string) ([]json.RawMessage, *int, error) {
	switch body[0] {
	case '[':
		var items []json.RawMessage
		if err := json.Unmarshal(body, &items); err != nil {
			return nil, nil, pageFailure(KindDecode, path, "page is not valid JSON")
		}
		return items, nil, nil
	case '{':
		var object map[string]json.RawMessage
		if err := json.Unmarshal(body, &object); err != nil {
			return nil, nil, pageFailure(KindDecode, path, "page is not valid JSON")
		}
		var total *int
		if raw, ok := object["total_count"]; ok {
			var parsed int
			if err := json.Unmarshal(raw, &parsed); err != nil {
				return nil, nil, pageFailure(KindContract, path, "total_count is not an integer")
			}
			total = &parsed
		}
		names := make([]string, 0, 3)
		if itemsField != "" {
			names = append(names, itemsField)
		}
		names = append(names, "items", "repositories")
		for _, name := range names {
			raw, ok := object[name]
			if !ok {
				continue
			}
			var items []json.RawMessage
			if err := json.Unmarshal(raw, &items); err != nil {
				return nil, nil, pageFailure(KindContract, path, fmt.Sprintf("page field %q is not an array", name))
			}
			return items, total, nil
		}
		return nil, nil, pageFailure(KindContract, path, "page object carries no items array")
	default:
		return nil, nil, pageFailure(KindDecode, path, "page is not valid JSON")
	}
}

// canonicalJSON re-encodes a page with sorted keys so two pages with the same
// content but different key order share one fingerprint.
func canonicalJSON(body []byte) []byte {
	var value any
	if err := json.Unmarshal(body, &value); err != nil {
		return body
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return body
	}
	return encoded
}

func pageFailure(kind ErrorKind, path, detail string) *Error {
	return &Error{
		Kind:     kind,
		Method:   http.MethodGet,
		Endpoint: RedactEndpoint(path),
		Detail:   detail,
	}
}

// sliceTarget validates the output argument and returns both the pointer and
// its element type.
func sliceTarget(out any) (reflect.Value, reflect.Type, error) {
	value := reflect.ValueOf(out)
	if !value.IsValid() || value.Kind() != reflect.Pointer || value.IsNil() {
		return reflect.Value{}, nil, &Error{
			Kind:   KindContract,
			Detail: "pagination target must be a non-nil pointer to a slice",
		}
	}
	elem := value.Elem()
	if elem.Kind() != reflect.Slice {
		return reflect.Value{}, nil, &Error{
			Kind:   KindContract,
			Detail: "pagination target must point to a slice",
		}
	}
	return value, elem.Type().Elem(), nil
}
