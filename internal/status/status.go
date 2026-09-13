// Package status assembles the read-only handoff state of one task: the local
// checkout, the remote facts, the projected stage, the blockers and the next
// actions a human or an agent should take (docs/design.md 10.1).
//
// Assembly is pure. Collect reads the world (git, GitHub) and hands Assemble
// every fact, so the rules that decide what is blocking can be tested without a
// network, a checkout or a clock.
package status

import (
	"fmt"
	"strings"

	"github.com/ghpipe/ghpipe/internal/checks"
	"github.com/ghpipe/ghpipe/internal/lifecycle"
	"github.com/ghpipe/ghpipe/internal/metadata"
)

// Severity is the merged severity of a report. The values are the envelope's
// status values, so the result envelope never needs a second vocabulary.
type Severity string

// The severities, ordered by severity: failed > unknown > pending > ready.
// "unknown" outranks "pending": "we could not find out" is a stronger reason to
// stop than "it is not done yet" (docs/design.md 10.1).
const (
	SeverityReady   Severity = "ready"
	SeverityPending Severity = "pending"
	SeverityUnknown Severity = "unknown"
	SeverityFailed  Severity = "failed"
)

func rank(s Severity) int {
	switch s {
	case SeverityFailed:
		return 3
	case SeverityUnknown:
		return 2
	case SeverityPending:
		return 1
	default:
		return 0
	}
}

// Merge returns the more severe of two severities.
func Merge(a, b Severity) Severity {
	if rank(a) >= rank(b) {
		return a
	}
	return b
}

// Blocker codes. Every code a report can carry is defined here, so the
// vocabulary is greppable and one code can never be spelled two ways.
const (
	// CodeRemoteUnknown: the remote facts could not be read (offline mode, a
	// transport failure, an incomplete read). Nothing is claimed about the
	// task.
	CodeRemoteUnknown = "remote_unknown"
	// CodeRemoteUnreadable: the remote answered and refused - the credential
	// was rejected, or the repository is not visible.
	CodeRemoteUnreadable = "remote_unreadable"
	// CodePRMissing: no associated pull request exists yet.
	CodePRMissing = "pr_missing"
	// CodeAmbiguousAssociation: the issue's pull requests cannot be associated
	// unambiguously.
	CodeAmbiguousAssociation = "ambiguous_association"
	// CodeTaskBlocked: the ghpipe:blocked overlay is present.
	CodeTaskBlocked = "task_blocked"
	// CodeDraft: the pull request is still a draft.
	CodeDraft = "draft"
	// CodeCancelled: the issue is closed without a merged pull request.
	CodeCancelled = "cancelled"
	// CodeHeadMissing: the checkout has no resolvable HEAD.
	CodeHeadMissing = "head_missing"
	// CodeHeadChanged: the pull request head moved while the facts were read.
	CodeHeadChanged = "head_changed"
	// CodeMergeState: the native merge state is not clean.
	CodeMergeState = "merge_state"
	// CodeChecks: the required checks are not all satisfied.
	CodeChecks = "checks"
	// CodeReviews: the review decision is not satisfied.
	CodeReviews = "reviews"

	// The closing facts of a merged task. Each one is independent: the task
	// only reaches done when all of them hold (docs/design.md 2.2, 8.5).
	CodeClosingMainCI        = "closing_main_ci"
	CodeClosingRemoteAbsent  = "closing_remote_absent"
	CodeClosingCleanupReport = "closing_cleanup_report"
	CodeClosingMetadata      = "closing_metadata"
	CodeClosingLocalCleanup  = "closing_local_cleanup"
	CodeClosingIssue         = "closing_issue"
)

// Blocker is one reason the task is not ready, with the severity that reason
// carries.
type Blocker struct {
	Code     string   `json:"code"`
	Severity Severity `json:"severity"`
	Detail   string   `json:"detail,omitempty"`
}

// Target is the object the report is about.
type Target struct {
	Kind   string `json:"kind"`
	Number int    `json:"number"`
}

// Checkout is the local checkout as the report saw it.
type Checkout struct {
	Branch string `json:"branch"`
	Head   string `json:"head"`
	// Detached is true when HEAD is not on a branch. It is reported rather
	// than hidden: a detached checkout is a supported state for review but
	// never for development (docs/design.md 8.2).
	Detached bool `json:"detached"`
	Dirty    bool `json:"dirty,omitempty"`
	Shallow  bool `json:"shallow,omitempty"`
	// TaskBranch is the branch this issue develops on, and the two flags say
	// whether the checkout still has it locally and as a tracking ref.
	TaskBranch       string `json:"task_branch,omitempty"`
	TaskBranchLocal  bool   `json:"task_branch_local"`
	TaskBranchRemote bool   `json:"task_branch_tracking"`
}

