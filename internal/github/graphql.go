package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// graphqlAccept is the media type GraphQL requires. It is not the REST media
// type, which is why Request carries a per-call Accept override.
const graphqlAccept = "application/json"

// graphqlPath is the single GraphQL endpoint.
const graphqlPath = "/graphql"

// GraphQLError is one entry of the response's errors array. Only the count is
// ever surfaced: message text is remote content.
type GraphQLError struct {
	Message string `json:"message"`
	Type    string `json:"type,omitempty"`
	Path    []any  `json:"path,omitempty"`
}

type graphQLPayload struct {
	Data   json.RawMessage `json:"data"`
	Errors []GraphQLError  `json:"errors"`
}

// GraphQL runs one query and decodes its data into out. A non-empty errors
// array fails the call; partial data is never returned.
func (c *Client) GraphQL(ctx context.Context, query string, variables map[string]any, out any) error {
	data, err := c.graphQLData(ctx, query, variables)
	if err != nil {
		return err
	}
	if out == nil || len(bytes.TrimSpace(data)) == 0 || string(bytes.TrimSpace(data)) == "null" {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return graphQLError(KindDecode, "GraphQL data does not match the requested shape")
	}
	return nil
}

// graphQLData performs the call and validates the envelope. Deciding what the
// data means is the caller's job; this layer only guarantees that the envelope
// is trustworthy.
func (c *Client) graphQLData(ctx context.Context, query string, variables map[string]any) (json.RawMessage, error) {
	if strings.TrimSpace(query) == "" {
		return nil, graphQLError(KindContract, "GraphQL query is empty")
	}
	body := map[string]any{"query": query}
	if variables != nil {
		body["variables"] = variables
	}
	resp, err := c.Do(ctx, Request{
		Method: http.MethodPost,
		Path:   graphqlPath,
		Body:   body,
		Accept: graphqlAccept,
	})
	if err != nil {
		return nil, err
	}
	trimmed := bytes.TrimSpace(resp.Body)
	if len(trimmed) == 0 {
		return nil, &Error{
			Kind:     KindContract,
			Method:   http.MethodPost,
			Endpoint: graphqlPath,
			Status:   resp.Status,
			Category: CategoryGraphQL,
			Detail:   "GraphQL response has no body",
		}
	}
	var payload graphQLPayload
	if err := json.Unmarshal(trimmed, &payload); err != nil {
		return nil, decodeError(http.MethodPost, graphqlPath, resp.Status)
	}
	if len(payload.Errors) > 0 {
		return nil, &Error{
			Kind:     KindContract,
			Method:   http.MethodPost,
			Endpoint: graphqlPath,
			Status:   resp.Status,
			Category: CategoryGraphQL,
			Detail:   fmt.Sprintf("GraphQL returned %d error(s)", len(payload.Errors)),
		}
	}
	if len(payload.Data) == 0 {
		return nil, graphQLError(KindContract, "GraphQL response carries no data field")
	}
	return payload.Data, nil
}

// GraphQLPagination describes one paginated connection. The query must take a
// single cursor variable so that "one connection per query" can be enforced by
// construction.
type GraphQLPagination struct {
	Query     string
	Variables map[string]any
	// ConnectionPath locates the connection inside data, for example
	// {"repository", "pullRequest", "commits"}.
	ConnectionPath []string
	// CursorVariable is the query variable the cursor is passed through.
	// Defaults to "endCursor".
	CursorVariable string
	// NodesField is the array field naming the connection's nodes. Defaults to
	// "nodes" ("edges" is not supported on purpose: edges hide the cursor
	// invariant this walker verifies).
	NodesField string
	// MaxPages overrides DefaultMaxPages when positive.
	MaxPages int
}

// GraphQLResult is a complete, verified connection walk.
type GraphQLResult struct {
	Nodes      []json.RawMessage
	Pages      int
	TotalCount *int
}

