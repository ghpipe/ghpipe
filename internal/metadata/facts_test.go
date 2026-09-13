package metadata

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/ghpipe/ghpipe/internal/github"
)

// fakeTransport is the injectable transport every remote test uses. No test in
// this repository binds a listening port: the sandbox forbids it and a real
// server would hide the request contract the tests are about
// (docs/design.md 14.4).
type fakeTransport struct {
	mu      sync.Mutex
	calls   []string
	bodies  []string
	handler func(req *http.Request, body string) (int, string, http.Header)
}

func (f *fakeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	body := ""
	if req.Body != nil {
		raw, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		body = string(raw)
	}
	f.mu.Lock()
	f.calls = append(f.calls, req.Method+" "+req.URL.Path)
	f.bodies = append(f.bodies, body)
	handler := f.handler
	f.mu.Unlock()

	status, payload, header := http.StatusOK, "{}", http.Header{}
	if handler != nil {
		status, payload, header = handler(req, body)
	}
	if header == nil {
		header = http.Header{}
	}
	return &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d", status),
		Header:     header,
		Body:       io.NopCloser(strings.NewReader(payload)),
		Request:    req,
	}, nil
}

func (f *fakeTransport) client() *github.Client {
	return github.New(
		github.WithHTTPClient(&http.Client{Transport: f}),
		github.WithToken("test-token"),
	)
}

func (f *fakeTransport) requested() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// assertReadsOnly fails when a test transport saw anything that could write.
// GraphQL reads are POSTs to /graphql, so "no writes" is "GET, or a POST whose
// body carries no mutation" (docs/design.md 7.3).
func (f *fakeTransport) assertReadsOnly(t *testing.T) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, call := range f.calls {
		switch {
		case strings.HasPrefix(call, http.MethodGet+" "):
		case strings.HasPrefix(call, http.MethodPost+" /graphql"):
			if strings.Contains(strings.ToLower(f.bodies[i]), "mutation") {
				t.Errorf("call %d is a GraphQL mutation: %s", i, f.bodies[i])
			}
		default:
			t.Errorf("read-only command issued %q", call)
		}
	}
}

func graphQLQueryOf(body string) string {
	var payload struct {
		Query string `json:"query"`
	}
	_ = json.Unmarshal([]byte(body), &payload)
	return payload.Query
}

func graphQLResponse(data string) (int, string, http.Header) {
	return http.StatusOK, `{"data":` + data + `}`, nil
}

func labelNodes(labels []string) []map[string]string {
	nodes := make([]map[string]string, 0, len(labels))
	for _, label := range labels {
		nodes = append(nodes, map[string]string{"name": label})
	}
	return nodes
}

func issueFixture(number int, state string, labels []string, milestone string) string {
	data := map[string]any{
		"repository": map[string]any{
			"issue": map[string]any{
				"number": number,
				"title":  "task",
				"state":  state,
				"body":   "body",
				"labels": map[string]any{"totalCount": len(labels), "nodes": labelNodes(labels)},
			},
		},
	}
	if milestone != "" {
		data["repository"].(map[string]any)["issue"].(map[string]any)["milestone"] = map[string]any{"title": milestone}
	}
	encoded, _ := json.Marshal(data)
	return string(encoded)
}

type prFixture struct {
	Number         int
	Repo           string
	Base           string
	State          string
	Draft          bool
	Merged         bool
	MergeCommit    string
	HeadRef        string
	HeadSHA        string
	Body           string
	Labels         []string
	Milestone      string
	ReviewDecision string
	MergeState     string
}

func (p prFixture) node() map[string]any {
	source := map[string]any{
		"__typename":  "PullRequest",
		"number":      p.Number,
		"body":        p.Body,
		"state":       orString(p.State, "OPEN"),
		"isDraft":     p.Draft,
		"merged":      p.Merged,
		"baseRefName": orString(p.Base, "main"),
		"headRefName": orString(p.HeadRef, fmt.Sprintf("ghpipe/issue-%d", p.Number)),
		"headRefOid":  p.HeadSHA,
		"repository":  map[string]any{"nameWithOwner": orString(p.Repo, "ghpipe/ghpipe")},
		"labels":      map[string]any{"totalCount": len(p.Labels), "nodes": labelNodes(p.Labels)},
	}
	if p.Merged {
		source["mergeCommit"] = map[string]any{"oid": p.MergeCommit}
	}
	if p.Milestone != "" {
		source["milestone"] = map[string]any{"title": p.Milestone}
	}
	if p.ReviewDecision != "" {
		source["reviewDecision"] = p.ReviewDecision
	}
	if p.MergeState != "" {
		source["mergeStateStatus"] = p.MergeState
	}
	return map[string]any{"__typename": "CrossReferencedEvent", "source": source}
}