// Reviews is the native review decision next to the raw review counts. The
// decision, not the counts, is what decides a stage: a review that was
// superseded by a later approval is still in the list but no longer decides
// anything.
type Reviews struct {
	Decision         string `json:"decision,omitempty"`
	Count            int    `json:"count"`
	Approved         int    `json:"approved"`
	ChangesRequested int    `json:"changes_requested"`
	Dismissed        int    `json:"dismissed"`
}

// Report is the status of one task.
type Report struct {
	Target   Target   `json:"target"`
	Checkout Checkout `json:"checkout"`
	Role     string   `json:"role"`
	// Issue and PR are the facts the stage was projected from. Both are null
	// when the remote facts are unknown.
	Issue *IssueView `json:"issue"`
	PR    *PRView    `json:"pr"`
	// Stage is the projected stage, or "unknown" when it could not be
	// projected.
	Stage   string             `json:"stage"`
	Reviews *Reviews           `json:"reviews"`
	Checks  *checks.Evaluation `json:"checks"`
	// DefaultBranchHead is shown next to a merged task's checks so a later
	// green main run cannot hide a red merge commit (docs/design.md 10.1).
	DefaultBranchHead string    `json:"default_branch_head,omitempty"`
	Blockers          []Blocker `json:"blockers"`
	NextActions       []string  `json:"next_actions"`
	ObservedAt        string    `json:"observed_at"`
	// Severity is the merged severity; the envelope status carries the same
	// value.
	Severity Severity `json:"severity"`
}

// IssueView is the issue as the report renders it.
type IssueView struct {
	Number    int      `json:"number"`
	Title     string   `json:"title"`
	State     string   `json:"state"`
	Closed    bool     `json:"closed"`
	Labels    []string `json:"labels"`
	Milestone string   `json:"milestone,omitempty"`
}

// PRView is the pull request as the report renders it. The body is deliberately
// absent: a report is not a copy of the ledger.
type PRView struct {
	Number           int      `json:"number"`
	State            string   `json:"state"`
	Closed           bool     `json:"closed"`
	Draft            bool     `json:"draft"`
	Merged           bool     `json:"merged"`
	MergeCommitSHA   string   `json:"merge_commit_sha,omitempty"`
	BaseRefName      string   `json:"base_ref_name"`
	HeadRefName      string   `json:"head_ref_name"`
	HeadSHA          string   `json:"head_sha"`
	Labels           []string `json:"labels"`
	Milestone        string   `json:"milestone,omitempty"`
	ReviewDecision   string   `json:"review_decision,omitempty"`
	MergeStateStatus string   `json:"merge_state_status,omitempty"`
}

// Local is every local fact the report needs.
type Local struct {
	Branch  string
	Head    string
	Dirty   bool
	Shallow bool
	// TaskBranch is "ghpipe/issue-N". It is empty when the issue number is not
	// known yet, and then no claim is made about its refs.
	TaskBranch         string
	TaskBranchLocal    bool
	TaskBranchTracking bool
	// TaskBranchErr is set when the refs could not be read at all. It keeps
	// "gone" and "not asked" apart, so an unreadable checkout can never
	// satisfy the local-cleanup closing fact.
	TaskBranchErr error
}

// LocalRefsAbsent reports whether both the local branch and its tracking ref
// are gone - one of the three independent cleanup facts (docs/design.md 8.5).
// A task branch that was never looked up answers "no".
func (l Local) LocalRefsAbsent() bool {
	return l.TaskBranch != "" && l.TaskBranchErr == nil && !l.TaskBranchLocal && !l.TaskBranchTracking
}

// Remote is every remote fact the report needs, already collected.
type Remote struct {
	Facts metadata.Facts
	// Checks is the evaluation bound to the pull request head, or to the merge
	// commit once the pull request is merged.
	Checks checks.Evaluation
	// ChecksError is set when the checks could not be read at all.
	ChecksError error
	// DefaultBranchHead is the default branch head, read only for a merged
	// task.
	DefaultBranchHead string
	// CleanupReport is "a delivery cleanup declaration for this pull request
	// exists".
	CleanupReport bool
	// CleanupError is set when the declaration could not be read.
	CleanupError error
	// CurrentPRHead is the head SHA re-read after every other fact, or empty
	// when the pull request is not active.
	CurrentPRHead string
	// HeadRereadError is set when the final head re-read failed.
	HeadRereadError error
}