// PaginateGraphQL follows one GraphQL connection page by page.
//
// It fails, returning nothing, when the walk cannot be proven complete: a
// missing or repeated cursor, a page budget overrun, a totalCount that moves
// between pages, or a page claiming hasNextPage=false while totalCount says
// more nodes exist.
func (c *Client) PaginateGraphQL(ctx context.Context, p GraphQLPagination) (GraphQLResult, error) {
	var empty GraphQLResult
	if strings.TrimSpace(p.Query) == "" {
		return empty, graphQLError(KindContract, "GraphQL query is empty")
	}
	if len(p.ConnectionPath) == 0 {
		return empty, graphQLError(KindContract, "a connection path is required")
	}
	cursorVariable := p.CursorVariable
	if cursorVariable == "" {
		cursorVariable = "endCursor"
	}
	nodesField := p.NodesField
	if nodesField == "" {
		nodesField = "nodes"
	}
	maxPages := p.MaxPages
	if maxPages <= 0 {
		maxPages = DefaultMaxPages
	}

	var (
		result GraphQLResult
		cursor string
		seen   = make(map[string]bool)
	)
	for page := 1; ; page++ {
		variables := make(map[string]any, len(p.Variables)+1)
		for name, value := range p.Variables {
			variables[name] = value
		}
		if cursor == "" {
			variables[cursorVariable] = nil
		} else {
			variables[cursorVariable] = cursor
		}

		data, err := c.graphQLData(ctx, p.Query, variables)
		if err != nil {
			return empty, err
		}
		connection, err := decodeConnection(data, p.ConnectionPath, nodesField)
		if err != nil {
			return empty, err
		}

		result.Pages = page
		result.Nodes = append(result.Nodes, connection.Nodes...)
		if connection.TotalCount != nil {
			if result.TotalCount != nil && *result.TotalCount != *connection.TotalCount {
				return empty, graphQLError(KindContract, "totalCount changed between pages")
			}
			result.TotalCount = connection.TotalCount
		}

		if !connection.HasNextPage {
			if result.TotalCount != nil && len(result.Nodes) < *result.TotalCount {
				return empty, graphQLError(KindIncomplete, fmt.Sprintf(
					"hasNextPage is false but only %d of %d nodes were returned",
					len(result.Nodes), *result.TotalCount))
			}
			return result, nil
		}
		if connection.EndCursor == "" {
			return empty, graphQLError(KindIncomplete, "hasNextPage is true but endCursor is empty")
		}
		if seen[connection.EndCursor] {
			return empty, graphQLError(KindIncomplete, "endCursor repeats a cursor that is already in use")
		}
		seen[connection.EndCursor] = true
		cursor = connection.EndCursor
		if page >= maxPages {
			return empty, graphQLError(KindIncomplete, fmt.Sprintf(
				"stopped after the %d page budget with more pages advertised", maxPages))
		}
	}
}

type connectionPage struct {
	Nodes       []json.RawMessage
	HasNextPage bool
	EndCursor   string
	TotalCount  *int
}

// decodeConnection walks the configured path and validates the connection
// shape. A connection is only usable if pageInfo is present and says
// unambiguously whether another page follows.
func decodeConnection(data json.RawMessage, path []string, nodesField string) (connectionPage, error) {
	var page connectionPage
	current := data
	for _, key := range path {
		var object map[string]json.RawMessage
		if err := json.Unmarshal(current, &object); err != nil {
			return page, graphQLError(KindContract, fmt.Sprintf("GraphQL data has no %q: its parent is not an object", key))
		}
		next, ok := object[key]
		if !ok {
			return page, graphQLError(KindContract, fmt.Sprintf("GraphQL data has no field %q on the connection path", key))
		}
		current = next
	}
	if len(bytes.TrimSpace(current)) == 0 || string(bytes.TrimSpace(current)) == "null" {
		return page, graphQLError(KindContract, "the connection is null")
	}

	var object map[string]json.RawMessage
	if err := json.Unmarshal(current, &object); err != nil {
		return page, graphQLError(KindContract, "the connection is not an object")
	}
	if raw, ok := object[nodesField]; ok {
		if err := json.Unmarshal(raw, &page.Nodes); err != nil {
			return page, graphQLError(KindContract, fmt.Sprintf("connection field %q is not an array", nodesField))
		}
	}
	if raw, ok := object["totalCount"]; ok && string(bytes.TrimSpace(raw)) != "null" {
		var total int
		if err := json.Unmarshal(raw, &total); err != nil {
			return page, graphQLError(KindContract, "totalCount is not an integer")
		}
		page.TotalCount = &total
	}
	raw, ok := object["pageInfo"]
	if !ok {
		return page, graphQLError(KindContract, "the connection has no pageInfo")
	}
	var info struct {
		HasNextPage *bool   `json:"hasNextPage"`
		EndCursor   *string `json:"endCursor"`
	}
	if err := json.Unmarshal(raw, &info); err != nil {
		return page, graphQLError(KindContract, "pageInfo is not an object")
	}
	if info.HasNextPage == nil {
		return page, graphQLError(KindContract, "pageInfo is missing hasNextPage")
	}
	page.HasNextPage = *info.HasNextPage
	if info.EndCursor != nil {
		page.EndCursor = *info.EndCursor
	}
	return page, nil
}

func graphQLError(kind ErrorKind, detail string) *Error {
	return &Error{
		Kind:     kind,
		Method:   http.MethodPost,
		Endpoint: graphqlPath,
		Category: CategoryGraphQL,
		Detail:   detail,
	}
}
