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
		for _, part := range strings.Split(value, ",") {
			fields := strings.Split(part, ";")
			target := strings.TrimSpace(fields[0])
			if !strings.HasPrefix(target, "<") || !strings.HasSuffix(target, ">") {
				continue
			}
			for _, field := range fields[1:] {
				field = strings.TrimSpace(field)
				if !strings.HasPrefix(field, "rel=") {
					continue
				}
				if strings.Trim(strings.TrimPrefix(field, "rel="), `"`) == "next" {
					return strings.Trim(target, "<>")
				}
			}
		}
	}
	return ""
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