// Problem describes why no remote facts are available.
type Problem struct {
	Offline  bool
	Code     string
	Severity Severity
	Detail   string
}

// Input is everything Assemble needs.
type Input struct {
	Target     Target
	Role       string
	Local      Local
	Remote     *Remote
	Problem    *Problem
	ObservedAt string
}

// Assemble turns collected facts into a report.
//
// The order of the checks is the order of the design's assembly sequence, and
// every blocker is derived from an observed fact - never from the absence of
// an error.
func Assemble(in Input) Report {
	report := Report{
		Target: in.Target,
		Checkout: Checkout{
			Branch:           in.Local.Branch,
			Head:             in.Local.Head,
			Detached:         in.Local.Branch == "" && in.Local.Head != "",
			Dirty:            in.Local.Dirty,
			Shallow:          in.Local.Shallow,
			TaskBranch:       in.Local.TaskBranch,
			TaskBranchLocal:  in.Local.TaskBranchLocal,
			TaskBranchRemote: in.Local.TaskBranchTracking,
		},
		Role:        in.Role,
		Stage:       "unknown",
		Blockers:    []Blocker{},
		NextActions: []string{},
		ObservedAt:  in.ObservedAt,
		Severity:    SeverityReady,
	}
	var blockers []Blocker
	add := func(code string, severity Severity, detail string) {
		blockers = append(blockers, Blocker{Code: code, Severity: severity, Detail: detail})
	}

	if in.Local.Head == "" {
		// The checkout could not be resolved. Everything else may still be
		// true, but a task whose local HEAD is unknown cannot be handed over.
		add(CodeHeadMissing, SeverityFailed, "the checkout HEAD could not be resolved")
	}

	if in.Remote == nil {
		problem := in.Problem
		if problem == nil {
			problem = &Problem{Code: CodeRemoteUnknown, Severity: SeverityUnknown, Detail: "no remote facts were collected"}
		}
		add(problem.Code, problem.Severity, problem.Detail)
		return finish(report, blockers)
	}

	remote := in.Remote
	facts := remote.Facts
	issueView := IssueView{
		Number:    facts.Issue.Number,
		Title:     facts.Issue.Title,
		State:     facts.Issue.State,
		Closed:    facts.Issue.Closed,
		Labels:    facts.Issue.Labels,
		Milestone: facts.Issue.Milestone,
	}
	report.Issue = &issueView

	focus := focusPR(facts)
	if focus != nil {
		report.PR = &PRView{
			Number:           focus.Number,
			State:            focus.State,
			Closed:           focus.Closed,
			Draft:            focus.Draft,
			Merged:           focus.Merged,
			MergeCommitSHA:   focus.MergeCommitSHA,
			BaseRefName:      focus.BaseRefName,
			HeadRefName:      focus.HeadRefName,
			HeadSHA:          focus.HeadSHA,
			Labels:           focus.Labels,
			Milestone:        focus.Milestone,
			ReviewDecision:   focus.ReviewDecision,
			MergeStateStatus: focus.MergeStateStatus,
		}
	}
	// Reviews are only collected for the active pull request, so a merged task
	// reports no review section rather than an empty verdict.
	if len(facts.Reviews) > 0 {
		report.Reviews = reviewSummary(facts)
	}
	if remote.ChecksError != nil || len(remote.Checks.Items) > 0 {
		evaluation := remote.Checks
		report.Checks = &evaluation
	}
	report.DefaultBranchHead = remote.DefaultBranchHead

	projection, err := metadata.Project(facts)
	if err != nil {
		add(CodeAmbiguousAssociation, SeverityFailed, "the lifecycle projection is ambiguous")
	} else {
		report.Stage = string(stageOf(projection, in.Target))
	}

	if hasLabel(facts.Issue.Labels, lifecycle.BlockedLabel) {
		add(CodeTaskBlocked, SeverityFailed, "the issue carries "+lifecycle.BlockedLabel)
	}

	merged := facts.MergedPR()
	active := facts.ActivePR()
	switch {
	case facts.Issue.Closed && merged == nil:
		add(CodeCancelled, SeverityPending, "the issue is closed without a merged pull request")
	case merged != nil:
		// The closing facts are judged below, one by one.
	default:
		// No merged pull request: the task is somewhere before closing, and a
		// missing pull request is the handoff the orchestrator is waiting for.
		if active == nil {
			add(CodePRMissing, SeverityPending, "the issue has no associated pull request")
		}
	}

	if active != nil {
		if active.Draft {
			add(CodeDraft, SeverityPending, "the pull request is a draft")
		}
		if remote.CurrentPRHead != "" && !strings.EqualFold(remote.CurrentPRHead, active.HeadSHA) {
			// The head moved while the facts were read: every statement about
			// this SHA is about a commit that is no longer the head.
			add(CodeHeadChanged, SeverityFailed, "the pull request head changed while the facts were read")
		}
		if remote.HeadRereadError != nil {
			add(CodeRemoteUnknown, SeverityUnknown, "the pull request head could not be re-read")
		}
		if severity, detail := mergeStateBlocker(active); severity != SeverityReady {
			add(CodeMergeState, severity, detail)
		}
		if severity, detail := reviewBlocker(active); severity != SeverityReady {
			add(CodeReviews, severity, detail)
		}
		if severity, detail := checksBlocker(remote); severity != SeverityReady {
			add(CodeChecks, severity, detail)
		}
	}

	if merged != nil {
		// A merged task is only done when every closing fact is proven. The
		// facts are independent, so each missing one is reported on its own
		// (docs/design.md 2.2).
		completion := facts.Completions[merged.Number]
		if !completion.MergeCI {
			add(CodeClosingMainCI, SeverityPending, "the required checks of the merge commit are not all successful")
		}
		if !completion.RemoteBranchAbsent {
			add(CodeClosingRemoteAbsent, SeverityPending, "the remote still advertises "+metadata.TaskBranch(facts.Issue.Number))
		}
		if !completion.LocalRefsAbsent {
			add(CodeClosingLocalCleanup, SeverityPending, "the local branch or its tracking ref still exists")
		}
		if !completion.CleanupReport {
			detail := "no cleanup declaration was found for this pull request"
			if remote.CleanupError != nil {
				detail = "the cleanup declaration could not be read"
			}
			add(CodeClosingCleanupReport, SeverityPending, detail)
		}
		if !completion.Metadata {
			add(CodeClosingMetadata, SeverityPending, "the labels or milestones do not match the projected stage")
		}
		if !completion.IssueClosed {
			add(CodeClosingIssue, SeverityPending, "the issue is still open")
		}
	}

	return finish(report, blockers)
}

