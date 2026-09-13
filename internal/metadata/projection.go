package metadata

import (
	"fmt"
	"sort"
	"strings"

	"github.com/ghpipe/ghpipe/internal/lifecycle"
)

// ObjectKind names the two kinds of object a task projects onto.
type ObjectKind string

// The object kinds. Issue and pull request numbers share GitHub's
// per-repository sequence, so the kind is always carried next to the number.
const (
	ObjectIssue ObjectKind = "issue"
	ObjectPR    ObjectKind = "pr"
)

// Object is the desired state of one object: the projected stage, the complete
// expected label set, and the milestone it must carry.
type Object struct {
	Kind   ObjectKind      `json:"kind"`
	Number int             `json:"number"`
	Stage  lifecycle.Stage `json:"stage"`
	// Labels is the complete expected label set, sorted: exactly one stage
	// label, the preserved overlays, and for a pull request the non-stage
	// labels inherited from the issue.
	Labels []string `json:"labels"`
	// Milestone is the milestone the object must carry. It is always empty for
	// a pull request: an issue holds the delivery milestone, and counting it
	// twice would double the progress (docs/design.md 2.3).
	Milestone string `json:"milestone"`
}

// Projection is the desired state of one task: the issue and every associated
// pull request, each with its projected stage.
type Projection struct {
	Issue Object   `json:"issue"`
	PRs   []Object `json:"prs"`
}

// Project derives the desired stage and label set of every object of a task.
//
// The stages come from lifecycle.Plan, so the issue and its pull requests can
// never disagree, and the label sets are derived from the same stages: the
// stage label is exclusive, `ghpipe:blocked` survives as an overlay, and a
// pull request inherits the issue's non-stage labels (docs/design.md 2.3,
// 10.3).
//
// Project is pure. It writes nothing and, in particular, it is the only place
// that decides what the labels should be - Differences only reports how the
// current facts deviate from it.
func Project(f Facts) (Projection, error) {
	stages, err := lifecycle.Plan(
		f.LifecycleIssue(),
		f.LifecyclePRs(),
		f.EffectiveReviews(),
		f.BranchExists,
		f.Completions,
	)
	if err != nil {
		return Projection{}, err
	}

	issueStage, err := stageOf(stages, f.Issue.Number, ObjectIssue)
	if err != nil {
		return Projection{}, err
	}
	projection := Projection{
		Issue: Object{
			Kind:      ObjectIssue,
			Number:    f.Issue.Number,
			Stage:     issueStage,
			Labels:    issueLabels(f.Issue.Labels, issueStage),
			Milestone: f.Issue.Milestone,
		},
		PRs: make([]Object, 0, len(f.PRs)),
	}
	for _, pr := range f.PRs {
		stage, err := stageOf(stages, pr.Number, ObjectPR)
		if err != nil {
			return Projection{}, err
		}
		projection.PRs = append(projection.PRs, Object{
			Kind:   ObjectPR,
			Number: pr.Number,
			Stage:  stage,
			// A pull request's labels are derived from the issue's: same
			// overlays, same type/severity labels, its own stage.
			Labels:    issueLabels(f.Issue.Labels, stage),
			Milestone: "",
		})
	}
	return projection, nil
}

// stageOf looks the projected stage up. lifecycle.Plan always answers for every
// object it was given; an empty answer therefore means the two halves of the
// package disagree, which must surface as an error rather than as an object
// without a stage (an empty stage has no valid label).
func stageOf(stages map[int]lifecycle.Stage, number int, kind ObjectKind) (lifecycle.Stage, error) {
	stage, ok := stages[number]
	if !ok || lifecycle.StageLabel(stage) == "" {
		return "", fmt.Errorf("metadata: no stage was projected for %s #%d", kind, number)
	}
	return stage, nil
}

// issueLabels builds the expected label set: every non-stage label of the
// source plus the projected stage label. Stage labels are exclusive, so a
// stale stage label is dropped here and reported as a difference rather than
// kept next to the new one.
func issueLabels(source []string, stage lifecycle.Stage) []string {
	labels := make([]string, 0, len(source)+1)
	for _, label := range source {
		if isStageLabel(label) {
			continue
		}
		labels = append(labels, label)
	}
	labels = append(labels, lifecycle.StageLabel(stage))
	return normalizeLabels(labels)
}

// isStageLabel reports whether label is one of the seven ghpipe stage labels.
// `ghpipe:blocked` is deliberately not a stage label: it is an overlay that
// survives next to the stage.
func isStageLabel(label string) bool {
	for _, stage := range []lifecycle.Stage{
		lifecycle.StageReady, lifecycle.StageActive, lifecycle.StageReview,
		lifecycle.StageChangesRequested, lifecycle.StageClosing,
		lifecycle.StageDone, lifecycle.StageCancelled,
	} {
		if label == lifecycle.StageLabel(stage) {
			return true
		}
	}
	return false
}

// isManagedLabel reports whether ghpipe owns the label. Only managed labels may
// be added or removed by a sync; everything else is inherited information
// (docs/design.md 10.3).
func isManagedLabel(label string) bool { return strings.HasPrefix(label, lifecycle.LabelPrefix) }

