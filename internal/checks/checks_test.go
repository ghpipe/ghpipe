package checks

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/ghpipe/ghpipe/internal/github"
)

const testSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const otherSHA = "dddddddddddddddddddddddddddddddddddddddd"

// fakeTransport injects the REST and GraphQL answers. No test binds a port
// (docs/design.md 14.4).
type fakeTransport struct {
	mu      sync.Mutex
	calls   []string
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

// api is a small scripted GitHub: rules on the REST side, the commit and its
// rollup on the GraphQL side.
type api struct {
	transport *fakeTransport
	rules     string
	rulesCode int
	commitOID string
	noRollup  bool
	contexts  []map[string]any
}

func (a *api) serve() {
	a.transport = &fakeTransport{}
	a.transport.handler = func(req *http.Request, body string) (int, string, http.Header) {
		switch {
		case strings.Contains(req.URL.Path, "/rules/branches/"):
			code := a.rulesCode
			if code == 0 {
				code = http.StatusOK
			}
			return code, a.rules, nil
		case req.Method == http.MethodPost && req.URL.Path == "/graphql":
			var payload struct {
				Query string `json:"query"`
			}
			_ = json.Unmarshal([]byte(body), &payload)
			if strings.Contains(payload.Query, "after: $endCursor") {
				return http.StatusOK, `{"data":` + a.rollupData() + `}`, nil
			}
			return http.StatusOK, `{"data":` + a.commitData() + `}`, nil
		default:
			return http.StatusNotFound, `{"message":"Not Found"}`, nil
		}
	}
}

func (a *api) commitData() string {
	oid := a.commitOID
	if oid == "" {
		oid = testSHA
	}
	if a.noRollup {
		return fmt.Sprintf(`{"repository":{"object":{"__typename":"Commit","oid":%q,"statusCheckRollup":null}}}`, oid)
	}
	return fmt.Sprintf(`{"repository":{"object":{"__typename":"Commit","oid":%q,"statusCheckRollup":{"contexts":{"totalCount":%d}}}}}`, oid, len(a.contexts))
}

func (a *api) rollupData() string {
	encoded, _ := json.Marshal(a.contexts)
	return fmt.Sprintf(`{"repository":{"object":{"statusCheckRollup":{"contexts":{"totalCount":%d,"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":%s}}}}}`,
		len(a.contexts), encoded)
}

func (a *api) evaluate(t *testing.T, required []Required) (Evaluation, error) {
	t.Helper()
	a.serve()
	return Evaluate(context.Background(), a.transport.client(), Request{
		Repository: "ghpipe/ghpipe",
		SHA:        testSHA,
		Branch:     "main",
		Required:   required,
	})
}

func rulesWith(context string, integrationID int) string {
	return fmt.Sprintf(`[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":%q,"integration_id":%d}]}}]`,
		context, integrationID)
}

func checkRun(name, status, conclusion, oid string) map[string]any {
	return map[string]any{
		"__typename": "CheckRun",
		"name":       name,
		"status":     status,
		"conclusion": conclusion,
		"checkSuite": map[string]any{
			"commit": map[string]any{"oid": oid},
			"app":    map[string]any{"databaseId": 15368},
		},
	}
}

func statusContext(context, state string) map[string]any {
	return map[string]any{
		"__typename": "StatusContext",
		"context":    context,
		"state":      state,
		"creator":    map[string]any{"login": "ghpipe-delivery[bot]"},
	}
}

func TestUniqueRejectsUnusableConfiguration(t *testing.T) {
	cases := map[string][]Required{
		"empty context": {{Context: "", IntegrationID: 1}},
		"zero source":   {{Context: "ci", IntegrationID: 0}},
		"duplicate":     {{Context: "ci", IntegrationID: 1}, {Context: "ci", IntegrationID: 2}},
	}
	for name, required := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Unique(required); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
	unique, err := Unique([]Required{{Context: "b", IntegrationID: 2}, {Context: "a", IntegrationID: 1}})
	if err != nil {
		t.Fatalf("Unique: %v", err)
	}
	if unique[0].Context != "a" {
		t.Errorf("Unique is not ordered: %+v", unique)
	}
}

func TestEvaluateSatisfiedCheck(t *testing.T) {
	a := &api{
		rules: rulesWith("ghpipe-quality", 15368),
		contexts: []map[string]any{
			checkRun("ghpipe-quality", "COMPLETED", "SUCCESS", testSHA),
		},
	}
	ev, err := a.evaluate(t, []Required{{Context: "ghpipe-quality", IntegrationID: 15368}})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !ev.RulesRead {
		t.Error("RulesRead = false, want true")
	}
	if ev.State() != StateReady {
		t.Fatalf("state = %s, want ready (%+v)", ev.State(), ev.Items)
	}
	item := ev.Items[0]
	if item.Conclusion != "success" {
		t.Errorf("conclusion = %q, want the raw conclusion", item.Conclusion)
	}
	if len(item.Sources) != 1 || item.Sources[0] != "check_run" {
		t.Errorf("sources = %v, want [check_run]", item.Sources)
	}
}

func TestEvaluateWrongSourceIsFailed(t *testing.T) {
	a := &api{
		// The rules require the context from another integration: the same name
		// from a different app is a different check (docs/design.md 2.4).
		rules: rulesWith("ghpipe-quality", 99999),
		contexts: []map[string]any{
			checkRun("ghpipe-quality", "COMPLETED", "SUCCESS", testSHA),
		},
	}
	ev, err := a.evaluate(t, []Required{{Context: "ghpipe-quality", IntegrationID: 15368}})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if ev.State() != StateFailed {
		t.Fatalf("state = %s, want failed (%+v)", ev.State(), ev.Items)
	}
	if ev.Items[0].Detail != "source_binding_mismatch" {
		t.Errorf("detail = %q, want source_binding_mismatch", ev.Items[0].Detail)
	}
}

func TestEvaluateUnreadableRulesIsUnknown(t *testing.T) {
	a := &api{rulesCode: http.StatusInternalServerError}
	ev, err := a.evaluate(t, []Required{{Context: "ghpipe-quality", IntegrationID: 15368}})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if ev.RulesRead {
		t.Error("RulesRead = true, want false")
	}
	if ev.State() != StateUnknown {
		t.Fatalf("state = %s, want unknown", ev.State())
	}
	for _, call := range a.transport.requested() {
		if strings.Contains(call, "/graphql") {
			t.Errorf("the rollup was read although the rules were unreadable: %v", a.transport.requested())
		}
	}
}

func TestEvaluateUndeclaredRequirementIsUnknown(t *testing.T) {
	a := &api{
		rules:    rulesWith("some-other-check", 15368),
		contexts: []map[string]any{checkRun("ghpipe-quality", "COMPLETED", "SUCCESS", testSHA)},
	}
	ev, err := a.evaluate(t, []Required{{Context: "ghpipe-quality", IntegrationID: 15368}})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if ev.State() != StateUnknown {
		t.Fatalf("state = %s, want unknown", ev.State())
	}
}

func TestEvaluateRollupOIDMismatchIsContractError(t *testing.T) {
	a := &api{
		rules:     rulesWith("ghpipe-quality", 15368),
		commitOID: otherSHA,
		contexts:  []map[string]any{checkRun("ghpipe-quality", "COMPLETED", "SUCCESS", testSHA)},
	}
	ev, err := a.evaluate(t, []Required{{Context: "ghpipe-quality", IntegrationID: 15368}})
	if !github.IsKind(err, github.KindContract) {
		t.Fatalf("err = %v, want a contract error", err)
	}
	if ev.State() != StateUnknown {
		t.Errorf("state = %s, want unknown when the rollup is not trustworthy", ev.State())
	}
}

func TestEvaluateCheckRunFromAnotherCommitIsContractError(t *testing.T) {
	a := &api{
		rules:    rulesWith("ghpipe-quality", 15368),
		contexts: []map[string]any{checkRun("ghpipe-quality", "COMPLETED", "SUCCESS", otherSHA)},
	}
	_, err := a.evaluate(t, []Required{{Context: "ghpipe-quality", IntegrationID: 15368}})
	if !github.IsKind(err, github.KindContract) {
		t.Fatalf("err = %v, want a contract error", err)
	}
}

func TestEvaluateEverySourceWithTheSameNameMustPass(t *testing.T) {
	cases := []struct {
		name     string
		contexts []map[string]any
		want     State
	}{
		{
			name: "check run and commit status both succeed",
			contexts: []map[string]any{
				checkRun("ghpipe-quality", "COMPLETED", "SUCCESS", testSHA),
				statusContext("ghpipe-quality", "SUCCESS"),
			},
			want: StateReady,
		},
		{
			name: "a green check run does not excuse a red commit status",
			contexts: []map[string]any{
				checkRun("ghpipe-quality", "COMPLETED", "SUCCESS", testSHA),
				statusContext("ghpipe-quality", "FAILURE"),
			},
			want: StateFailed,
		},
		{
			name: "a pending source keeps the check pending",
			contexts: []map[string]any{
				checkRun("ghpipe-quality", "COMPLETED", "SUCCESS", testSHA),
				statusContext("ghpipe-quality", "PENDING"),
			},
			want: StatePending,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := &api{rules: rulesWith("ghpipe-quality", 15368), contexts: tc.contexts}
			ev, err := a.evaluate(t, []Required{{Context: "ghpipe-quality", IntegrationID: 15368}})
			if err != nil {
				t.Fatalf("Evaluate: %v", err)
			}
			if ev.State() != tc.want {
				t.Fatalf("state = %s, want %s (%+v)", ev.State(), tc.want, ev.Items)
			}
			if tc.want != StateReady {
				return
			}
			if len(ev.Items[0].Sources) != 2 {
				t.Errorf("sources = %v, want both sources", ev.Items[0].Sources)
			}
		})
	}
}