// finish merges the severities, renders the next actions and returns the
// report. next_actions is empty exactly when the task is ready.
func finish(report Report, blockers []Blocker) Report {
	severity := SeverityReady
	report.Blockers = make([]Blocker, 0, len(blockers))
	seenCode := map[string]bool{}
	actions := []string{}
	seenAction := map[string]bool{}
	for _, blocker := range blockers {
		severity = Merge(severity, blocker.Severity)
		report.Blockers = append(report.Blockers, blocker)
		if seenCode[blocker.Code] {
			continue
		}
		seenCode[blocker.Code] = true
		action := actionFor(blocker)
		if action == "" || seenAction[action] {
			continue
		}
		seenAction[action] = true
		actions = append(actions, action)
	}
	report.Severity = severity
	report.NextActions = actions
	return report
}

// actionFor renders the human-facing next action of one blocker. Every blocker
// code has one, because a blocker without an action is only a complaint.
func actionFor(blocker Blocker) string {
	switch blocker.Code {
	case CodeRemoteUnknown:
		return "re-run without --offline and with a credential to read the remote facts"
	case CodeRemoteUnreadable:
		return "check the credential and the repository visibility, then re-run"
	case CodePRMissing:
		return "the developer pushes ghpipe/issue-N and opens a pull request whose body carries one standalone \"Closes #N\" line"
	case CodeAmbiguousAssociation:
		return "resolve the ambiguous pull request association by hand, then re-run"
	case CodeTaskBlocked:
		return "the orchestrator removes the ghpipe:blocked label once the blocker is resolved"
	case CodeDraft:
		return "the developer marks the pull request ready for review"
	case CodeCancelled:
		return "report the cancellation; keep the branch and the commits, do not develop further"
	case CodeHeadMissing:
		return "fix the checkout (git rev-parse HEAD must resolve) and re-run"
	case CodeHeadChanged:
		return "re-run status to bind to the new head SHA; the previous SHA is no longer the head"
	case CodeMergeState:
		return "resolve the native merge state (conflicts, required reviews, gates) and re-run"
	case CodeChecks:
		return "make the required checks pass on this SHA (see checks.items)"
	case CodeReviews:
		return "obtain an independent review decision of APPROVED for this exact SHA"
	case CodeClosingMainCI:
		return "make the required checks of the merge commit pass (see checks)"
	case CodeClosingRemoteAbsent:
		return "delete the remote development branch (or confirm delete_branch_on_merge ran)"
	case CodeClosingLocalCleanup:
		return "run ghpipe cleanup to remove the local branch and its tracking ref"
	case CodeClosingCleanupReport:
		return "run ghpipe cleanup so the delivery cleanup declaration is published"
	case CodeClosingMetadata:
		return "apply the metadata differences (labels and milestones) for this task"
	case CodeClosingIssue:
		return "close the issue"
	default:
		return ""
	}
}