func timelineFixture(nodes []map[string]any) string {
	items := make([]any, 0, len(nodes))
	for _, node := range nodes {
		items = append(items, node)
	}
	data := map[string]any{
		"repository": map[string]any{
			"issue": map[string]any{
				"timelineItems": map[string]any{
					"totalCount": len(items),
					"pageInfo":   map[string]any{"hasNextPage": false, "endCursor": nil},
					"nodes":      items,
				},
			},
		},
	}
	encoded, _ := json.Marshal(data)
	return string(encoded)
}

func orString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

// collector bundles the fixtures one Collect test needs.
type collector struct {
	transport    *fakeTransport
	issue        string
	timeline     []string
	reviews      []string
	reviewLinks  []string
	branchStatus int
	pulls        map[string]string
	requests     []string
}

func (c *collector) serve() {
	page := 0
	reviewPage := 0
	c.transport = &fakeTransport{}
	c.transport.handler = func(req *http.Request, body string) (int, string, http.Header) {
		c.requests = append(c.requests, req.Method+" "+req.URL.Path)
		switch {
		case req.Method == http.MethodPost && req.URL.Path == "/graphql":
			query := graphQLQueryOf(body)
			switch {
			case strings.Contains(query, "timelineItems"):
				if page >= len(c.timeline) {
					return http.StatusInternalServerError, `{}`, nil
				}
				payload := c.timeline[page]
				page++
				return graphQLResponse(payload)
			default:
				return graphQLResponse(c.issue)
			}
		case strings.HasSuffix(req.URL.Path, "/reviews"):
			if reviewPage >= len(c.reviews) {
				return http.StatusInternalServerError, `{}`, nil
			}
			payload := c.reviews[reviewPage]
			header := http.Header{}
			if reviewPage < len(c.reviewLinks) && c.reviewLinks[reviewPage] != "" {
				header.Set("Link", c.reviewLinks[reviewPage])
			}
			reviewPage++
			return http.StatusOK, payload, header
		case strings.Contains(req.URL.Path, "/git/ref/heads/"):
			if c.branchStatus == http.StatusNotFound {
				return http.StatusNotFound, `{"message":"Not Found"}`, nil
			}
			return http.StatusOK, `{"object":{"sha":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}}`, nil
		case strings.Contains(req.URL.Path, "/pulls/"):
			if payload, ok := c.pulls[req.URL.Path]; ok {
				return http.StatusOK, payload, nil
			}
			return http.StatusNotFound, `{"message":"Not Found"}`, nil
		case req.URL.Path == "/repos/ghpipe/ghpipe":
			return http.StatusOK, `{"default_branch":"main"}`, nil
		default:
			return http.StatusNotFound, `{"message":"Not Found"}`, nil
		}
	}
}

func (c *collector) collect(t *testing.T, opts Options) (Facts, error) {
	t.Helper()
	c.serve()
	if opts.Repository == "" {
		opts.Repository = "ghpipe/ghpipe"
	}
	return Collect(context.Background(), c.transport.client(), opts)
}

const headSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestCollectAssociatesTheSingleStandaloneClosingPR(t *testing.T) {
	c := &collector{
		issue: issueFixture(7, "OPEN", []string{"bug", "ghpipe:ready"}, "M1"),
		timeline: []string{timelineFixture([]map[string]any{
			// The task's pull request.
			prFixture{Number: 9, Body: "Closes #7", HeadSHA: headSHA, ReviewDecision: "APPROVED",
				MergeState: "CLEAN", Labels: []string{"ghpipe:ready"}}.node(),
			// A cross-reference from another repository: reported, never the task's PR.
			prFixture{Number: 10, Repo: "other/thing", Body: "Closes #7"}.node(),
			// A pull request against another branch is not a claim on the default branch.
			prFixture{Number: 11, Base: "release", Body: "Closes #7"}.node(),
			// A same-repository pull request that closes something else.
			prFixture{Number: 12, Body: "Fixes #8"}.node(),
		})},
		// Reviews are paginated: both pages must be read (AC: 分页必须完整).
		reviews: []string{
			`[{"id":1,"state":"COMMENTED","user":{"login":"rev"},"commit_id":"` + headSHA + `"}]`,
			`[{"id":2,"state":"CHANGES_REQUESTED","user":{"login":"rev"},"commit_id":"` + headSHA + `"},` +
				`{"id":3,"state":"APPROVED","user":{"login":"rev"},"commit_id":"` + headSHA + `"}]`,
		},
		reviewLinks: []string{
			`<https://api.github.com/repos/ghpipe/ghpipe/pulls/9/reviews?page=2>; rel="next"`,
			"",
		},
	}
	facts, err := c.collect(t, Options{Issue: 7})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if facts.DefaultBranch != "main" {
		t.Errorf("DefaultBranch = %q, want main", facts.DefaultBranch)
	}
	if len(facts.PRs) != 1 || facts.PRs[0].Number != 9 {
		t.Fatalf("associated PRs = %+v, want only #9", facts.PRs)
	}
	if len(facts.CrossRepoRefs) != 1 || facts.CrossRepoRefs[0] != 10 {
		t.Errorf("CrossRepoRefs = %v, want [10]", facts.CrossRepoRefs)
	}
	if len(facts.Reviews) != 3 {
		t.Errorf("reviews = %d, want 3 (both pages)", len(facts.Reviews))
	}
	if !facts.BranchExists {
		t.Error("BranchExists = false, want true")
	}
	if facts.Issue.Milestone != "M1" || facts.Issue.Number != 7 {
		t.Errorf("issue facts = %+v", facts.Issue)
	}
	// The association is decided by the body, so the raw review list must not
	// decide a stage: the effective verdict is the last one per author.
	effective := facts.EffectiveReviews()
	if len(effective) != 1 || effective[0].State != "APPROVED" {
		t.Errorf("effective reviews = %+v, want one APPROVED", effective)
	}
	c.transport.assertReadsOnly(t)
}

func TestCollectRefusesSecondClosingReference(t *testing.T) {
	c := &collector{
		issue: issueFixture(7, "OPEN", nil, ""),
		timeline: []string{timelineFixture([]map[string]any{
			prFixture{Number: 9, Body: "Closes #7\nFixes #8", HeadSHA: headSHA}.node(),
		})},
	}
	_, err := c.collect(t, Options{Issue: 7})
	if !errors.Is(err, ErrMultipleClosingReferences) {
		t.Fatalf("err = %v, want ErrMultipleClosingReferences", err)
	}
}

func TestCollectRefusesCrossRepositoryClosingReference(t *testing.T) {
	c := &collector{
		issue: issueFixture(7, "OPEN", nil, ""),
		timeline: []string{timelineFixture([]map[string]any{
			prFixture{Number: 9, Body: "Closes other/repo#7", HeadSHA: headSHA}.node(),
		})},
	}
	_, err := c.collect(t, Options{Issue: 7})
	if !errors.Is(err, ErrCrossRepoReference) {
		t.Fatalf("err = %v, want ErrCrossRepoReference", err)
	}
}

// TestCollectRefusesCrossLineSecondClosingReference is the contract-level
// regression test for the defect in issue #8. The body carries the standalone
// claim "Closes #7" and a second claim written across a line break,
// "Closes\n#7". Before the fix the separator accepted only horizontal
// whitespace, so the second reference was invisible: the body looked like
// exactly one closing reference and the association rule of docs/design.md 2.3
// ("出现第二个关闭引用即阻断") was bypassed. The second claim must refuse the
// body, wherever it is written - an issue timeline or a pull request body.
func TestCollectRefusesCrossLineSecondClosingReference(t *testing.T) {
	const body = "Closes #7\n\nCloses\n#7"

	owns, err := classifyClaim("ghpipe/ghpipe", 7, body)
	if !errors.Is(err, ErrMultipleClosingReferences) {
		t.Fatalf("classifyClaim(ghpipe/ghpipe, 7, %q) = (%v, %v), want ErrMultipleClosingReferences",
			body, owns, err)
	}

	c := &collector{
		issue: issueFixture(7, "OPEN", nil, ""),
		timeline: []string{timelineFixture([]map[string]any{
			prFixture{Number: 9, Body: body, HeadSHA: headSHA}.node(),
		})},
	}
	if _, err := c.collect(t, Options{Issue: 7}); !errors.Is(err, ErrMultipleClosingReferences) {
		t.Fatalf("Collect(Issue 7) with %q: err = %v, want ErrMultipleClosingReferences", body, err)
	}

	// The same grammar decides a pull request target, so the second call site
	// must refuse the same body.
	c = &collector{
		issue: issueFixture(7, "OPEN", nil, ""),
		pulls: map[string]string{
			"/repos/ghpipe/ghpipe/pulls/99": `{"body":` + strconv.Quote(body) + `,"head":{"sha":"` + headSHA + `"}}`,
		},
	}
	if _, err := c.collect(t, Options{PR: 99}); !errors.Is(err, ErrMultipleClosingReferences) {
		t.Fatalf("Collect(PR 99) with %q: err = %v, want ErrMultipleClosingReferences", body, err)
	}
}

