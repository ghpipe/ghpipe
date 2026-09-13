package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ghpipe/ghpipe/internal/gitx"
	"github.com/ghpipe/ghpipe/internal/result"
)

const (
	headSHA  = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	localSHA = "cccccccccccccccccccccccccccccccccccccccc"
)

// fakeGit scripts the git Runner. The commands the status and doctor paths run
// are fixed, so a scripted answer is more honest than a real repository: it
// records exactly which questions were asked.
type fakeGit struct {
	mu        sync.Mutex
	asked     []string
	responses map[string]gitx.Command
}

func (f *fakeGit) Run(ctx context.Context, dir string, argv []string) (gitx.Command, error) {
	key := strings.Join(argv, " ")
	f.mu.Lock()
	f.asked = append(f.asked, key)
	f.mu.Unlock()
	if command, ok := f.responses[key]; ok {
		return command, nil
	}
	return gitx.Command{ExitCode: 1, Stderr: "not scripted"}, nil
}

func (f *fakeGit) questions() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.asked...)
}

func workingCheckout(dirty bool) *fakeGit {
	return &fakeGit{responses: map[string]gitx.Command{
		"symbolic-ref --quiet --short HEAD": {Stdout: "ghpipe/issue-7\n"},
		"rev-parse --verify HEAD":           {Stdout: localSHA + "\n"},
		"status --porcelain":                {Stdout: porcelain(dirty)},
		"rev-parse --is-shallow-repository": {Stdout: "false\n"},
		"rev-parse --verify --quiet refs/heads/ghpipe/issue-7^{commit}": {
			Stdout: localSHA + "\n",
		},
		"rev-parse --verify --quiet refs/remotes/origin/ghpipe/issue-7^{commit}": {
			Stdout: localSHA + "\n",
		},
	}}
}

func porcelain(dirty bool) string {
	if dirty {
		return " M main.go\n"
	}
	return ""
}

// fakeTransport records every request so a test can assert that a command only
// ever read.
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

func (f *fakeTransport) httpClient() *http.Client { return &http.Client{Transport: f} }

func (f *fakeTransport) requested() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// assertReadsOnly fails when a request could write. GraphQL reads are POSTs to
// /graphql carrying a query, so the rule is "GET, or a query-only POST".
func (f *fakeTransport) assertReadsOnly(t *testing.T) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, call := range f.calls {
		switch {
		case strings.HasPrefix(call, http.MethodGet+" "):
		case strings.HasPrefix(call, http.MethodPost+" /graphql"):
			if strings.Contains(strings.ToLower(f.bodies[i]), "mutation") {
				t.Errorf("call %d is a GraphQL mutation", i)
			}
		default:
			t.Errorf("read-only command issued %q", call)
		}
	}
}

// githubFixture answers the reads a healthy review-stage task needs.
func githubFixture() func(*http.Request, string) (int, string, http.Header) {
	return func(req *http.Request, body string) (int, string, http.Header) {
		switch {
		case req.URL.Path == "/repos/ghpipe/ghpipe":
			return http.StatusOK, `{"default_branch":"main"}`, nil
		case req.Method == http.MethodPost && req.URL.Path == "/graphql":
			var payload struct {
				Query string `json:"query"`
			}
			_ = json.Unmarshal([]byte(body), &payload)
			switch {
			case strings.Contains(payload.Query, "timelineItems"):
				return http.StatusOK, `{"data":` + timelineData + `}`, nil
			case strings.Contains(payload.Query, "after: $endCursor"):
				return http.StatusOK, `{"data":` + rollupData + `}`, nil
			case strings.Contains(payload.Query, "statusCheckRollup"):
				return http.StatusOK, `{"data":` + commitData + `}`, nil
			default:
				return http.StatusOK, `{"data":` + issueData + `}`, nil
			}
		case strings.Contains(req.URL.Path, "/rules/branches/"):
			return http.StatusOK, rulesData, nil
		case strings.HasSuffix(req.URL.Path, "/reviews"):
			return http.StatusOK, `[{"id":1,"state":"APPROVED","user":{"login":"rev"},"commit_id":"` + headSHA + `"}]`, nil
		case strings.Contains(req.URL.Path, "/git/ref/heads/"):
			return http.StatusOK, `{"object":{"sha":"` + headSHA + `"}}`, nil
		case strings.Contains(req.URL.Path, "/pulls/"):
			return http.StatusOK, `{"body":"Closes #7","head":{"sha":"` + headSHA + `"}}`, nil
		default:
			return http.StatusNotFound, `{"message":"Not Found"}`, nil
		}
	}
}

