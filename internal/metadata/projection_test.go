package metadata

import (
	"errors"
	"reflect"
	"testing"

	"github.com/ghpipe/ghpipe/internal/lifecycle"
)

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

func TestProjectStagesAndLabels(t *testing.T) {
	cases := []struct {
		name          string
		facts         Facts
		wantIssue     lifecycle.Stage
		wantPR        lifecycle.Stage
		wantIssueTags []string
		wantPRTags    []string
	}{
		{
			name: "ready",
			facts: Facts{
				Issue: IssueFacts{Number: 7, Labels: []string{"bug", "ghpipe:ready"}},
			},
			wantIssue:     lifecycle.StageReady,
			wantIssueTags: []string{"bug", "ghpipe:ready"},
		},
		{
			name: "branch without a pull request is active",
			facts: Facts{
				Issue:        IssueFacts{Number: 7, Labels: []string{"ghpipe:ready"}},
				BranchExists: true,
			},
			wantIssue:     lifecycle.StageActive,
			wantIssueTags: []string{"ghpipe:active"},
		},
		{
			name: "draft pull request is active and inherits the issue labels",
			facts: Facts{
				Issue: IssueFacts{Number: 7, Labels: []string{"ghpipe:type/bug", "ghpipe:blocked", "area:cli"}},
				PRs:   []PRFacts{{Number: 9, Draft: true, Labels: []string{"ghpipe:type/bug"}}},
			},
			wantIssue:     lifecycle.StageActive,
			wantPR:        lifecycle.StageActive,
			wantIssueTags: []string{"area:cli", "ghpipe:active", "ghpipe:blocked", "ghpipe:type/bug"},
			wantPRTags:    []string{"area:cli", "ghpipe:active", "ghpipe:blocked", "ghpipe:type/bug"},
		},
		{
			name: "changes requested wins over review",
			facts: Facts{
				Issue: IssueFacts{Number: 7, Labels: []string{"ghpipe:active"}},
				PRs:   []PRFacts{{Number: 9, Labels: []string{"ghpipe:active"}}},
				Reviews: []Review{
					{Author: "a", State: "CHANGES_REQUESTED"},
				},
			},
			wantIssue:     lifecycle.StageChangesRequested,
			wantPR:        lifecycle.StageChangesRequested,
			wantIssueTags: []string{"ghpipe:changes-requested"},
			wantPRTags:    []string{"ghpipe:changes-requested"},
		},
		{
			name: "a later approval supersedes an earlier request for changes",
			facts: Facts{
				Issue: IssueFacts{Number: 7},
				PRs:   []PRFacts{{Number: 9}},
				Reviews: []Review{
					{Author: "a", State: "CHANGES_REQUESTED"},
					{Author: "a", State: "APPROVED"},
				},
			},
			wantIssue:     lifecycle.StageReview,
			wantPR:        lifecycle.StageReview,
			wantIssueTags: []string{"ghpipe:review"},
			wantPRTags:    []string{"ghpipe:review"},
		},
		{
			name: "merged with every closing fact is done",
			facts: Facts{
				Issue: IssueFacts{Number: 7, Closed: true, Labels: []string{"ghpipe:done"}},
				PRs: []PRFacts{{
					Number: 9, Merged: true, Closed: true, Labels: []string{"ghpipe:done"},
				}},
				Completions: map[int]lifecycle.Completion{9: complete()},
			},
			wantIssue:     lifecycle.StageDone,
			wantPR:        lifecycle.StageDone,
			wantIssueTags: []string{"ghpipe:done"},
			wantPRTags:    []string{"ghpipe:done"},
		},
		{
			name: "merged without the closing facts is closing",
			facts: Facts{
				Issue: IssueFacts{Number: 7, Labels: []string{"ghpipe:done"}},
				PRs:   []PRFacts{{Number: 9, Merged: true, Closed: true}},
			},
			wantIssue:     lifecycle.StageClosing,
			wantPR:        lifecycle.StageClosing,
			wantIssueTags: []string{"ghpipe:closing"},
			wantPRTags:    []string{"ghpipe:closing"},
		},
		{
			name: "a closed unmerged pull request is cancelled while the issue stays active",
			facts: Facts{
				Issue:        IssueFacts{Number: 7},
				PRs:          []PRFacts{{Number: 9, Closed: true}},
				BranchExists: true,
			},
			wantIssue:     lifecycle.StageActive,
			wantPR:        lifecycle.StageCancelled,
			wantIssueTags: []string{"ghpipe:active"},
			wantPRTags:    []string{"ghpipe:cancelled"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			projection, err := Project(tc.facts)
			if err != nil {
				t.Fatalf("Project: %v", err)
			}
			if projection.Issue.Stage != tc.wantIssue {
				t.Errorf("issue stage = %q, want %q", projection.Issue.Stage, tc.wantIssue)
			}
			if !reflect.DeepEqual(projection.Issue.Labels, tc.wantIssueTags) {
				t.Errorf("issue labels = %v, want %v", projection.Issue.Labels, tc.wantIssueTags)
			}
			if len(tc.facts.PRs) == 0 {
				return
			}
			object := projection.PRs[0]
			if object.Stage != tc.wantPR {
				t.Errorf("pr stage = %q, want %q", object.Stage, tc.wantPR)
			}
			if !reflect.DeepEqual(object.Labels, tc.wantPRTags) {
				t.Errorf("pr labels = %v, want %v", object.Labels, tc.wantPRTags)
			}
			// A pull request must never carry a milestone: the issue holds it.
			if object.Milestone != "" {
				t.Errorf("pr milestone = %q, want empty", object.Milestone)
			}
		})
	}
}

