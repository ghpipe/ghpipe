package github

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"testing"
)

type issue struct {
	Number int `json:"number"`
}

// linkNext renders the Link header the contract is supposed to follow.
func linkNext(next, last string) string {
	return fmt.Sprintf(`<%s>; rel="next", <%s>; rel="last"`, next, last)
}

// AC1: two pages are concatenated into one result.
func TestPaginateFollowsLinkHeaderAcrossPages(t *testing.T) {
	client, transport := newTestClient(t, servedBy(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("page") {
		case "", "1":
			w.Header().Set("Link", linkNext(testBaseURL+"/issues?page=2&per_page=100", testBaseURL+"/issues?page=2&per_page=100"))
			_, _ = io.WriteString(w, `[{"number":1},{"number":2}]`)
		case "2":
			_, _ = io.WriteString(w, `[{"number":3},{"number":4},{"number":5}]`)
		default:
			t.Errorf("unexpected page %q", r.URL.Query().Get("page"))
		}
	})))

	var got []issue
	if err := client.Paginate(context.Background(), "/issues", &got, PaginateOptions{}); err != nil {
		t.Fatalf("Paginate: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("collected %d issues, want 5: %+v", len(got), got)
	}
	for i, item := range got {
		if item.Number != i+1 {
			t.Errorf("item %d = %+v, want number %d", i, item, i+1)
		}
	}
	if requests := transport.calls(); requests != 2 {
		t.Errorf("transport saw %d requests, want 2: the Link header drives the walk", requests)
	}
}

// AC1: a short page that advertises a next page keeps the walk going - page
// size is never treated as proof of the end.
func TestPaginateContinuesAfterAShortPage(t *testing.T) {
	client, transport := newTestClient(t, servedBy(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("page") {
		case "", "1":
			w.Header().Set("Link", linkNext(testBaseURL+"/issues?page=2", testBaseURL+"/issues?page=2"))
			_, _ = io.WriteString(w, `[{"number":1}]`)
		case "2":
			_, _ = io.WriteString(w, `[{"number":2}]`)
		default:
			t.Errorf("unexpected page %q", r.URL.Query().Get("page"))
		}
	})))

	var got []issue
	if err := client.Paginate(context.Background(), "/issues", &got, PaginateOptions{}); err != nil {
		t.Fatalf("Paginate: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("collected %d issues, want 2: %+v", len(got), got)
	}
	if requests := transport.calls(); requests != 2 {
		t.Errorf("transport saw %d requests, want 2: a short page is not proof of the end", requests)
	}
}

// AC1: a repeated page is a contract violation, and the caller must not
// receive the partial result collected so far.
func TestPaginateRejectsADuplicatePage(t *testing.T) {
	client := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		if page == "" {
			page = "1"
		}
		next := fmt.Sprintf("%s/issues?page=%s", testBaseURL, increment(page))
		w.Header().Set("Link", linkNext(next, next))
		_, _ = io.WriteString(w, `[{"number":1}]`)
	}))

	var got []issue
	err := client.Paginate(context.Background(), "/issues", &got, PaginateOptions{})
	if kind, ok := KindOf(err); !ok || kind != KindContract {
		t.Fatalf("kind = %q (typed=%v), want %q; err = %v", kind, ok, KindContract, err)
	}
	if !IsContract(err) {
		t.Errorf("a duplicate page must satisfy IsContract: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("a failed pagination must leave the target empty, got %+v", got)
	}
	assertRedacted(t, err)
}

// AC1: a page that reports more items than the walk collected is incomplete.
func TestPaginateRejectsATotalCountShortfall(t *testing.T) {
	client := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"total_count":5,"items":[{"number":1}]}`)
	}))

	var got []issue
	err := client.Paginate(context.Background(), "/search/issues", &got, PaginateOptions{})
	if kind, ok := KindOf(err); !ok || kind != KindIncomplete {
		t.Fatalf("kind = %q (typed=%v), want %q; err = %v", kind, ok, KindIncomplete, err)
	}
	if !IsContract(err) {
		t.Errorf("a total_count shortfall must satisfy IsContract: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("a failed pagination must leave the target empty, got %+v", got)
	}
}

// AC1: hitting the page budget is an incomplete result, not a quiet truncation.
func TestPaginateStopsAtThePageBudget(t *testing.T) {
	client, transport := newTestClient(t, servedBy(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		if page == "" {
			page = "1"
		}
		next := fmt.Sprintf("%s/issues?page=%s", testBaseURL, increment(page))
		w.Header().Set("Link", linkNext(next, next))
		// Every page differs, so the duplicate-page guard cannot fire first.
		_, _ = fmt.Fprintf(w, `[{"number":%s}]`, page)
	})))

	var got []issue
	err := client.Paginate(context.Background(), "/issues", &got, PaginateOptions{MaxPages: 2})
	if kind, ok := KindOf(err); !ok || kind != KindIncomplete {
		t.Fatalf("kind = %q (typed=%v), want %q; err = %v", kind, ok, KindIncomplete, err)
	}
	if len(got) != 0 {
		t.Errorf("a failed pagination must leave the target empty, got %+v", got)
	}
	if requests := transport.calls(); requests != 2 {
		t.Errorf("transport saw %d requests, want 2: the budget stops the walk", requests)
	}
}

// The request side of the contract: per_page is pinned and the caller's page
// selection is dropped, so the Link header stays authoritative.
func TestPaginatePinsPerPageAndDropsCallerPaging(t *testing.T) {
	var received url.Values
	client := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received = r.URL.Query()
		_, _ = io.WriteString(w, `[]`)
	}))

	var got []issue
	if err := client.Paginate(context.Background(), "/issues?page=9&per_page=5&state=open", &got, PaginateOptions{}); err != nil {
		t.Fatalf("Paginate: %v", err)
	}
	if value := received.Get("per_page"); value != "100" {
		t.Errorf("per_page = %q, want %q", value, "100")
	}
	if value := received.Get("page"); value != "" {
		t.Errorf("page = %q, want it to be dropped", value)
	}
	if value := received.Get("state"); value != "open" {
		t.Errorf("state = %q, want the caller's other filters to survive", value)
	}
}

// A caller passing something that cannot hold the results gets a precondition
// failure rather than a panic.
func TestPaginateRejectsANonSliceTarget(t *testing.T) {
	client := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `[]`)
	}))

	if err := client.Paginate(context.Background(), "/issues", nil, PaginateOptions{}); !IsKind(err, KindContract) {
		t.Fatalf("err = %v, want a contract error for a nil target", err)
	}
	var target issue
	if err := client.Paginate(context.Background(), "/issues", &target, PaginateOptions{}); !IsKind(err, KindContract) {
		t.Fatalf("err = %v, want a contract error for a non-slice target", err)
	}
}

// An empty first page with no Link header is a legitimate empty list.
func TestPaginateAcceptsAnEmptyList(t *testing.T) {
	client := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `[]`)
	}))

	got := []issue{{Number: 99}}
	if err := client.Paginate(context.Background(), "/issues", &got, PaginateOptions{}); err != nil {
		t.Fatalf("Paginate: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got = %+v, want an empty result", got)
	}
}

// increment is a tiny helper so the fake server can advertise the next page.
func increment(page string) string {
	n := 0
	_, _ = fmt.Sscanf(page, "%d", &n)
	return fmt.Sprintf("%d", n+1)
}