const issueData = `{"repository":{"issue":{"number":7,"title":"task","state":"OPEN","body":"",` +
	`"labels":{"totalCount":1,"nodes":[{"name":"ghpipe:review"}]}}}}`

const timelineData = `{"repository":{"issue":{"timelineItems":{"totalCount":1,` +
	`"pageInfo":{"hasNextPage":false,"endCursor":null},` +
	`"nodes":[{"__typename":"CrossReferencedEvent","source":{"__typename":"PullRequest",` +
	`"number":9,"body":"Closes #7","state":"OPEN","isDraft":false,"merged":false,` +
	`"baseRefName":"main","headRefName":"ghpipe/issue-7","headRefOid":"` + headSHA + `",` +
	`"reviewDecision":"APPROVED","mergeStateStatus":"CLEAN",` +
	`"labels":{"totalCount":1,"nodes":[{"name":"ghpipe:review"}]},` +
	`"repository":{"nameWithOwner":"ghpipe/ghpipe"}}}]}}}}`

const rulesData = `[{"type":"required_status_checks","parameters":{"required_status_checks":[` +
	`{"context":"ghpipe-quality","integration_id":15368}]}}]`

const commitData = `{"repository":{"object":{"__typename":"Commit",` +
	`"oid":"` + headSHA + `","statusCheckRollup":{"contexts":{"totalCount":1}}}}}`

const rollupData = `{"repository":{"object":{"statusCheckRollup":{"contexts":{"totalCount":1,` +
	`"pageInfo":{"hasNextPage":false,"endCursor":null},` +
	`"nodes":[{"__typename":"CheckRun","name":"ghpipe-quality","status":"COMPLETED",` +
	`"conclusion":"SUCCESS","checkSuite":{"commit":{"oid":"` + headSHA + `"},` +
	`"app":{"databaseId":15368}}}]}}}}}`

