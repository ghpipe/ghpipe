// Package lifecycle projects a task's lifecycle stage from GitHub facts.
//
// Projection is pure: no I/O, no clock, no stored state. The same facts always
// yield the same stage, which is exactly what lets every command re-derive the
// stage from the remote instead of persisting it (docs/design.md 2.2, 10.3).
//
// "blocked" is an overlay label, not a stage, and pending / failed / unknown
// are operation outcomes rather than lifecycle stages, so none of them is a
// Stage here.
package lifecycle

import "errors"

// Stage is one of the seven projected lifecycle stages.
type Stage string

// The lifecycle stages, in the order of docs/design.md 2.2.
const (
	StageReady            Stage = "ready"
	StageActive           Stage = "active"
	StageReview           Stage = "review"
	StageChangesRequested Stage = "changes-requested"
	StageClosing          Stage = "closing"
	StageDone             Stage = "done"
	StageCancelled        Stage = "cancelled"
)

// LabelPrefix namespaces every label ghpipe manages.
const LabelPrefix = "ghpipe:"

// BlockedLabel is the overlay label. It is preserved next to the stage label
// and never replaced by it.
const BlockedLabel = LabelPrefix + "blocked"

// StageLabel returns the managed label for s, or "" when s is not a known
// stage. Returning "" rather than inventing a label keeps an unknown stage from
// ever reaching GitHub as ghpipe:.
func StageLabel(s Stage) string {
	switch s {
	case StageReady, StageActive, StageReview, StageChangesRequested, StageClosing, StageDone, StageCancelled:
		return LabelPrefix + string(s)
	default:
		return ""
	}
}

// Issue is the subset of Issue facts the projection needs.
type Issue struct {
	Number int
	// Closed is true once the issue state is CLOSED, for any reason.
	Closed bool
}

// PR is the subset of pull request facts the projection needs. Callers pass
// only PRs that are associated with the issue (same repository, base = default
// branch, a single standalone "Closes #N"): association is a fact-collection
// concern, not a projection concern.
type PR struct {
	Number int
	// Draft is the native isDraft flag.
	Draft bool
	// Merged is the native merged flag. A merged PR is always also closed.
	Merged bool
	// Closed is true once the state is CLOSED, merged or not.
	Closed bool
}

// ReviewState is a native review state.
type ReviewState string

// Review states. Only CHANGES_REQUESTED moves a stage; a dismissed review is
// treated as absent.
const (
	ReviewApproved         ReviewState = "APPROVED"
	ReviewChangesRequested ReviewState = "CHANGES_REQUESTED"
	ReviewCommented        ReviewState = "COMMENTED"
	ReviewDismissed        ReviewState = "DISMISSED"
)

// Review is one review of the PR being projected.
type Review struct {
	State ReviewState
}

// Completion records the closing facts of a merged task. Each field is an
// independently observed fact and the projection never infers one from another:
// a missing fact keeps the task in closing, because "not proven" and "proven
// absent" are different answers (docs/design.md 2.2, 8.5).
type Completion struct {
	// MergeCI is "the required checks bound to the merge commit succeeded".
	MergeCI bool
	// Metadata is "stage label and milestone match the projected facts".
	Metadata bool
	// RemoteBranchAbsent is "ls-remote advertised no head for the branch".
	RemoteBranchAbsent bool
	// LocalRefsAbsent is "the local branch and its tracking ref are gone".
	LocalRefsAbsent bool
	// CleanupReport is "a scope-matching Delivery App cleanup declaration
	// exists".
	CleanupReport bool
	// IssueClosed is "the issue itself is closed".
	IssueClosed bool
}

// Complete reports whether every closing fact holds.
func (c Completion) Complete() bool {
	return c.MergeCI && c.Metadata && c.RemoteBranchAbsent && c.LocalRefsAbsent && c.CleanupReport && c.IssueClosed
}

// ErrMultipleActivePRs means the issue has more than one open PR. ghpipe never
// guesses which one is the task, so the ambiguity is reported instead of
// projected.
var ErrMultipleActivePRs = errors.New("lifecycle: more than one active pull request is associated with the issue")