func TestEvaluateSkippedKeepsItsRawConclusion(t *testing.T) {
	a := &api{
		rules:    rulesWith("ghpipe-quality", 15368),
		contexts: []map[string]any{checkRun("ghpipe-quality", "COMPLETED", "SKIPPED", testSHA)},
	}
	ev, err := a.evaluate(t, []Required{{Context: "ghpipe-quality", IntegrationID: 15368}})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	item := ev.Items[0]
	if item.Conclusion != "skipped" {
		t.Errorf("conclusion = %q, want skipped (the raw conclusion, not a verdict)", item.Conclusion)
	}
	if item.State == StateReady {
		t.Log("skipped is an acceptable conclusion, so the state is ready while the raw conclusion stays visible")
	}
}

func TestEvaluateFailureAndPendingStates(t *testing.T) {
	cases := []struct {
		name     string
		contexts []map[string]any
		want     State
		detail   string
	}{
		{"failure", []map[string]any{checkRun("ghpipe-quality", "COMPLETED", "FAILURE", testSHA)}, StateFailed, ""},
		{"timed out", []map[string]any{checkRun("ghpipe-quality", "COMPLETED", "TIMED_OUT", testSHA)}, StateFailed, ""},
		{"in progress", []map[string]any{checkRun("ghpipe-quality", "IN_PROGRESS", "", testSHA)}, StatePending, ""},
		{"nothing reported", nil, StatePending, "no result was reported for this commit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := &api{rules: rulesWith("ghpipe-quality", 15368), contexts: tc.contexts}
			ev, err := a.evaluate(t, []Required{{Context: "ghpipe-quality", IntegrationID: 15368}})
			if err != nil {
				t.Fatalf("Evaluate: %v", err)
			}
			if ev.State() != tc.want {
				t.Fatalf("state = %s, want %s (%+v)", ev.State(), tc.want, ev.Items)
			}
			if tc.detail != "" && ev.Items[0].Detail != tc.detail {
				t.Errorf("detail = %q, want %q", ev.Items[0].Detail, tc.detail)
			}
		})
	}
}