// project writes a minimal, valid project configuration and returns its root.
func testProject(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, ".ghpipe")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	config := `{
	  "schema_version": 1,
	  "repository": "ghpipe/ghpipe",
	  "default_branch": "main",
	  "quality": {"mode": "strict"},
	  "ci": {"required_checks": [{"context": "ghpipe-quality", "integration_id": 15368}]},
	  "commands": {"test": {"cwd": ".", "argv": ["go", "test", "-count=1", "./..."]}}
	}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(config), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return root
}

type runResult struct {
	code   int
	stdout string
	stderr string
}

func run(t *testing.T, deps Deps, args ...string) runResult {
	t.Helper()
	var out, errOut bytes.Buffer
	deps.Streams = Streams{Out: &out, Err: &errOut}
	code := RunWith(args, deps)
	return runResult{code: code, stdout: out.String(), stderr: errOut.String()}
}

// envelope is the decoded result envelope.
type envelope struct {
	SchemaVersion int    `json:"schema_version"`
	Command       string `json:"command"`
	Status        string `json:"status"`
	Repository    string `json:"repository"`
	Target        *struct {
		Kind   string `json:"kind"`
		Number int    `json:"number"`
	} `json:"target"`
	Data  json.RawMessage `json:"data"`
	Error *struct {
		Category string `json:"category"`
		Phase    string `json:"phase"`
		Detail   string `json:"detail"`
	} `json:"error"`
	Next       []string `json:"next"`
	ObservedAt string   `json:"observed_at"`
}

func decodeEnvelope(t *testing.T, out string) envelope {
	t.Helper()
	var env envelope
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("stdout is not one JSON envelope: %v\n%s", err, out)
	}
	return env
}

var errCredentialRead = errors.New("the credential path must not be touched in offline mode")

func TestStatusOfflineReadsNoCredentialAndNoNetwork(t *testing.T) {
	root := testProject(t)
	transport := &fakeTransport{handler: githubFixture()}
	deps := Deps{
		Dir:   root,
		Git:   workingCheckout(false),
		HTTP:  transport.httpClient(),
		Token: func(repository, role string) (string, error) { return "", errCredentialRead },
	}

	got := run(t, deps, "status", "--offline", "--issue", "7", "--role", "developer", "--json")
	if got.code != result.ExitFailed {
		t.Fatalf("exit = %d, want 1 (%s)", got.code, got.stderr)
	}
	env := decodeEnvelope(t, got.stdout)
	if env.Command != "status" || env.Status != "unknown" {
		t.Fatalf("envelope = %+v, want a status/unknown envelope", env)
	}
	if env.Repository != "ghpipe/ghpipe" {
		t.Errorf("repository = %q", env.Repository)
	}
	if env.Target == nil || env.Target.Kind != "issue" || env.Target.Number != 7 {
		t.Errorf("target = %+v, want issue #7", env.Target)
	}
	data := decodeStatusData(t, env.Data)
	if data.Severity != "unknown" || data.Stage != "unknown" {
		t.Errorf("severity/stage = %s/%s, want unknown/unknown", data.Severity, data.Stage)
	}
	if !containsCode(data.Blockers, "remote_unknown") {
		t.Errorf("blockers = %+v, want remote_unknown", data.Blockers)
	}
	if data.Checkout.Head != localSHA {
		t.Errorf("checkout head = %q, want the local SHA (offline still reports git facts)", data.Checkout.Head)
	}
	if got := transport.requested(); len(got) != 0 {
		t.Errorf("offline mode made requests: %v", got)
	}
}

func TestStatusOfflineDoesNotUseTheDefaultCredentialSource(t *testing.T) {
	// No Token seam at all: if offline mode reached DefaultToken, a missing
	// credential would turn into exit code 3 instead of the offline report.
	root := testProject(t)
	got := run(t, Deps{Dir: root, Git: workingCheckout(false)},
		"status", "--offline", "--issue", "7", "--role", "developer", "--json")
	if got.code != result.ExitFailed {
		t.Fatalf("exit = %d, want 1 (%s)", got.code, got.stderr)
	}
	if status := decodeEnvelope(t, got.stdout).Status; status != "unknown" {
		t.Fatalf("status = %s, want unknown", status)
	}
}

func TestStatusOnlineReadsOnly(t *testing.T) {
	root := testProject(t)
	transport := &fakeTransport{handler: githubFixture()}
	deps := Deps{
		Dir:   root,
		Git:   workingCheckout(false),
		HTTP:  transport.httpClient(),
		Token: func(repository, role string) (string, error) { return "fake-token", nil },
	}
	got := run(t, deps, "status", "--issue", "7", "--role", "developer", "--json")
	if got.code != result.ExitOK {
		t.Fatalf("exit = %d, want 0 (%s)", got.code, got.stdout+got.stderr)
	}
	env := decodeEnvelope(t, got.stdout)
	data := decodeStatusData(t, env.Data)
	if data.Stage != "review" {
		t.Errorf("stage = %q, want review", data.Stage)
	}
	if data.Severity != "ready" || len(data.Blockers) != 0 {
		t.Errorf("severity = %s, blockers = %+v, want ready and none", data.Severity, data.Blockers)
	}
	if len(data.NextActions) != 0 {
		t.Errorf("next_actions = %v, want empty for a ready task", data.NextActions)
	}
	if data.Checks == nil || !data.Checks.RulesRead || len(data.Checks.Items) != 1 {
		t.Fatalf("checks = %+v, want one evaluated required check", data.Checks)
	}
	if data.Checks.Items[0].State != "ready" || data.Checks.Items[0].Conclusion != "success" {
		t.Errorf("check item = %+v, want a ready success", data.Checks.Items[0])
	}
	if data.PR == nil || data.PR.Number != 9 || data.PR.HeadSHA != headSHA {
		t.Errorf("pr = %+v, want #9 at the head SHA", data.PR)
	}
	transport.assertReadsOnly(t)
}

func TestStatusOnlineNeedsACredential(t *testing.T) {
	root := testProject(t)
	transport := &fakeTransport{handler: githubFixture()}
	deps := Deps{
		Dir:   root,
		Git:   workingCheckout(false),
		HTTP:  transport.httpClient(),
		Token: func(repository, role string) (string, error) { return "", errors.New("no credential") },
	}
	got := run(t, deps, "status", "--issue", "7", "--role", "delivery")
	if got.code != result.ExitUsageError {
		t.Fatalf("exit = %d, want 3", got.code)
	}
	if requests := transport.requested(); len(requests) != 0 {
		t.Errorf("a missing credential must not reach the network: %v", requests)
	}
	if !strings.Contains(got.stderr, "credential") {
		t.Errorf("stderr = %q, want a credential message", got.stderr)
	}
}

func TestMetadataIsPreviewOnly(t *testing.T) {
	root := testProject(t)
	transport := &fakeTransport{handler: githubFixture()}
	deps := Deps{
		Dir:   root,
		Git:   workingCheckout(false),
		HTTP:  transport.httpClient(),
		Token: func(repository, role string) (string, error) { return "fake-token", nil },
	}
	got := run(t, deps, "metadata", "--issue", "7", "--json")
	if got.code != result.ExitOK {
		t.Fatalf("exit = %d, want 0 (%s)", got.code, got.stdout+got.stderr)
	}
	env := decodeEnvelope(t, got.stdout)
	if env.Command != "metadata" || env.Status != "succeeded" {
		t.Fatalf("envelope = %+v", env)
	}
	var data struct {
		Projection struct {
			Issue struct {
				Number int      `json:"number"`
				Stage  string   `json:"stage"`
				Labels []string `json:"labels"`
			} `json:"issue"`
			PRs []struct {
				Number    int      `json:"number"`
				Stage     string   `json:"stage"`
				Labels    []string `json:"labels"`
				Milestone string   `json:"milestone"`
			} `json:"prs"`
		} `json:"projection"`
		Differences struct {
			Issue struct {
				AddLabels    []string `json:"add_labels"`
				RemoveLabels []string `json:"remove_labels"`
			} `json:"issue"`
		} `json:"differences"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil {
		t.Fatalf("data: %v", err)
	}
	if data.Projection.Issue.Stage != "review" {
		t.Errorf("issue stage = %q, want review", data.Projection.Issue.Stage)
	}
	if len(data.Projection.PRs) != 1 || data.Projection.PRs[0].Stage != "review" {
		t.Errorf("projected PRs = %+v, want #9 in review", data.Projection.PRs)
	}
	if data.Projection.PRs[0].Milestone != "" {
		t.Errorf("projected PR milestone = %q, want empty", data.Projection.PRs[0].Milestone)
	}
	transport.assertReadsOnly(t)
}

