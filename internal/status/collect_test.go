package status

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/ghpipe/ghpipe/internal/checks"
	"github.com/ghpipe/ghpipe/internal/github"
	"github.com/ghpipe/ghpipe/internal/lifecycle"
	"github.com/ghpipe/ghpipe/internal/metadata"
)

// mergedTask is the smallest task that has reached "every closing fact is
// provable": the issue is closed, exactly one pull request is merged, and the
// objects already carry the labels a finished task projects. Every case below
// changes exactly one fact, so a test failure names the derivation that broke.
func mergedTask() metadata.Facts {
	return metadata.Facts{
		Repository:    "ghpipe/ghpipe",
		DefaultBranch: "main",
		Issue: metadata.IssueFacts{
			Number: 7, State: "CLOSED", Closed: true,
			Labels: []string{"ghpipe:done"},
		},
		PRs: []metadata.PRFacts{{
			Number:         9,
			State:          "CLOSED",
			Closed:         true,
			Merged:         true,
			MergeCommitSHA: headSHA,
			BaseRefName:    "main",
			HeadRefName:    "ghpipe/issue-7",
			HeadSHA:        headSHA,
			Labels:         []string{"ghpipe:done"},
		}},
		BranchExists: true,
	}
}

func readyChecks() checks.Evaluation {
	return checks.Evaluation{
		SHA:       headSHA,
		Branch:    "main",
		RulesRead: true,
		Items: []checks.Item{{
			Context:       "ghpipe-quality",
			IntegrationID: 15368,
			State:         checks.StateReady,
			Conclusion:    "success",
			Sources:       []string{"check_run"},
		}},
	}
}

func mergedRemote() *Remote {
	return &Remote{Facts: mergedTask(), Checks: readyChecks(), CleanupReport: true}
}

func taskBranch() Local {
	return Local{TaskBranch: "ghpipe/issue-7"}
}

// TestMetadataMatchesIsDerivedFromTheLabels pins the derivation the reviewer
// could not falsify in a571dd5: metadataMatches consults the projection instead
// of answering true, and every way the labels can deviate is visible here.
func TestMetadataMatchesIsDerivedFromTheLabels(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*metadata.Facts)
		want   bool
	}{
		{
			name:   "the labels already are the ones of a finished task",
			mutate: func(*metadata.Facts) {},
			want:   true,
		},
		{
			name: "a non-stage label carried by both objects does not decide it",
			mutate: func(f *metadata.Facts) {
				f.Issue.Labels = []string{"ghpipe:done", "ghpipe:type/slice"}
				f.PRs[0].Labels = []string{"ghpipe:done", "ghpipe:type/slice"}
			},
			want: true,
		},
		{
			name: "the task still carries the closing stage",
			mutate: func(f *metadata.Facts) {
				f.Issue.Closed = false
				f.Issue.State = "OPEN"
				f.Issue.Labels = []string{"ghpipe:closing"}
				f.PRs[0].Labels = []string{"ghpipe:closing"}
			},
			want: false,
		},
		{
			name: "two stage labels on the issue",
			mutate: func(f *metadata.Facts) {
				f.Issue.Labels = []string{"ghpipe:done", "ghpipe:active"}
			},
			want: false,
		},
		{
			name: "a missing stage label",
			mutate: func(f *metadata.Facts) {
				f.Issue.Labels = nil
				f.PRs[0].Labels = nil
			},
			want: false,
		},
		{
			name:   "the pull request carries a milestone",
			mutate: func(f *metadata.Facts) { f.PRs[0].Milestone = "M1" },
			want:   false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			facts := mergedTask()
			tc.mutate(&facts)
			if got := metadataMatches(facts, 9); got != tc.want {
				t.Errorf("metadataMatches(#9) = %v, want %v (issue labels %v, pr labels %v, pr milestone %q)",
					got, tc.want, facts.Issue.Labels, facts.PRs[0].Labels, facts.PRs[0].Milestone)
			}
		})
	}
}

