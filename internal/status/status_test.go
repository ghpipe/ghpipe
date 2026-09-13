package status

import (
	"reflect"
	"testing"

	"github.com/ghpipe/ghpipe/internal/checks"
	"github.com/ghpipe/ghpipe/internal/lifecycle"
	"github.com/ghpipe/ghpipe/internal/metadata"
)

const (
	headSHA   = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	otherSHA  = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	localHead = "cccccccccccccccccccccccccccccccccccccccc"
)

// allCodes is every blocker code this package can emit. Every one of them must
// have a next action: a blocker without an action is only a complaint.
var allCodes = []string{
	CodeRemoteUnknown, CodeRemoteUnreadable, CodePRMissing, CodeAmbiguousAssociation,
	CodeTaskBlocked, CodeDraft, CodeCancelled, CodeHeadMissing, CodeHeadChanged,
	CodeMergeState, CodeChecks, CodeReviews, CodeClosingMainCI, CodeClosingRemoteAbsent,
	CodeClosingCleanupReport, CodeClosingMetadata, CodeClosingLocalCleanup, CodeClosingIssue,
}

func TestEveryBlockerCodeHasANextAction(t *testing.T) {
	for _, code := range allCodes {
		if action := actionFor(Blocker{Code: code}); action == "" {
			t.Errorf("blocker %q has no next action", code)
		}
	}
}

func openTask() metadata.Facts {
	return metadata.Facts{
		Repository:    "ghpipe/ghpipe",
		DefaultBranch: "main",
		Issue:         metadata.IssueFacts{Number: 7, State: "OPEN", Labels: []string{"ghpipe:review"}},
		PRs: []metadata.PRFacts{{
			Number:           9,
			State:            "OPEN",
			BaseRefName:      "main",
			HeadRefName:      "ghpipe/issue-7",
			HeadSHA:          headSHA,
			Labels:           []string{"ghpipe:review"},
			ReviewDecision:   "APPROVED",
			MergeStateStatus: "CLEAN",
		}},
		Reviews: []metadata.Review{{ID: 1, Author: "rev", State: "APPROVED", CommitSHA: headSHA}},
	}
}

func openInput(facts metadata.Facts) Input {
	in := Input{
		Target: Target{Kind: "issue", Number: 7},
		Role:   "developer",
		Local: Local{
			Branch:             "ghpipe/issue-7",
			Head:               localHead,
			TaskBranch:         "ghpipe/issue-7",
			TaskBranchLocal:    true,
			TaskBranchTracking: true,
		},
	}
	in.Remote = &Remote{Facts: facts}
	// The final re-read saw the same head as the facts: no head_changed.
	if active := facts.ActivePR(); active != nil {
		in.Remote.CurrentPRHead = active.HeadSHA
	}
	return in
}