// PRStage projects the stage of one PR from that PR's own facts alone:
//
//	merged with every closing fact   -> done
//	merged with a fact missing       -> closing
//	closed unmerged                  -> cancelled
//	draft                            -> active
//	changes requested (not dismissed)-> changes-requested
//	otherwise                        -> review
//
// The order matters: a draft is active even when a stale review asks for
// changes, and a merged PR is never judged by its reviews.
//
// It is the per-PR half of the projection, and it is exported because the
// metadata slice has to label a PR it already knows about without re-deriving
// the whole task. It sees no issue, no branch and no sibling PR, so it cannot
// express the issue-level rules of docs/design.md 10.3 - an issue left open
// behind a merged PR stays in closing, and a closed issue without a merged PR
// is cancelled whatever its PRs say. Plan applies those rules and returns one
// stage per object; prefer Plan when both the issue and its PRs are being
// labelled, so the two objects can never disagree.
func PRStage(pr PR, reviews []Review, completion Completion) Stage {
	switch {
	case pr.Merged && completion.Complete():
		return StageDone
	case pr.Merged:
		return StageClosing
	case pr.Closed:
		return StageCancelled
	case pr.Draft:
		return StageActive
	case changesRequested(reviews):
		return StageChangesRequested
	default:
		return StageReview
	}
}

// ActivePR returns the single open, unmerged PR, or nil when there is none.
// More than one is an error: one issue has exactly one active PR, and an
// ambiguous association must never be resolved by picking one.
func ActivePR(prs []PR) (*PR, error) {
	var found *PR
	for i := range prs {
		if prs[i].Merged || prs[i].Closed {
			continue
		}
		if found != nil {
			return nil, ErrMultipleActivePRs
		}
		found = &prs[i]
	}
	return found, nil
}

// MergedPR returns the merged PR with the highest number, or nil when the issue
// never had one. Highest number means most recently created, which is the PR a
// later re-work would have produced.
func MergedPR(prs []PR) *PR {
	var found *PR
	for i := range prs {
		if !prs[i].Merged {
			continue
		}
		if found == nil || prs[i].Number > found.Number {
			found = &prs[i]
		}
	}
	return found
}

// Plan projects one stage per object of a task: the issue itself and every PR
// associated with it. Metadata sync compares labels object by object, so the
// projection must answer for each of them at once (docs/design.md 10.3): the
// returned map is keyed by issue number and by the number of every PR in prs,
// and always holds exactly len(prs)+1 entries. Issue and PR numbers share
// GitHub's per-repository sequence, so the keys cannot collide.
//
// The issue entry implements the table in docs/design.md 10.3, in that
// precedence order:
//
//	issue closed, a PR merged        -> that PR's stage (done / closing)
//	issue closed, nothing merged     -> cancelled
//	an active PR                     -> that PR's stage
//	nothing active, a PR merged      -> closing (the issue is still open)
//	no PR at all, a branch exists    -> active
//	no PR at all, no branch          -> ready
//
// Every PR entry is that PR's own PRStage, including PRs the issue-level rules
// ignore: a PR that was closed unmerged is cancelled even while the issue it
// belonged to stays active. PRStage exists for callers that need exactly one
// PR; Plan is what keeps a whole task's labels consistent.
//
// branchExists is a fact read from the remote, not an assumption derived from
// the local checkout: a claim on another machine still makes a task active.
//
// completions is keyed by PR number and holds the closing facts of merged PRs;
// a missing entry means "nothing proven yet", which projects as closing.
//
// An ambiguous association (more than one active PR) yields a nil map and
// ErrMultipleActivePRs: a partial projection would let the sync write labels
// derived from a guess.
func Plan(issue Issue, prs []PR, reviews []Review, branchExists bool, completions map[int]Completion) (map[int]Stage, error) {
	// Ambiguity is checked first, whatever else the facts say: refusing to
	// project an ambiguous association is a rule, not a branch of the table.
	active, err := ActivePR(prs)
	if err != nil {
		return nil, err
	}
	stages := make(map[int]Stage, len(prs)+1)
	for _, pr := range prs {
		stages[pr.Number] = PRStage(pr, reviews, completions[pr.Number])
	}
	merged := MergedPR(prs)
	switch {
	case issue.Closed && merged != nil:
		stages[issue.Number] = stages[merged.Number]
	case issue.Closed:
		stages[issue.Number] = StageCancelled
	case active != nil:
		stages[issue.Number] = stages[active.Number]
	case merged != nil:
		// The merge did not close the issue (a missing "Closes #N" is a real
		// defect), so the task stays in closing rather than being papered over.
		stages[issue.Number] = StageClosing
	case branchExists:
		stages[issue.Number] = StageActive
	default:
		stages[issue.Number] = StageReady
	}
	return stages, nil
}

// changesRequested reports whether any review still asks for changes. A
// dismissed review no longer counts; APPROVED and COMMENTED do not decide a
// stage on their own.
func changesRequested(reviews []Review) bool {
	for _, review := range reviews {
		if review.State == ReviewChangesRequested {
			return true
		}
	}
	return false
}