func TestProjectKeepsTheBlockedOverlayAndOneStageLabel(t *testing.T) {
	facts := Facts{
		Issue: IssueFacts{Number: 7, Labels: []string{
			"ghpipe:ready", "ghpipe:active", "ghpipe:blocked", "ghpipe:type/bug",
		}},
		PRs: []PRFacts{{Number: 9, Draft: true}},
	}
	projection, err := Project(facts)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	want := []string{"ghpipe:active", "ghpipe:blocked", "ghpipe:type/bug"}
	if !reflect.DeepEqual(projection.Issue.Labels, want) {
		t.Errorf("issue labels = %v, want %v (exactly one stage label)", projection.Issue.Labels, want)
	}
	if !reflect.DeepEqual(projection.PRs[0].Labels, want) {
		t.Errorf("pr labels = %v, want %v", projection.PRs[0].Labels, want)
	}
}

func TestProjectRefusesAmbiguousPullRequests(t *testing.T) {
	facts := Facts{
		Issue: IssueFacts{Number: 7},
		PRs:   []PRFacts{{Number: 9}, {Number: 10}},
	}
	if _, err := Project(facts); !errors.Is(err, lifecycle.ErrMultipleActivePRs) {
		t.Fatalf("err = %v, want lifecycle.ErrMultipleActivePRs", err)
	}
}

func TestDifferencesComputesWithoutWriting(t *testing.T) {
	issue := IssueFacts{
		Number:    7,
		Labels:    []string{"ghpipe:ready", "ghpipe:type/bug", "area:cli"},
		Milestone: "M1",
	}
	prs := []PRFacts{{
		Number:    9,
		Draft:     true,
		Labels:    []string{"ghpipe:ready", "area:cli", "stale-label"},
		Milestone: "M1",
	}}
	issueBefore := issue
	issueBefore.Labels = append([]string(nil), issue.Labels...)
	prsBefore := append([]PRFacts(nil), prs...)
	prsBefore[0].Labels = append([]string(nil), prs[0].Labels...)

	facts := Facts{Issue: issue, PRs: prs}
	projection, err := Project(facts)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	differences, err := Differences(issue, prs, projection)
	if err != nil {
		t.Fatalf("Differences: %v", err)
	}

	// The issue already carries the right stage label, so only the overlay-free
	// difference of the pull request shows up.
	if !reflect.DeepEqual(differences.Issue.AddLabels, []string{"ghpipe:active"}) {
		t.Errorf("issue add = %v, want [ghpipe:active]", differences.Issue.AddLabels)
	}
	if !reflect.DeepEqual(differences.Issue.RemoveLabels, []string{"ghpipe:ready"}) {
		t.Errorf("issue remove = %v, want [ghpipe:ready]", differences.Issue.RemoveLabels)
	}
	pr := differences.PRs[0]
	// The pull request inherits the issue's non-stage labels, so it gains the
	// type label too.
	if !reflect.DeepEqual(pr.AddLabels, []string{"ghpipe:active", "ghpipe:type/bug"}) {
		t.Errorf("pr add = %v, want [ghpipe:active ghpipe:type/bug]", pr.AddLabels)
	}
	// Only managed labels are removable: "stale-label" is not ghpipe's to take.
	if !reflect.DeepEqual(pr.RemoveLabels, []string{"ghpipe:ready"}) {
		t.Errorf("pr remove = %v, want [ghpipe:ready]", pr.RemoveLabels)
	}
	if len(pr.InheritLabels) != 0 {
		t.Errorf("pr inherit = %v, want none (every non-managed label is present)", pr.InheritLabels)
	}
	if pr.MilestoneCleared != true {
		t.Error("MilestoneCleared = false, want true (a pull request must not carry a milestone)")
	}
	// Purity: the facts the caller passed are untouched.
	if !reflect.DeepEqual(issue, issueBefore) {
		t.Errorf("issue facts changed: %+v", issue)
	}
	if !reflect.DeepEqual(prs, prsBefore) {
		t.Errorf("pr facts changed: %+v", prs)
	}
	if differences.Empty() {
		t.Error("Empty = true, want false")
	}
	// The stage label differs, so the metadata closing fact cannot hold.
	if differences.StageLabelsMatch() {
		t.Error("StageLabelsMatch = true, want false")
	}
	if differences.MilestonesMatch() {
		t.Error("MilestonesMatch = true, want false")
	}
}