func TestMetadataHasNoApplyFlag(t *testing.T) {
	root := testProject(t)
	transport := &fakeTransport{handler: githubFixture()}
	deps := Deps{
		Dir:   root,
		Git:   workingCheckout(false),
		HTTP:  transport.httpClient(),
		Token: func(repository, role string) (string, error) { return "fake-token", nil },
	}
	got := run(t, deps, "metadata", "--issue", "7", "--apply")
	if got.code != result.ExitUsageError {
		t.Fatalf("exit = %d, want 3 for --apply", got.code)
	}
	if requests := transport.requested(); len(requests) != 0 {
		t.Errorf("--apply must be refused before any request: %v", requests)
	}
}

func TestDoctorOffline(t *testing.T) {
	root := testProject(t)
	transport := &fakeTransport{handler: githubFixture()}
	deps := Deps{
		Dir:   root,
		Git:   workingCheckout(false),
		HTTP:  transport.httpClient(),
		Token: func(repository, role string) (string, error) { return "", errCredentialRead },
	}
	got := run(t, deps, "doctor", "--offline", "--for", "plan", "--json")
	if got.code != result.ExitOK {
		t.Fatalf("exit = %d, want 0 (%s)", got.code, got.stderr)
	}
	env := decodeEnvelope(t, got.stdout)
	var data struct {
		For                string `json:"for"`
		Offline            bool   `json:"offline"`
		Notice             string `json:"notice"`
		BusinessAcceptance struct {
			Evaluated bool `json:"evaluated"`
		} `json:"business_acceptance"`
		Checks []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil {
		t.Fatalf("data: %v", err)
	}
	if !data.Offline || data.For != "plan" {
		t.Errorf("data = %+v", data)
	}
	if data.BusinessAcceptance.Evaluated {
		t.Error("business_acceptance.evaluated = true, want false")
	}
	if data.Notice == "" {
		t.Error("the report must say it is not business acceptance")
	}
	if len(data.Checks) == 0 {
		t.Fatal("no checks were reported")
	}
	for _, check := range data.Checks {
		if check.Status != "pass" {
			t.Errorf("check %s = %s, want pass", check.Name, check.Status)
		}
	}
	if requests := transport.requested(); len(requests) != 0 {
		t.Errorf("offline doctor made requests: %v", requests)
	}
}

func TestDoctorRejectsUnsupportedPreflights(t *testing.T) {
	root := testProject(t)
	deps := Deps{Dir: root, Git: workingCheckout(false)}
	cases := [][]string{
		{"doctor", "--offline", "--for", "bootstrap"},
		{"doctor", "--for", "plan"},
		{"doctor", "--offline"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			if got := run(t, deps, args...); got.code != result.ExitUsageError {
				t.Fatalf("exit = %d, want 3", got.code)
			}
		})
	}
}