func TestAssembleScenarios(t *testing.T) {
	blocked := openTask()
	blocked.Issue.Labels = append(blocked.Issue.Labels, "ghpipe:blocked")

	draft := openTask()
	draft.PRs[0].Draft = true

	changesRequested := openTask()
	changesRequested.PRs[0].ReviewDecision = "CHANGES_REQUESTED"
	changesRequested.Reviews = []metadata.Review{{ID: 1, Author: "rev", State: "CHANGES_REQUESTED"}}

	needsReview := openTask()
	needsReview.PRs[0].ReviewDecision = "REVIEW_REQUIRED"

	dirty := openTask()
	dirty.PRs[0].MergeStateStatus = "DIRTY"

	missingPR := metadata.Facts{
		Repository:    "ghpipe/ghpipe",
		DefaultBranch: "main",
		Issue:         metadata.IssueFacts{Number: 7, State: "OPEN"},
	}

	cancelled := metadata.Facts{
		Repository:    "ghpipe/ghpipe",
		DefaultBranch: "main",
		Issue:         metadata.IssueFacts{Number: 7, State: "CLOSED", Closed: true},
	}

	ambiguous := openTask()
	ambiguous.PRs = append(ambiguous.PRs, metadata.PRFacts{
		Number: 10, State: "OPEN", BaseRefName: "main", HeadSHA: otherSHA,
	})

	failing := openTask()
	failingChecks := checks.Evaluation{SHA: headSHA, Branch: "main", RulesRead: true, Items: []checks.Item{
		{Context: "ghpipe-quality", IntegrationID: 15368, State: checks.StateFailed, Conclusion: "failure"},
	}}

	merged := func(completion lifecycle.Completion) metadata.Facts {
		// A merged task's issue is closed by the merge itself; the completion
		// fact and the issue state describe the same reality.
		state, closed := "OPEN", false
		if completion.IssueClosed {
			state, closed = "CLOSED", true
		}
		return metadata.Facts{
			Repository:    "ghpipe/ghpipe",
			DefaultBranch: "main",
			Issue:         metadata.IssueFacts{Number: 7, State: state, Closed: closed},
			PRs: []metadata.PRFacts{{
				Number: 9, State: "CLOSED", Closed: true, Merged: true,
				BaseRefName: "main", HeadRefName: "ghpipe/issue-7", HeadSHA: headSHA,
				MergeCommitSHA: otherSHA, Labels: []string{"ghpipe:closing"},
			}},
			Completions: map[int]lifecycle.Completion{9: completion},
		}
	}
	done := complete()
	noMergeCI := complete()
	noMergeCI.MergeCI = false
	noMetadata := complete()
	noMetadata.Metadata = false
	remotePresent := complete()
	remotePresent.RemoteBranchAbsent = false
	localPresent := complete()
	localPresent.LocalRefsAbsent = false
	noCleanupReport := complete()
	noCleanupReport.CleanupReport = false
	issueOpen := complete()
	issueOpen.IssueClosed = false

	cases := []struct {
		name         string
		input        Input
		wantCodes    []string
		wantSeverity Severity
		wantStage    string
	}{
		{
			name:         "offline is unknown",
			input:        Input{Target: Target{Kind: "issue", Number: 7}, Role: "developer", Local: Local{Head: localHead}, Problem: &Problem{Offline: true, Code: CodeRemoteUnknown, Severity: SeverityUnknown, Detail: "offline"}},
			wantCodes:    []string{CodeRemoteUnknown},
			wantSeverity: SeverityUnknown,
		},
		{
			name:         "a refused remote read is a failure",
			input:        Input{Target: Target{Kind: "issue", Number: 7}, Local: Local{Head: localHead}, Problem: &Problem{Code: CodeRemoteUnreadable, Severity: SeverityFailed, Detail: "denied"}},
			wantCodes:    []string{CodeRemoteUnreadable},
			wantSeverity: SeverityFailed,
		},
		{
			name:         "a missing HEAD is a failure",
			input:        Input{Target: Target{Kind: "issue", Number: 7}, Local: Local{}, Problem: &Problem{Code: CodeRemoteUnknown, Severity: SeverityUnknown}},
			wantCodes:    []string{CodeRemoteUnknown, CodeHeadMissing},
			wantSeverity: SeverityFailed,
		},
		{
			name:         "no pull request yet",
			input:        openInput(missingPR),
			wantCodes:    []string{CodePRMissing},
			wantSeverity: SeverityPending,
			wantStage:    string(lifecycle.StageReady),
		},
		{
			name:         "a draft pull request",
			input:        openInput(draft),
			wantCodes:    []string{CodeDraft},
			wantSeverity: SeverityPending,
			wantStage:    string(lifecycle.StageActive),
		},
		{
			name:         "changes requested",
			input:        openInput(changesRequested),
			wantCodes:    []string{CodeReviews},
			wantSeverity: SeverityFailed,
			wantStage:    string(lifecycle.StageChangesRequested),
		},
		{
			name:         "review not decided yet",
			input:        openInput(needsReview),
			wantCodes:    []string{CodeReviews},
			wantSeverity: SeverityPending,
			wantStage:    string(lifecycle.StageReview),
		},
		{
			name:         "the blocked overlay",
			input:        openInput(blocked),
			wantCodes:    []string{CodeTaskBlocked},
			wantSeverity: SeverityFailed,
		},
		{
			name:         "a dirty native merge state",
			input:        openInput(dirty),
			wantCodes:    []string{CodeMergeState},
			wantSeverity: SeverityFailed,
		},
		{
			name:         "a cancelled task",
			input:        openInput(cancelled),
			wantCodes:    []string{CodeCancelled},
			wantSeverity: SeverityPending,
			wantStage:    string(lifecycle.StageCancelled),
		},
		{
			name: "an ambiguous association",
			input: func() Input {
				in := openInput(ambiguous)
				in.Remote.CurrentPRHead = headSHA
				return in
			}(),
			wantCodes:    []string{CodeAmbiguousAssociation},
			wantSeverity: SeverityFailed,
		},
		{
			name: "the head moved while reading",
			input: func() Input {
				in := openInput(openTask())
				in.Remote.CurrentPRHead = otherSHA
				return in
			}(),
			wantCodes:    []string{CodeHeadChanged},
			wantSeverity: SeverityFailed,
			wantStage:    string(lifecycle.StageReview),
		},
		{
			name: "a required check failed",
			input: func() Input {
				in := openInput(failing)
				in.Remote.Checks = failingChecks
				return in
			}(),
			wantCodes:    []string{CodeChecks},
			wantSeverity: SeverityFailed,
		},
		{
			name:         "everything proven is done",
			input:        openInput(merged(done)),
			wantCodes:    nil,
			wantSeverity: SeverityReady,
			wantStage:    string(lifecycle.StageDone),
		},
		{
			name:         "the merge commit has not passed",
			input:        openInput(merged(noMergeCI)),
			wantCodes:    []string{CodeClosingMainCI},
			wantSeverity: SeverityPending,
			wantStage:    string(lifecycle.StageClosing),
		},
		{
			name:         "the remote branch is still there",
			input:        openInput(merged(remotePresent)),
			wantCodes:    []string{CodeClosingRemoteAbsent},
			wantSeverity: SeverityPending,
		},
		{
			name:         "the local branch is still there",
			input:        openInput(merged(localPresent)),
			wantCodes:    []string{CodeClosingLocalCleanup},
			wantSeverity: SeverityPending,
		},
		{
			name:         "no cleanup declaration",
			input:        openInput(merged(noCleanupReport)),
			wantCodes:    []string{CodeClosingCleanupReport},
			wantSeverity: SeverityPending,
		},
		{
			name:         "the metadata does not match",
			input:        openInput(merged(noMetadata)),
			wantCodes:    []string{CodeClosingMetadata},
			wantSeverity: SeverityPending,
		},
		{
			name:         "the issue is still open",
			input:        openInput(merged(issueOpen)),
			wantCodes:    []string{CodeClosingIssue},
			wantSeverity: SeverityPending,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			report := Assemble(tc.input)
			if report.Severity != tc.wantSeverity {
				t.Errorf("severity = %s, want %s (blockers %+v)", report.Severity, tc.wantSeverity, report.Blockers)
			}
			if tc.wantStage != "" && report.Stage != tc.wantStage {
				t.Errorf("stage = %q, want %q", report.Stage, tc.wantStage)
			}
			for _, code := range tc.wantCodes {
				if !hasCode(report.Blockers, code) {
					t.Errorf("missing blocker %q in %+v", code, report.Blockers)
				}
			}
			if len(tc.wantCodes) == 0 {
				if len(report.Blockers) != 0 {
					t.Errorf("blockers = %+v, want none", report.Blockers)
				}
				if len(report.NextActions) != 0 {
					t.Errorf("next_actions = %v, want empty for a ready task", report.NextActions)
				}
				return
			}
			if len(report.NextActions) == 0 {
				t.Errorf("no next actions for blockers %+v", report.Blockers)
			}
		})
	}
}

