package lifecycle

import (
	"errors"
	"reflect"
	"testing"
)

func TestStageLabelCoversEveryStageAndNothingElse(t *testing.T) {
	want := map[Stage]string{
		StageReady:            "ghpipe:ready",
		StageActive:           "ghpipe:active",
		StageReview:           "ghpipe:review",
		StageChangesRequested: "ghpipe:changes-requested",
		StageClosing:          "ghpipe:closing",
		StageDone:             "ghpipe:done",
		StageCancelled:        "ghpipe:cancelled",
	}
	if len(want) != 7 {
		t.Fatalf("the lifecycle has seven stages, table has %d", len(want))
	}
	for stage, label := range want {
		if got := StageLabel(stage); got != label {
			t.Errorf("StageLabel(%q) = %q, want %q", stage, got, label)
		}
	}
	// Unknown input must never produce a label that could be written to GitHub.
	for _, stage := range []Stage{"", "blocked", "pending", "failed", "unknown", "READY"} {
		if got := StageLabel(stage); got != "" {
			t.Errorf("StageLabel(%q) = %q, want empty", stage, got)
		}
	}
	if BlockedLabel != "ghpipe:blocked" {
		t.Errorf("BlockedLabel = %q, want ghpipe:blocked", BlockedLabel)
	}
}

func TestCompletionCompleteRequiresEveryFact(t *testing.T) {
	all := Completion{
		MergeCI: true, Metadata: true, RemoteBranchAbsent: true,
		LocalRefsAbsent: true, CleanupReport: true, IssueClosed: true,
	}
	if !all.Complete() {
		t.Fatal("all facts present but Complete() reported false")
	}
	facts := map[string]Completion{
		"merge ci":      {Metadata: true, RemoteBranchAbsent: true, LocalRefsAbsent: true, CleanupReport: true, IssueClosed: true},
		"metadata":      {MergeCI: true, RemoteBranchAbsent: true, LocalRefsAbsent: true, CleanupReport: true, IssueClosed: true},
		"remote branch": {MergeCI: true, Metadata: true, LocalRefsAbsent: true, CleanupReport: true, IssueClosed: true},
		"local refs":    {MergeCI: true, Metadata: true, RemoteBranchAbsent: true, CleanupReport: true, IssueClosed: true},
		"cleanup":       {MergeCI: true, Metadata: true, RemoteBranchAbsent: true, LocalRefsAbsent: true, IssueClosed: true},
		"issue closed":  {MergeCI: true, Metadata: true, RemoteBranchAbsent: true, LocalRefsAbsent: true, CleanupReport: true},
	}
	for name, completion := range facts {
		if completion.Complete() {
			t.Errorf("Complete() = true while the %s fact is missing", name)
		}
	}
}