func TestDifferencesInheritsIssueLabels(t *testing.T) {
	issue := IssueFacts{Number: 7, Labels: []string{"ghpipe:review", "area:cli"}}
	prs := []PRFacts{{Number: 9, Labels: []string{"ghpipe:review"}}}
	prsBefore := append([]PRFacts(nil), prs...)
	prsBefore[0].Labels = append([]string(nil), prs[0].Labels...)

	projection, err := Project(Facts{Issue: issue, PRs: prs})
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	differences, err := Differences(issue, prs, projection)
	if err != nil {
		t.Fatalf("Differences: %v", err)
	}
	// "area:cli" is not ghpipe's to write: it is inherited information, so it
	// is reported separately from the managed labels.
	if !reflect.DeepEqual(differences.PRs[0].InheritLabels, []string{"area:cli"}) {
		t.Errorf("inherit = %v, want [area:cli]", differences.PRs[0].InheritLabels)
	}
	if len(differences.PRs[0].AddLabels) != 0 {
		t.Errorf("add = %v, want none", differences.PRs[0].AddLabels)
	}
	if !reflect.DeepEqual(prs, prsBefore) {
		t.Errorf("pr facts changed: %+v", prs)
	}
	// Stage labels and milestones already match, so metadata is complete even
	// though a non-managed label is missing.
	if !differences.StageLabelsMatch() || !differences.MilestonesMatch() {
		t.Error("stage labels and milestones should match")
	}
}

func TestDifferencesNeedsAConsistentProjection(t *testing.T) {
	issue := IssueFacts{Number: 7}
	projection, err := Project(Facts{Issue: issue})
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	if _, err := Differences(IssueFacts{Number: 8}, nil, projection); err == nil {
		t.Fatal("expected an error for a projection of another issue")
	}
	projection.PRs = []Object{{Kind: ObjectPR, Number: 9, Stage: lifecycle.StageReview, Labels: []string{"ghpipe:review"}}}
	if _, err := Differences(issue, nil, projection); err == nil {
		t.Fatal("expected an error for a projected pull request that is not in the facts")
	}
}

func TestDifferencesIsEmptyWhenEverythingMatches(t *testing.T) {
	issue := IssueFacts{Number: 7, Labels: []string{"ghpipe:review", "area:cli"}}
	prs := []PRFacts{{Number: 9, Labels: []string{"ghpipe:review", "area:cli"}}}
	projection, err := Project(Facts{Issue: issue, PRs: prs})
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	differences, err := Differences(issue, prs, projection)
	if err != nil {
		t.Fatalf("Differences: %v", err)
	}
	if !differences.Empty() {
		t.Errorf("differences = %+v, want empty", differences)
	}
	if !differences.StageLabelsMatch() || !differences.MilestonesMatch() {
		t.Error("a matching task must satisfy both closing facts")
	}
}