func TestSeverityIsTheWorstBlocker(t *testing.T) {
	facts := openTask()
	facts.Issue.Labels = append(facts.Issue.Labels, "ghpipe:blocked")
	in := openInput(facts)
	// A blocked task with a failing required check: the merged severity is
	// failed, and the order of the blockers does not matter.
	in.Remote.Checks = checks.Evaluation{SHA: headSHA, RulesRead: true, Items: []checks.Item{
		{Context: "ghpipe-quality", IntegrationID: 15368, State: checks.StateUnknown},
	}}
	report := Assemble(in)
	if report.Severity != SeverityFailed {
		t.Errorf("severity = %s, want failed", report.Severity)
	}
	if !hasCode(report.Blockers, CodeTaskBlocked) || !hasCode(report.Blockers, CodeChecks) {
		t.Errorf("blockers = %+v, want task_blocked and checks", report.Blockers)
	}
}

func TestReportRendersTheTargetObject(t *testing.T) {
	facts := openTask()
	in := openInput(facts)
	in.Target = Target{Kind: "pr", Number: 9}
	report := Assemble(in)
	if report.PR == nil || report.PR.Number != 9 {
		t.Fatalf("pr = %+v, want #9", report.PR)
	}
	if report.Stage != string(lifecycle.StageReview) {
		t.Errorf("stage = %q, want review for the requested pull request", report.Stage)
	}
	if report.Issue == nil || report.Issue.Number != 7 {
		t.Errorf("issue = %+v, want #7", report.Issue)
	}
	if report.Reviews == nil || report.Reviews.Decision != "APPROVED" {
		t.Errorf("reviews = %+v, want the native decision", report.Reviews)
	}
}

func TestLocalRefsAbsentNeedsAReadAnswer(t *testing.T) {
	cases := []struct {
		name  string
		local Local
		want  bool
	}{
		{"both refs are gone", Local{TaskBranch: "ghpipe/issue-7"}, true},
		{"the local branch is still there", Local{TaskBranch: "ghpipe/issue-7", TaskBranchLocal: true}, false},
		{"the tracking ref is still there", Local{TaskBranch: "ghpipe/issue-7", TaskBranchTracking: true}, false},
		{"the branch was never looked up", Local{}, false},
		{"the refs could not be read", Local{TaskBranch: "ghpipe/issue-7", TaskBranchErr: errNotRead}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.local.LocalRefsAbsent(); got != tc.want {
				t.Errorf("LocalRefsAbsent = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAssembleKeepsBlockersStable(t *testing.T) {
	in := openInput(openTask())
	first := Assemble(in)
	second := Assemble(in)
	if !reflect.DeepEqual(first.Blockers, second.Blockers) {
		t.Errorf("blockers differ between runs: %+v vs %+v", first.Blockers, second.Blockers)
	}
}

func hasCode(blockers []Blocker, code string) bool {
	for _, blocker := range blockers {
		if blocker.Code == code {
			return true
		}
	}
	return false
}

var errNotRead = &testError{}

type testError struct{}

func (*testError) Error() string { return "the refs could not be read" }

func complete() lifecycle.Completion {
	return lifecycle.Completion{
		MergeCI:            true,
		Metadata:           true,
		RemoteBranchAbsent: true,
		LocalRefsAbsent:    true,
		CleanupReport:      true,
		IssueClosed:        true,
	}
}