func normalizeLabels(labels []string) []string {
	seen := make(map[string]bool, len(labels))
	out := make([]string, 0, len(labels))
	for _, label := range labels {
		if label == "" || seen[label] {
			continue
		}
		seen[label] = true
		out = append(out, label)
	}
	sort.Strings(out)
	return out
}

// ObjectDiff is one object's deviation from the desired state. Every field is a
// difference only: computing it never changes anything.
type ObjectDiff struct {
	Kind   ObjectKind      `json:"kind"`
	Number int             `json:"number"`
	Stage  lifecycle.Stage `json:"stage"`
	// AddLabels and RemoveLabels are the managed (`ghpipe:*`) labels the sync
	// may add and remove.
	AddLabels    []string `json:"add_labels"`
	RemoveLabels []string `json:"remove_labels"`
	// InheritLabels lists non-managed labels of the issue a pull request does
	// not carry yet. They are inherited, not managed: they are reported so the
	// operator sees them, and they are never silently written.
	InheritLabels []string `json:"inherit_labels,omitempty"`
	// MilestoneCleared is true when the object carries a milestone that the
	// projection says it must not have (pull requests only).
	MilestoneCleared bool `json:"milestone_cleared,omitempty"`
}

// DiffSet is the complete difference between the current facts and the
// projection.
type DiffSet struct {
	Issue ObjectDiff   `json:"issue"`
	PRs   []ObjectDiff `json:"prs"`
}

// Empty reports whether every object already matches the projection.
func (d DiffSet) Empty() bool {
	if !d.Issue.empty() {
		return false
	}
	for _, pr := range d.PRs {
		if !pr.empty() {
			return false
		}
	}
	return true
}

func (d ObjectDiff) empty() bool {
	return len(d.AddLabels) == 0 && len(d.RemoveLabels) == 0 &&
		len(d.InheritLabels) == 0 && !d.MilestoneCleared
}

// StageLabelsMatch reports whether the objects already carry the stage labels
// the projection expects, ignoring overlays, inherited labels and milestones.
// status uses it as one half of the "metadata matches the projected facts"
// closing fact.
func (d DiffSet) StageLabelsMatch() bool {
	for _, diff := range append([]ObjectDiff{d.Issue}, d.PRs...) {
		for _, label := range diff.AddLabels {
			if isStageLabel(label) {
				return false
			}
		}
		for _, label := range diff.RemoveLabels {
			if isStageLabel(label) {
				return false
			}
		}
	}
	return true
}

// MilestonesMatch reports whether no object carries a milestone the projection
// says it must not have. It is the second half of the metadata closing fact.
func (d DiffSet) MilestonesMatch() bool {
	for _, diff := range d.PRs {
		if diff.MilestoneCleared {
			return false
		}
	}
	return true
}

// Differences compares the current issue and pull request facts against the
// desired projection and returns only the differences.
//
// It is the task brief's "Differences(issue, prs, desired)": a pure function
// with no client, no clock and no side effect. The task brief requires it to
// compute and not write, so it takes the facts as values and the caller keeps
// ownership of them.
func Differences(issue IssueFacts, prs []PRFacts, desired Projection) (DiffSet, error) {
	var out DiffSet
	if desired.Issue.Number != issue.Number {
		return out, fmt.Errorf("metadata: the desired projection is for issue #%d, not #%d",
			desired.Issue.Number, issue.Number)
	}
	out.Issue = labelDiff(desired.Issue, issue.Labels, false)

	out.PRs = make([]ObjectDiff, 0, len(desired.PRs))
	for _, object := range desired.PRs {
		current := findPR(prs, object.Number)
		if current == nil {
			return DiffSet{}, fmt.Errorf("metadata: the projection names pull request #%d, which is not in the facts",
				object.Number)
		}
		diff := labelDiff(object, current.Labels, true)
		// A pull request must not carry a milestone: the issue holds it, and a
		// second copy would be counted twice (docs/design.md 2.3).
		diff.MilestoneCleared = current.Milestone != ""
		out.PRs = append(out.PRs, diff)
	}
	return out, nil
}

func findPR(prs []PRFacts, number int) *PRFacts {
	for i := range prs {
		if prs[i].Number == number {
			return &prs[i]
		}
	}
	return nil
}

// labelDiff compares one object's labels with the desired set. onlyManaged is
// true for pull requests, where a missing non-managed label is an inherited
// label rather than a managed one.
func labelDiff(desired Object, current []string, onlyManaged bool) ObjectDiff {
	have := make(map[string]bool, len(current))
	for _, label := range current {
		have[label] = true
	}
	want := make(map[string]bool, len(desired.Labels))
	diff := ObjectDiff{Kind: desired.Kind, Number: desired.Number, Stage: desired.Stage}
	for _, label := range desired.Labels {
		want[label] = true
		if have[label] {
			continue
		}
		switch {
		case isManagedLabel(label):
			diff.AddLabels = append(diff.AddLabels, label)
		case onlyManaged:
			diff.InheritLabels = append(diff.InheritLabels, label)
		}
	}
	for _, label := range current {
		if want[label] || !isManagedLabel(label) {
			continue
		}
		diff.RemoveLabels = append(diff.RemoveLabels, label)
	}
	diff.AddLabels = normalizeLabels(diff.AddLabels)
	diff.RemoveLabels = normalizeLabels(diff.RemoveLabels)
	diff.InheritLabels = normalizeLabels(diff.InheritLabels)
	return diff
}