// TestCompletionsDeriveEveryClosingFact drives completions directly, one fact
// at a time. In a571dd5 this function was only reached through Assemble, so
// forcing Metadata or RemoteBranchAbsent to true broke no test.
func TestCompletionsDeriveEveryClosingFact(t *testing.T) {
	complete := lifecycle.Completion{
		MergeCI:            true,
		Metadata:           true,
		RemoteBranchAbsent: true,
		LocalRefsAbsent:    true,
		CleanupReport:      true,
		IssueClosed:        true,
	}

	cases := []struct {
		name  string
		build func() (metadata.Facts, *Remote, Local)
		want  lifecycle.Completion
	}{
		{
			name: "every fact proven",
			build: func() (metadata.Facts, *Remote, Local) {
				facts := mergedTask()
				facts.BranchExists = false
				remote := mergedRemote()
				remote.Facts = facts
				return facts, remote, taskBranch()
			},
			want: complete,
		},
		{
			name: "the remote still advertises the branch",
			build: func() (metadata.Facts, *Remote, Local) {
				return mergedTask(), mergedRemote(), taskBranch()
			},
			want: lifecycle.Completion{
				MergeCI: true, Metadata: true, RemoteBranchAbsent: false,
				LocalRefsAbsent: true, CleanupReport: true, IssueClosed: true,
			},
		},
		{
			name: "the checks could not be read",
			build: func() (metadata.Facts, *Remote, Local) {
				remote := mergedRemote()
				remote.ChecksError = errNotRead
				return mergedTask(), remote, taskBranch()
			},
			want: lifecycle.Completion{
				MergeCI: false, Metadata: true, RemoteBranchAbsent: false,
				LocalRefsAbsent: true, CleanupReport: true, IssueClosed: true,
			},
		},
		{
			name: "the checks are ready but no check was ever read",
			build: func() (metadata.Facts, *Remote, Local) {
				remote := mergedRemote()
				remote.Checks = checks.Evaluation{SHA: headSHA, RulesRead: true}
				return mergedTask(), remote, taskBranch()
			},
			want: lifecycle.Completion{
				MergeCI: false, Metadata: true, RemoteBranchAbsent: false,
				LocalRefsAbsent: true, CleanupReport: true, IssueClosed: true,
			},
		},
		{
			name: "a required check is pending",
			build: func() (metadata.Facts, *Remote, Local) {
				remote := mergedRemote()
				remote.Checks.Items[0].State = checks.StatePending
				return mergedTask(), remote, taskBranch()
			},
			want: lifecycle.Completion{
				MergeCI: false, Metadata: true, RemoteBranchAbsent: false,
				LocalRefsAbsent: true, CleanupReport: true, IssueClosed: true,
			},
		},
		{
			name: "the local refs could not be read",
			build: func() (metadata.Facts, *Remote, Local) {
				return mergedTask(), mergedRemote(), Local{TaskBranch: "ghpipe/issue-7", TaskBranchErr: errNotRead}
			},
			want: lifecycle.Completion{
				MergeCI: true, Metadata: true, RemoteBranchAbsent: false,
				LocalRefsAbsent: false, CleanupReport: true, IssueClosed: true,
			},
		},
		{
			name: "the local branch is still there",
			build: func() (metadata.Facts, *Remote, Local) {
				local := taskBranch()
				local.TaskBranchLocal = true
				return mergedTask(), mergedRemote(), local
			},
			want: lifecycle.Completion{
				MergeCI: true, Metadata: true, RemoteBranchAbsent: false,
				LocalRefsAbsent: false, CleanupReport: true, IssueClosed: true,
			},
		},
		{
			name: "no cleanup declaration exists",
			build: func() (metadata.Facts, *Remote, Local) {
				remote := mergedRemote()
				remote.CleanupReport = false
				return mergedTask(), remote, taskBranch()
			},
			want: lifecycle.Completion{
				MergeCI: true, Metadata: true, RemoteBranchAbsent: false,
				LocalRefsAbsent: true, CleanupReport: false, IssueClosed: true,
			},
		},
		{
			name: "the metadata does not match",
			build: func() (metadata.Facts, *Remote, Local) {
				facts := mergedTask()
				facts.Issue.Labels = []string{"ghpipe:closing"}
				facts.PRs[0].Labels = []string{"ghpipe:closing"}
				remote := mergedRemote()
				remote.Facts = facts
				return facts, remote, taskBranch()
			},
			want: lifecycle.Completion{
				MergeCI: true, Metadata: false, RemoteBranchAbsent: false,
				LocalRefsAbsent: true, CleanupReport: true, IssueClosed: true,
			},
		},
		{
			name: "the issue is still open",
			build: func() (metadata.Facts, *Remote, Local) {
				facts := mergedTask()
				facts.Issue.Closed = false
				facts.Issue.State = "OPEN"
				remote := mergedRemote()
				remote.Facts = facts
				return facts, remote, taskBranch()
			},
			want: lifecycle.Completion{
				MergeCI: true, Metadata: false, RemoteBranchAbsent: false,
				LocalRefsAbsent: true, CleanupReport: true, IssueClosed: false,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			facts, remote, local := tc.build()
			got := completions(facts, remote, local)[9]
			if got != tc.want {
				t.Errorf("completions()[9] = %+v, want %+v", got, tc.want)
			}
			if got.Complete() != tc.want.Complete() {
				t.Errorf("Complete() = %v, want %v", got.Complete(), tc.want.Complete())
			}
		})
	}

	// A completion exists only for a merged pull request: an active task has
	// nothing to settle, and an empty answer must not be read as "done".
	if got := completions(metadata.Facts{Issue: metadata.IssueFacts{Number: 7}}, &Remote{}, Local{}); len(got) != 0 {
		t.Errorf("completions of an unmerged task = %+v, want none", got)
	}
}

