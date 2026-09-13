package github

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// graphqlServer builds a handler that records the cursor each page was asked
// for and replies with the scripted page for that cursor.
func graphqlServer(t *testing.T, pages map[string]string) (http.HandlerFunc, *[]string) {
	t.Helper()
	seen := &[]string{}
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if accept := r.Header.Get("Accept"); accept != graphqlAccept {
			t.Errorf("Accept = %q, want %q", accept, graphqlAccept)
		}
		var body struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decoding request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if strings.TrimSpace(body.Query) == "" {
			t.Errorf("query is empty")
		}
		cursor, _ := body.Variables["endCursor"].(string)
		*seen = append(*seen, cursor)
		page, ok := pages[cursor]
		if !ok {
			t.Errorf("unexpected cursor %q", cursor)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = io.WriteString(w, page)
	}, seen
}

func commitsPage(nodes string, hasNext bool, endCursor string, totalCount *int) string {
	total := "null"
	if totalCount != nil {
		total = strconv.Itoa(*totalCount)
	}
	return `{"data":{"repository":{"commits":{` +
		`"totalCount":` + total + `,` +
		`"nodes":[` + nodes + `],` +
		`"pageInfo":{"hasNextPage":` + boolText(hasNext) + `,"endCursor":"` + endCursor + `"}` +
		`}}}}`
}

func boolText(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

func testCommitsPagination() GraphQLPagination {
	return GraphQLPagination{
		Query:          "query($endCursor: String) { repository { commits(first: 2, after: $endCursor) { totalCount nodes { oid } pageInfo { hasNextPage endCursor } } } }",
		ConnectionPath: []string{"repository", "commits"},
	}
}

// AC4 baseline: a two-page walk collects every node and passes the cursor
// through the declared variable.
func TestPaginateGraphQLWalksTheConnection(t *testing.T) {
	count := 3
	handler, seen := graphqlServer(t, map[string]string{
		"":   commitsPage(`{"oid":"a"},{"oid":"b"}`, true, "c1", &count),
		"c1": commitsPage(`{"oid":"c"}`, false, "", &count),
	})
	client := testClient(t, handler)

	result, err := client.PaginateGraphQL(context.Background(), testCommitsPagination())
	if err != nil {
		t.Fatalf("PaginateGraphQL: %v", err)
	}
	if result.Pages != 2 {
		t.Errorf("Pages = %d, want 2", result.Pages)
	}
	if len(result.Nodes) != 3 {
		t.Fatalf("Nodes = %d, want 3", len(result.Nodes))
	}
	if result.TotalCount == nil || *result.TotalCount != 3 {
		t.Errorf("TotalCount = %v, want 3", result.TotalCount)
	}
	if len(*seen) != 2 || (*seen)[0] != "" || (*seen)[1] != "c1" {
		t.Errorf("cursors = %q, want [\"\" \"c1\"]", *seen)
	}
}

// AC4: a cursor that repeats would loop forever, so it is an incomplete walk.
func TestPaginateGraphQLRejectsARepeatedCursor(t *testing.T) {
	handler, _ := graphqlServer(t, map[string]string{
		"":   commitsPage(`{"oid":"a"}`, true, "c1", nil),
		"c1": commitsPage(`{"oid":"b"}`, true, "c1", nil),
	})
	client := testClient(t, handler)

	result, err := client.PaginateGraphQL(context.Background(), testCommitsPagination())
	if kind, ok := KindOf(err); !ok || kind != KindIncomplete {
		t.Fatalf("kind = %q (typed=%v), want %q; err = %v", kind, ok, KindIncomplete, err)
	}
	if len(result.Nodes) != 0 {
		t.Errorf("a failed walk must return no nodes, got %d", len(result.Nodes))
	}
	assertRedacted(t, err)
}

// AC4: hasNextPage without a cursor is a contradiction, not a next page.
func TestPaginateGraphQLRejectsAMissingCursor(t *testing.T) {
	handler, _ := graphqlServer(t, map[string]string{
		"": commitsPage(`{"oid":"a"}`, true, "", nil),
	})
	client := testClient(t, handler)

	if _, err := client.PaginateGraphQL(context.Background(), testCommitsPagination()); !IsKind(err, KindIncomplete) {
		t.Fatalf("err = %v, want an incomplete error", err)
	}
}

// AC4: "no next page" while totalCount still promises nodes is inconsistent.
func TestPaginateGraphQLRejectsInconsistentHasNextPage(t *testing.T) {
	count := 5
	handler, _ := graphqlServer(t, map[string]string{
		"": commitsPage(`{"oid":"a"}`, false, "", &count),
	})
	client := testClient(t, handler)

	_, err := client.PaginateGraphQL(context.Background(), testCommitsPagination())
	if kind, ok := KindOf(err); !ok || kind != KindIncomplete {
		t.Fatalf("kind = %q (typed=%v), want %q; err = %v", kind, ok, KindIncomplete, err)
	}
	if !strings.Contains(err.Error(), "hasNextPage") {
		t.Errorf("error should name the inconsistency: %v", err)
	}
}

// AC4: a non-empty errors array fails the call and never echoes the message.
func TestGraphQLErrorsFailTheCallWithoutEchoingText(t *testing.T) {
	client := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"data":null,"errors":[{"message":"boom","type":"FORBIDDEN","path":["repository"]}]}`)
	}))

	var out map[string]any
	err := client.GraphQL(context.Background(), "query { viewer { login } }", nil, &out)
	typed, ok := As(err)
	if !ok {
		t.Fatalf("error is not typed: %v", err)
	}
	if typed.Kind != KindContract {
		t.Errorf("Kind = %q, want %q", typed.Kind, KindContract)
	}
	if typed.Category != CategoryGraphQL {
		t.Errorf("Category = %q, want %q", typed.Category, CategoryGraphQL)
	}
	if !strings.Contains(typed.Detail, "1 error") {
		t.Errorf("detail = %q, want it to count the errors", typed.Detail)
	}
	assertRedacted(t, err)
}

// The envelope is trusted only when it has data and no errors.
func TestGraphQLDecodesData(t *testing.T) {
	client := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"data":{"viewer":{"login":"octocat"}}}`)
	}))

	var out struct {
		Viewer struct {
			Login string `json:"login"`
		} `json:"viewer"`
	}
	if err := client.GraphQL(context.Background(), "query { viewer { login } }", nil, &out); err != nil {
		t.Fatalf("GraphQL: %v", err)
	}
	if out.Viewer.Login != "octocat" {
		t.Errorf("login = %q, want %q", out.Viewer.Login, "octocat")
	}
}

// A response whose data does not match the requested shape is a decode error,
// which keeps "our query is wrong" distinguishable from "the answer is
// incomplete".
func TestGraphQLDataShapeMismatchIsADecodeError(t *testing.T) {
	client := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"data":{"viewer":"not an object"}}`)
	}))

	var out struct {
		Viewer struct {
			Login string `json:"login"`
		} `json:"viewer"`
	}
	if err := client.GraphQL(context.Background(), "query { viewer { login } }", nil, &out); !IsKind(err, KindDecode) {
		t.Fatalf("err = %v, want a decode error", err)
	}
}

// A missing connection on the path is a contract failure, not an empty result:
// silently treating it as "no commits" is how a ledger loses work.
func TestPaginateGraphQLRejectsAMissingConnection(t *testing.T) {
	client := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"data":{"repository":{}}}`)
	}))

	if _, err := client.PaginateGraphQL(context.Background(), testCommitsPagination()); !IsKind(err, KindContract) {
		t.Fatalf("err = %v, want a contract error", err)
	}
}