func TestEvaluateCommitWithoutARollupIsPending(t *testing.T) {
	a := &api{rules: rulesWith("ghpipe-quality", 15368), noRollup: true}
	ev, err := a.evaluate(t, []Required{{Context: "ghpipe-quality", IntegrationID: 15368}})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if ev.State() != StatePending {
		t.Fatalf("state = %s, want pending", ev.State())
	}
}

func TestEvaluateRejectsBadRequests(t *testing.T) {
	transport := &fakeTransport{}
	c := transport.client()
	if _, err := Evaluate(context.Background(), c, Request{Repository: "ghpipe/ghpipe", SHA: "short", Branch: "main"}); err == nil {
		t.Error("expected a short SHA to be refused")
	}
	if _, err := Evaluate(context.Background(), c, Request{Repository: "ghpipe/ghpipe", SHA: testSHA, Branch: ""}); err == nil {
		t.Error("expected a missing branch to be refused")
	}
	if _, err := Evaluate(context.Background(), c, Request{Repository: "not-a-repo", SHA: testSHA, Branch: "main"}); err == nil {
		t.Error("expected a malformed repository to be refused")
	}
}

func TestWorstOrdersBySeverity(t *testing.T) {
	if Worst(StateReady, StatePending, StateUnknown, StateFailed) != StateFailed {
		t.Error("failed must win")
	}
	if Worst(StateReady, StatePending, StateUnknown) != StateUnknown {
		t.Error("unknown must outrank pending")
	}
	if Worst(StateReady, StatePending) != StatePending {
		t.Error("pending must outrank ready")
	}
	if Worst(StateReady) != StateReady {
		t.Error("ready must be the zero value")
	}
}