func TestDoctorDevelopReportsADirtyDefaultBranch(t *testing.T) {
	root := testProject(t)
	git := workingCheckout(true)
	git.responses["symbolic-ref --quiet --short HEAD"] = gitx.Command{Stdout: "main\n"}
	got := run(t, Deps{Dir: root, Git: git}, "doctor", "--offline", "--for", "develop", "--json")
	if got.code != result.ExitFailed {
		t.Fatalf("exit = %d, want 1 (%s)", got.code, got.stdout)
	}
	env := decodeEnvelope(t, got.stdout)
	if env.Status != "failed" {
		t.Errorf("status = %s, want failed", env.Status)
	}
	if !strings.Contains(got.stdout, "branch_guard") {
		t.Errorf("stdout = %s, want a branch_guard check", got.stdout)
	}
}

func TestStatusRejectsBadUsage(t *testing.T) {
	root := testProject(t)
	deps := Deps{Dir: root, Git: workingCheckout(false)}
	cases := [][]string{
		{"status", "--offline", "--role", "developer"},
		{"status", "--offline", "--issue", "7", "--pr", "9", "--role", "developer"},
		{"status", "--offline", "--issue", "7"},
		{"status", "--offline", "--issue", "0", "--role", "developer"},
		{"status", "--offline", "--issue", "7", "--role", "owner"},
		{"status", "--offline", "--issue", "7", "--role", "developer", "--nope"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			if got := run(t, deps, args...); got.code != result.ExitUsageError {
				t.Fatalf("exit = %d, want 3 (%s)", got.code, got.stderr)
			}
		})
	}
}

func TestStatusOutsideAProjectIsAPrecondition(t *testing.T) {
	dir := t.TempDir()
	got := run(t, Deps{Dir: dir, Git: workingCheckout(false)},
		"status", "--offline", "--issue", "7", "--role", "developer", "--json")
	if got.code != result.ExitUsageError {
		t.Fatalf("exit = %d, want 3 (%s)", got.code, got.stderr)
	}
	env := decodeEnvelope(t, got.stdout)
	if env.Error == nil || env.Error.Category != "configuration" {
		t.Errorf("error = %+v, want a configuration precondition", env.Error)
	}
}

type blockerView struct {
	Code     string `json:"code"`
	Severity string `json:"severity"`
}

type statusData struct {
	Severity string `json:"severity"`
	Stage    string `json:"stage"`
	Role     string `json:"role"`
	Checkout struct {
		Branch          string `json:"branch"`
		Head            string `json:"head"`
		TaskBranchLocal bool   `json:"task_branch_local"`
	} `json:"checkout"`
	PR *struct {
		Number  int    `json:"number"`
		HeadSHA string `json:"head_sha"`
	} `json:"pr"`
	Checks *struct {
		RulesRead bool `json:"rules_read"`
		Items     []struct {
			Context    string   `json:"context"`
			State      string   `json:"state"`
			Conclusion string   `json:"conclusion"`
			Sources    []string `json:"sources"`
		} `json:"items"`
	} `json:"checks"`
	Blockers    []blockerView `json:"blockers"`
	NextActions []string      `json:"next_actions"`
}

func decodeStatusData(t *testing.T, raw json.RawMessage) statusData {
	t.Helper()
	var data statusData
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatalf("status data: %v", err)
	}
	return data
}

func containsCode(blockers []blockerView, code string) bool {
	for _, blocker := range blockers {
		if blocker.Code == code {
			return true
		}
	}
	return false
}