func TestPRStage(t *testing.T) {
	complete := Completion{
		MergeCI: true, Metadata: true, RemoteBranchAbsent: true,
		LocalRefsAbsent: true, CleanupReport: true, IssueClosed: true,
	}
	cases := []struct {
		name       string
		pr         PR
		reviews    []Review
		completion Completion
		want       Stage
	}{
		{
			name: "merged with every closing fact is done",
			pr:   PR{Number: 5, Merged: true, Closed: true}, completion: complete, want: StageDone,
		},
		{
			name:       "merged with one missing fact is closing",
			pr:         PR{Number: 5, Merged: true, Closed: true},
			completion: Completion{MergeCI: true, Metadata: true, RemoteBranchAbsent: true, LocalRefsAbsent: true, CleanupReport: true},
			want:       StageClosing,
		},
		{
			name: "merged ignores reviews",
			pr:   PR{Number: 5, Merged: true, Closed: true}, completion: complete,
			reviews: []Review{{State: ReviewChangesRequested}},
			want:    StageDone,
		},
		{
			name: "closed unmerged is cancelled",
			pr:   PR{Number: 5, Closed: true}, want: StageCancelled,
		},
		{
			name: "draft is active",
			pr:   PR{Number: 5, Draft: true}, want: StageActive,
		},
		{
			name: "draft outranks a changes-requested review",
			pr:   PR{Number: 5, Draft: true}, reviews: []Review{{State: ReviewChangesRequested}},
			want: StageActive,
		},
		{
			name: "changes requested",
			pr:   PR{Number: 5}, reviews: []Review{{State: ReviewCommented}, {State: ReviewChangesRequested}},
			want: StageChangesRequested,
		},
		{
			name: "a dismissed changes-requested review does not count",
			pr:   PR{Number: 5}, reviews: []Review{{State: ReviewDismissed}, {State: ReviewApproved}},
			want: StageReview,
		},
		{
			name: "approved alone is still review",
			pr:   PR{Number: 5}, reviews: []Review{{State: ReviewApproved}}, want: StageReview,
		},
		{
			name: "open PR without reviews is review",
			pr:   PR{Number: 5}, want: StageReview,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := PRStage(tc.pr, tc.reviews, tc.completion); got != tc.want {
				t.Fatalf("PRStage = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestActivePR(t *testing.T) {
	prs := []PR{
		{Number: 1, Merged: true, Closed: true},
		{Number: 2, Closed: true},
		{Number: 3},
	}
	active, err := ActivePR(prs)
	if err != nil {
		t.Fatal(err)
	}
	if active == nil || active.Number != 3 {
		t.Fatalf("active = %+v, want PR 3", active)
	}
	if active, err = ActivePR([]PR{{Number: 1, Merged: true, Closed: true}}); err != nil || active != nil {
		t.Fatalf("merged-only = (%+v, %v), want (nil, nil)", active, err)
	}
	if active, err = ActivePR(nil); err != nil || active != nil {
		t.Fatalf("empty = (%+v, %v), want (nil, nil)", active, err)
	}
}

func TestActivePRRejectsAmbiguity(t *testing.T) {
	_, err := ActivePR([]PR{{Number: 3}, {Number: 4, Draft: true}})
	if !errors.Is(err, ErrMultipleActivePRs) {
		t.Fatalf("err = %v, want ErrMultipleActivePRs", err)
	}
}

func TestMergedPRPicksTheNewest(t *testing.T) {
	prs := []PR{
		{Number: 9, Merged: true, Closed: true},
		{Number: 4, Merged: true, Closed: true},
		{Number: 12, Closed: true},
	}
	merged := MergedPR(prs)
	if merged == nil || merged.Number != 9 {
		t.Fatalf("merged = %+v, want PR 9", merged)
	}
	if merged = MergedPR([]PR{{Number: 1, Closed: true}}); merged != nil {
		t.Fatalf("merged = %+v, want nil", merged)
	}
}

func TestPlan(t *testing.T) {
	complete := Completion{
		MergeCI: true, Metadata: true, RemoteBranchAbsent: true,
		LocalRefsAbsent: true, CleanupReport: true, IssueClosed: true,
	}
	open := Issue{Number: 3}
	closed := Issue{Number: 3, Closed: true}
	draft := PR{Number: 5, Draft: true}
	ready := PR{Number: 5}
	merged := PR{Number: 5, Merged: true, Closed: true}
	abandoned := PR{Number: 5, Closed: true}
	changesRequested := []Review{{State: ReviewChangesRequested}}

	cases := []struct {
		name         string
		issue        Issue
		prs          []PR
		reviews      []Review
		branchExists bool
		completions  map[int]Completion
		want         map[int]Stage
	}{
		{
			name: "no pr and no branch is ready", issue: open, want: map[int]Stage{3: StageReady},
		},
		{
			name: "no pr with a branch is active", issue: open, branchExists: true,
			want: map[int]Stage{3: StageActive},
		},
		{
			name: "a draft pr is active", issue: open, prs: []PR{draft},
			want: map[int]Stage{3: StageActive, 5: StageActive},
		},
		{
			name: "an open pr under review", issue: open, prs: []PR{ready},
			want: map[int]Stage{3: StageReview, 5: StageReview},
		},
		{
			name:  "changes requested sends it back to the developer",
			issue: open, prs: []PR{ready}, reviews: changesRequested,
			want: map[int]Stage{3: StageChangesRequested, 5: StageChangesRequested},
		},
		{
			name:  "a branch that is already merged closes the task",
			issue: open, prs: []PR{merged},
			want: map[int]Stage{3: StageClosing, 5: StageClosing},
		},
		{
			// The issue rule outranks the PR's closing facts for the issue
			// entry: an issue still open behind a merge is not done. The
			// facts themselves are self-contradictory here (Completion
			// carries IssueClosed while the issue is open), which is only
			// reachable by hand-built input - the PR entry reports what those
			// facts say about the PR.
			name:  "an open issue stays closing even with every fact present",
			issue: open, prs: []PR{merged}, completions: map[int]Completion{5: complete},
			want: map[int]Stage{3: StageClosing, 5: StageDone},
		},
		{
			name:  "a closed issue with an incomplete merge is closing",
			issue: closed, prs: []PR{merged},
			want: map[int]Stage{3: StageClosing, 5: StageClosing},
		},
		{
			name:  "a closed issue with every closing fact is done",
			issue: closed, prs: []PR{merged}, completions: map[int]Completion{5: complete},
			want: map[int]Stage{3: StageDone, 5: StageDone},
		},
		{
			name:  "a closed issue with no merged pr is cancelled",
			issue: closed, prs: []PR{abandoned}, branchExists: true,
			want: map[int]Stage{3: StageCancelled, 5: StageCancelled},
		},
		{
			name:  "a closed issue with no pr at all is cancelled",
			issue: closed, want: map[int]Stage{3: StageCancelled},
		},
		{
			name:  "a closed unmerged pr leaves the task active while the issue is open",
			issue: open, prs: []PR{abandoned}, branchExists: true,
			want: map[int]Stage{3: StageActive, 5: StageCancelled},
		},
		{
			name:  "a closed unmerged pr with no branch leaves the task ready",
			issue: open, prs: []PR{abandoned},
			want: map[int]Stage{3: StageReady, 5: StageCancelled},
		},
		{
			name:  "a merged pr plus a new draft pr is active again",
			issue: open, prs: []PR{merged, {Number: 7, Draft: true}},
			want: map[int]Stage{3: StageActive, 5: StageClosing, 7: StageActive},
		},
		{
			name:  "a merged pr is used when the issue is closed and none is active",
			issue: closed, prs: []PR{merged, {Number: 6, Closed: true}},
			completions: map[int]Completion{5: complete},
			want:        map[int]Stage{3: StageDone, 5: StageDone, 6: StageCancelled},
		},
		{
			name:  "the newest merged pr decides the completion",
			issue: closed, prs: []PR{
				{Number: 5, Merged: true, Closed: true},
				{Number: 8, Merged: true, Closed: true},
			},
			completions: map[int]Completion{5: complete},
			want:        map[int]Stage{3: StageClosing, 5: StageDone, 8: StageClosing},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Plan(tc.issue, tc.prs, tc.reviews, tc.branchExists, tc.completions)
			if err != nil {
				t.Fatalf("Plan returned %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Plan = %v, want %v", got, tc.want)
			}
			// One entry per object: the issue plus every associated PR. A
			// missing PR entry is how an unsynced PR label would slip through.
			if len(got) != len(tc.prs)+1 {
				t.Fatalf("Plan returned %d entries, want %d (one per object)", len(got), len(tc.prs)+1)
			}
			for number, stage := range got {
				if stage == "" {
					t.Fatalf("object %d projected an empty stage", number)
				}
			}
		})
	}
}

func TestPlanRejectsTwoActivePRs(t *testing.T) {
	cases := []struct {
		name  string
		issue Issue
	}{
		{"open issue", Issue{Number: 3}},
		{"closed issue", Issue{Number: 3, Closed: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prs := []PR{{Number: 5}, {Number: 6, Draft: true}}
			got, err := Plan(tc.issue, prs, nil, true, nil)
			if !errors.Is(err, ErrMultipleActivePRs) {
				t.Fatalf("err = %v, want ErrMultipleActivePRs", err)
			}
			// No partial map: half a projection would be written as labels.
			if got != nil {
				t.Fatalf("stages = %v, want nil on an ambiguous association", got)
			}
		})
	}
}

// TestPlanProjectsStageForEveryObject is the multi-object shape metadata sync
// depends on: one stage for the issue and one for each associated PR, each
// derived from that object's own facts, in a single call.
func TestPlanProjectsStageForEveryObject(t *testing.T) {
	complete := Completion{
		MergeCI: true, Metadata: true, RemoteBranchAbsent: true,
		LocalRefsAbsent: true, CleanupReport: true, IssueClosed: true,
	}
	// A task whose history contains more than one PR: one merge, one PR closed
	// without merging, and - in some cases - a fresh active PR.
	mergedPR := PR{Number: 5, Merged: true, Closed: true}
	abandonedPR := PR{Number: 6, Closed: true}
	draftPR := PR{Number: 7, Draft: true}

	cases := []struct {
		name         string
		issue        Issue
		prs          []PR
		reviews      []Review
		branchExists bool
		completions  map[int]Completion
		want         map[int]Stage
	}{
		{
			name:  "an open issue with a merged and an abandoned pr",
			issue: Issue{Number: 3}, prs: []PR{mergedPR, abandonedPR},
			want: map[int]Stage{3: StageClosing, 5: StageClosing, 6: StageCancelled},
		},
		{
			name:  "a closed issue with a merged and an abandoned pr",
			issue: Issue{Number: 3, Closed: true}, prs: []PR{mergedPR, abandonedPR},
			completions: map[int]Completion{5: complete},
			want:        map[int]Stage{3: StageDone, 5: StageDone, 6: StageCancelled},
		},
		{
			name:  "an active pr does not change the stages of the older ones",
			issue: Issue{Number: 3}, prs: []PR{mergedPR, abandonedPR, draftPR},
			want: map[int]Stage{3: StageActive, 5: StageClosing, 6: StageCancelled, 7: StageActive},
		},
		{
			name:  "a work in progress pr carries its own review state",
			issue: Issue{Number: 3}, prs: []PR{mergedPR, abandonedPR, {Number: 7}},
			reviews: []Review{{State: ReviewChangesRequested}},
			want: map[int]Stage{
				3: StageChangesRequested, 5: StageClosing, 6: StageCancelled, 7: StageChangesRequested,
			},
		},
		{
			name:  "a closed issue whose only pr was abandoned",
			issue: Issue{Number: 3, Closed: true}, prs: []PR{abandonedPR}, branchExists: true,
			want: map[int]Stage{3: StageCancelled, 6: StageCancelled},
		},
		{
			name:  "no prs leaves the issue as the only object",
			issue: Issue{Number: 3}, branchExists: true,
			want: map[int]Stage{3: StageActive},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Plan(tc.issue, tc.prs, tc.reviews, tc.branchExists, tc.completions)
			if err != nil {
				t.Fatalf("Plan returned %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("stages = %v, want %v", got, tc.want)
			}
			if len(got) != len(tc.prs)+1 {
				t.Fatalf("returned %d entries, want one per object (%d)", len(got), len(tc.prs)+1)
			}
			// Every PR the caller passed must be labelled, not just the ones
			// the issue-level rule looked at.
			for _, pr := range tc.prs {
				if stage, ok := got[pr.Number]; !ok {
					t.Fatalf("PR %d has no stage in %v", pr.Number, got)
				} else if stage != tc.want[pr.Number] {
					t.Fatalf("PR %d = %q, want %q", pr.Number, stage, tc.want[pr.Number])
				}
			}
			if _, ok := got[tc.issue.Number]; !ok {
				t.Fatalf("issue %d has no stage in %v", tc.issue.Number, got)
			}
		})
	}
}

func TestPlanIsDeterministicAndDoesNotMutateInput(t *testing.T) {
	prs := []PR{{Number: 5, Draft: true}}
	before := prs[0]
	first, err := Plan(Issue{Number: 3}, prs, nil, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Plan(Issue{Number: 3}, prs, nil, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("same facts projected %v then %v", first, second)
	}
	if prs[0] != before {
		t.Fatalf("Plan mutated its input: %+v", prs[0])
	}
}