// stubTransport is the injectable transport for this package's collect tests.
// No test in this repository binds a listening port: the sandbox forbids it and
// a real server would hide the request contract the test is about
// (docs/design.md 14.4).
type stubTransport struct {
	handler func(req *http.Request) (int, string)
}

func (s stubTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	status, payload := s.handler(req)
	return &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d", status),
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(payload)),
		Request:    req,
	}, nil
}

// TestUnreadBranchNeverProvesAbsence is the "unread branch" half of the
// reviewer's requirement. A branch that could not be read is not an absent
// branch: metadata.Collect refuses the whole read instead of answering "gone",
// so no completion is derived at all and nothing can claim the branch is gone.
// The other half - a branch the remote was asked about - is the 404 case in
// internal/metadata's TestCollect*Branch* tests.
func TestUnreadBranchNeverProvesAbsence(t *testing.T) {
	transport := stubTransport{handler: func(req *http.Request) (int, string) {
		switch {
		case req.Method == http.MethodGet && req.URL.Path == "/repos/ghpipe/ghpipe":
			return http.StatusOK, `{"default_branch":"main"}`
		case req.Method == http.MethodPost && req.URL.Path == "/graphql":
			body, _ := io.ReadAll(req.Body)
			if strings.Contains(string(body), "timelineItems") {
				return http.StatusOK, `{"data":{"repository":{"issue":{"timelineItems":` +
					`{"totalCount":0,"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[]}}}}}`
			}
			return http.StatusOK, `{"data":{"repository":{"issue":{` +
				`"number":7,"title":"task","state":"OPEN","body":"",` +
				`"labels":{"totalCount":1,"nodes":[{"name":"ghpipe:closing"}]}}}}}`
		case req.Method == http.MethodGet && strings.Contains(req.URL.Path, "/git/ref/heads/"):
			return http.StatusInternalServerError, `{"message":"the ref store is unavailable"}`
		default:
			return http.StatusNotFound, `{"message":"Not Found"}`
		}
	}}

	in, err := Collect(context.Background(), Options{
		Target:     Target{Kind: "issue", Number: 7},
		Repository: "ghpipe/ghpipe",
		Dir:        t.TempDir(),
		Client: github.New(
			github.WithHTTPClient(&http.Client{Transport: transport}),
			github.WithToken("test-token"),
		),
	})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if in.Remote != nil {
		t.Fatalf("Remote = %+v, want no facts: an unread branch must not become a fact", in.Remote)
	}
	if in.Problem == nil {
		t.Fatal("Problem = nil, want the unread branch reported")
	}
	if in.Problem.Severity != SeverityUnknown || in.Problem.Code != CodeRemoteUnknown {
		t.Errorf("Problem = %+v, want an unknown remote fact", in.Problem)
	}
}