// TestCollectRefusesURLClosingReferenceItCannotPlaceInThisRepository is the
// regression test for the defect in a571dd5. The change request's minimal
// reproduction was classifyClaim("ghpipe/ghpipe", 7, "Closes
// https://example.com/issues/7") returning (true, nil): the URL degraded to the
// unqualified "#7" reading, which means "this repository", so a pointer at
// something else was accepted as a claim on this issue's task pull request.
func TestCollectRefusesURLClosingReferenceItCannotPlaceInThisRepository(t *testing.T) {
	for _, body := range []string{
		"Closes https://example.com/issues/7",
		"Closes https://gitlab.com/foo/issues/7",
		"Closes https://github.com/issues/7",
	} {
		t.Run(body, func(t *testing.T) {
			owns, err := classifyClaim("ghpipe/ghpipe", 7, body)
			if !errors.Is(err, ErrCrossRepoReference) {
				t.Fatalf("classifyClaim(ghpipe/ghpipe, 7, %q) = (%v, %v), want ErrCrossRepoReference",
					body, owns, err)
			}
			c := &collector{
				issue: issueFixture(7, "OPEN", nil, ""),
				timeline: []string{timelineFixture([]map[string]any{
					prFixture{Number: 9, Body: body, HeadSHA: headSHA}.node(),
				})},
			}
			if _, err := c.collect(t, Options{Issue: 7}); !errors.Is(err, ErrCrossRepoReference) {
				t.Fatalf("Collect with %q: err = %v, want ErrCrossRepoReference", body, err)
			}
		})
	}

	// The same grammar decides a pull request target, so the second call site
	// must refuse the same bodies.
	for _, body := range []string{
		"Closes https://example.com/issues/7",
		"Closes https://gitlab.com/foo/issues/7",
	} {
		c := &collector{
			issue: issueFixture(7, "OPEN", nil, ""),
			pulls: map[string]string{
				"/repos/ghpipe/ghpipe/pulls/99": `{"body":` + strconv.Quote(body) + `,"head":{"sha":"` + headSHA + `"}}`,
			},
		}
		if _, err := c.collect(t, Options{PR: 99}); !errors.Is(err, ErrCrossRepoReference) {
			t.Fatalf("Collect(PR 99) with %q: err = %v, want ErrCrossRepoReference", body, err)
		}
	}
}

func TestCollectRefusesTwoActivePullRequests(t *testing.T) {
	c := &collector{
		issue: issueFixture(7, "OPEN", nil, ""),
		timeline: []string{timelineFixture([]map[string]any{
			prFixture{Number: 9, Body: "Closes #7", HeadSHA: headSHA}.node(),
			prFixture{Number: 10, Body: "Closes #7", HeadSHA: headSHA}.node(),
		})},
	}
	_, err := c.collect(t, Options{Issue: 7})
	if !errors.Is(err, ErrAmbiguousAssociation) {
		t.Fatalf("err = %v, want ErrAmbiguousAssociation", err)
	}
}

func TestCollectAllowsOneMergedAndOneActivePullRequest(t *testing.T) {
	c := &collector{
		issue: issueFixture(7, "OPEN", nil, ""),
		timeline: []string{timelineFixture([]map[string]any{
			prFixture{Number: 9, Body: "Closes #7", HeadSHA: headSHA, State: "CLOSED", Merged: true,
				MergeCommit: strings.Repeat("c", 40)}.node(),
			prFixture{Number: 10, Body: "Closes #7", HeadSHA: headSHA}.node(),
		})},
		reviews: []string{`[]`},
	}
	facts, err := c.collect(t, Options{Issue: 7})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if active := facts.ActivePR(); active == nil || active.Number != 10 {
		t.Errorf("ActivePR = %+v, want #10", active)
	}
	if merged := facts.MergedPR(); merged == nil || merged.Number != 9 {
		t.Errorf("MergedPR = %+v, want #9", merged)
	}
}