// focusPR picks the pull request the report is about: the requested one, else
// the active one, else the merged one.
func focusPR(facts metadata.Facts) *metadata.PRFacts {
	if facts.TargetPR > 0 {
		return facts.PR(facts.TargetPR)
	}
	if active := facts.ActivePR(); active != nil {
		return active
	}
	return facts.MergedPR()
}

// stageOf projects the stage of the report's target object.
func stageOf(projection metadata.Projection, target Target) lifecycle.Stage {
	if target.Kind == string(metadata.ObjectPR) {
		for _, object := range projection.PRs {
			if object.Number == target.Number {
				return object.Stage
			}
		}
	}
	return projection.Issue.Stage
}

func reviewSummary(facts metadata.Facts) *Reviews {
	summary := &Reviews{Count: len(facts.Reviews)}
	if pr := focusPR(facts); pr != nil {
		summary.Decision = pr.ReviewDecision
	}
	for _, review := range facts.Reviews {
		switch review.State {
		case "APPROVED":
			summary.Approved++
		case "CHANGES_REQUESTED":
			summary.ChangesRequested++
		case "DISMISSED":
			summary.Dismissed++
		}
	}
	return summary
}

// mergeStateBlocker judges the native merge state. CLEAN and DRAFT are not
// blockers (a draft is reported as a draft); BEHIND and UNKNOWN still have to
// settle, so they are pending; anything else is a definite obstacle.
func mergeStateBlocker(pr *metadata.PRFacts) (Severity, string) {
	if pr.Draft {
		return SeverityReady, ""
	}
	switch pr.MergeStateStatus {
	case "", "CLEAN", "DRAFT":
		return SeverityReady, ""
	case "BEHIND", "UNKNOWN":
		return SeverityPending, "the native merge state is " + pr.MergeStateStatus
	default:
		return SeverityFailed, "the native merge state is " + pr.MergeStateStatus
	}
}

// reviewBlocker judges the review decision of an open pull request.
func reviewBlocker(pr *metadata.PRFacts) (Severity, string) {
	switch pr.ReviewDecision {
	case "APPROVED":
		return SeverityReady, ""
	case "CHANGES_REQUESTED":
		return SeverityFailed, "the review decision is CHANGES_REQUESTED"
	default:
		return SeverityPending, "the pull request has no APPROVED review decision yet"
	}
}

// checksBlocker judges the required checks of the pull request head.
func checksBlocker(remote *Remote) (Severity, string) {
	if remote.ChecksError != nil {
		return SeverityUnknown, "the required checks could not be read"
	}
	if len(remote.Checks.Items) == 0 {
		return SeverityReady, ""
	}
	switch remote.Checks.State() {
	case checks.StateReady:
		return SeverityReady, ""
	case checks.StatePending:
		return SeverityPending, summarize(remote.Checks, "not finished")
	case checks.StateUnknown:
		return SeverityUnknown, summarize(remote.Checks, "unproven")
	default:
		return SeverityFailed, summarize(remote.Checks, "not successful")
	}
}

func summarize(evaluation checks.Evaluation, word string) string {
	counts := map[checks.State]int{}
	for _, item := range evaluation.Items {
		if item.State != checks.StateReady {
			counts[item.State]++
		}
	}
	states := []checks.State{checks.StateFailed, checks.StateUnknown, checks.StatePending}
	parts := make([]string, 0, len(states))
	for _, state := range states {
		if counts[state] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", counts[state], state))
		}
	}
	if len(parts) == 0 {
		return "the required checks are " + word
	}
	return "required checks " + word + ": " + strings.Join(parts, ", ")
}

func hasLabel(labels []string, want string) bool {
	for _, label := range labels {
		if label == want {
			return true
		}
	}
	return false
}