func TestCollectPaginationMustBeComplete(t *testing.T) {
	// hasNextPage is true but the cursor is empty: the walk cannot be proven
	// complete, so the facts must not be returned (docs/design.md 7.3).
	broken := `{"repository":{"issue":{"timelineItems":{"totalCount":2,
      "pageInfo":{"hasNextPage":true,"endCursor":null},
      "nodes":[]}}}}`
	c := &collector{issue: issueFixture(7, "OPEN", nil, ""), timeline: []string{broken}}
	_, err := c.collect(t, Options{Issue: 7})
	if !github.IsKind(err, github.KindIncomplete) {
		t.Fatalf("err = %v, want an incomplete read", err)
	}
}

func TestCollectBranchAbsentIsAnAnswer(t *testing.T) {
	c := &collector{
		issue:        issueFixture(7, "OPEN", nil, ""),
		timeline:     []string{timelineFixture(nil)},
		branchStatus: http.StatusNotFound,
	}
	facts, err := c.collect(t, Options{Issue: 7})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if facts.BranchExists {
		t.Error("BranchExists = true, want false for a 404")
	}
}

func TestCollectIssueNotFound(t *testing.T) {
	c := &collector{issue: `{"repository":{"issue":null}}`}
	_, err := c.collect(t, Options{Issue: 7})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestCollectPullRequestTargetMustBeAssociated(t *testing.T) {
	c := &collector{
		issue: issueFixture(7, "OPEN", nil, ""),
		timeline: []string{timelineFixture([]map[string]any{
			prFixture{Number: 9, Body: "Closes #7", HeadSHA: headSHA}.node(),
		})},
		pulls: map[string]string{
			"/repos/ghpipe/ghpipe/pulls/99": `{"body":"Closes #7","head":{"sha":"` + headSHA + `"}}`,
		},
	}
	_, err := c.collect(t, Options{PR: 99})
	if !errors.Is(err, ErrUnassociatedPR) {
		t.Fatalf("err = %v, want ErrUnassociatedPR", err)
	}
}

func TestCollectPullRequestTargetResolvesItsIssue(t *testing.T) {
	c := &collector{
		issue: issueFixture(7, "OPEN", nil, ""),
		timeline: []string{timelineFixture([]map[string]any{
			prFixture{Number: 9, Body: "Closes #7", HeadSHA: headSHA}.node(),
		})},
		reviews: []string{`[]`},
		pulls: map[string]string{
			"/repos/ghpipe/ghpipe/pulls/9": `{"body":"Closes #7","head":{"sha":"` + headSHA + `"}}`,
		},
	}
	facts, err := c.collect(t, Options{PR: 9})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if facts.Issue.Number != 7 || facts.TargetPR != 9 {
		t.Errorf("facts = issue #%d target pr #%d, want #7 / #9", facts.Issue.Number, facts.TargetPR)
	}
}

func TestCollectRequiresExactlyOneTarget(t *testing.T) {
	c := &collector{issue: issueFixture(7, "OPEN", nil, "")}
	if _, err := c.collect(t, Options{}); err == nil {
		t.Fatal("expected an error when neither issue nor pull request is given")
	}
	if _, err := c.collect(t, Options{Issue: 7, PR: 9}); err == nil {
		t.Fatal("expected an error when both targets are given")
	}
}

func TestCleanupDeclared(t *testing.T) {
	transport := &fakeTransport{}
	transport.handler = func(req *http.Request, body string) (int, string, http.Header) {
		return http.StatusOK, `[{"body":"nothing here"},{"body":"<!-- ghpipe:cleanup:9 --> done"}]`, nil
	}
	declared, err := CleanupDeclared(context.Background(), transport.client(), "ghpipe/ghpipe", 9)
	if err != nil {
		t.Fatalf("CleanupDeclared: %v", err)
	}
	if !declared {
		t.Error("declared = false, want true")
	}

	missing := &fakeTransport{}
	missing.handler = func(req *http.Request, body string) (int, string, http.Header) {
		return http.StatusOK, `[{"body":"<!-- ghpipe:cleanup:10 --> other pr"}]`, nil
	}
	declared, err = CleanupDeclared(context.Background(), missing.client(), "ghpipe/ghpipe", 9)
	if err != nil {
		t.Fatalf("CleanupDeclared: %v", err)
	}
	if declared {
		t.Error("declared = true for another pull request's declaration")
	}
}

func TestBranchHeadPropagatesRealFailures(t *testing.T) {
	transport := &fakeTransport{}
	transport.handler = func(req *http.Request, body string) (int, string, http.Header) {
		return http.StatusInternalServerError, `{}`, nil
	}
	if _, _, err := BranchHead(context.Background(), transport.client(), "ghpipe/ghpipe", "ghpipe/issue-7"); err == nil {
		t.Fatal("expected a 500 to be an error, not an absent branch")
	}
}
