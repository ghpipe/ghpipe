package status

import (
	"context"
	"errors"
	"time"

	"github.com/ghpipe/ghpipe/internal/checks"
	"github.com/ghpipe/ghpipe/internal/github"
	"github.com/ghpipe/ghpipe/internal/gitx"
	"github.com/ghpipe/ghpipe/internal/lifecycle"
	"github.com/ghpipe/ghpipe/internal/metadata"
)

// Options selects the task and the sources Collect reads.
type Options struct {
	Target        Target
	Role          string
	Offline       bool
	Dir           string
	Repository    string
	DefaultBranch string
	Required      []checks.Required
	MaxPages      int
	// Git reads the checkout. A nil value means the real git binary.
	Git *gitx.Client
	// Client is the GitHub transport. It is required unless Offline is set,
	// and it is never touched in offline mode: that is the point of offline
	// mode (no credential, no request).
	Client *github.Client
}

// Collect reads one task's status input: the local checkout first, then - only
// when not offline - the remote facts.
//
// A remote failure is not returned as an error: it is part of the status, and
// the report has to say *why* the facts are unknown. Only a broken invocation
// (no client in online mode) is an error.
func Collect(ctx context.Context, opts Options) (Input, error) {
	in := Input{
		Target:     opts.Target,
		Role:       opts.Role,
		ObservedAt: time.Now().UTC().Format(time.RFC3339),
	}
	git := opts.Git
	if git == nil {
		git = gitx.New(nil)
	}
	dir := opts.Dir

	// Local facts are read first: they are available offline, and a broken
	// checkout must be reported even when the remote is healthy.
	if branch, err := git.CurrentBranch(ctx, dir); err == nil {
		in.Local.Branch = branch
	}
	if head, err := git.HeadSHA(ctx, dir); err == nil {
		in.Local.Head = head
	}
	if dirty, err := git.IsDirty(ctx, dir); err == nil {
		in.Local.Dirty = dirty
	}
	if shallow, err := git.IsShallow(ctx, dir); err == nil {
		in.Local.Shallow = shallow
	}

	issue := 0
	pr := 0
	switch opts.Target.Kind {
	case "issue":
		issue = opts.Target.Number
	case "pr":
		pr = opts.Target.Number
	}

	if opts.Offline {
		// Offline mode stops here on purpose: no credential is resolved, no
		// request is made, and every remote fact stays unknown.
		applyLocalTaskBranch(ctx, git, dir, &in, issue)
		in.Problem = &Problem{
			Offline:  true,
			Code:     CodeRemoteUnknown,
			Severity: SeverityUnknown,
			Detail:   "offline: no credential was read and no request was made",
		}
		return in, nil
	}
	if opts.Client == nil {
		return Input{}, errors.New("status: an online read needs a client")
	}

	facts, err := metadata.Collect(ctx, opts.Client, metadata.Options{
		Repository: opts.Repository,
		Issue:      issue,
		PR:         pr,
		MaxPages:   opts.MaxPages,
	})
	if err != nil {
		in.Problem = classify(err)
		return in, nil
	}
	applyLocalTaskBranch(ctx, git, dir, &in, facts.Issue.Number)

	remote := &Remote{Facts: facts}
	active := facts.ActivePR()
	merged := facts.MergedPR()
	sha, branch := "", facts.DefaultBranch
	switch {
	case merged != nil:
		// A merged task's checks are bound to the merge commit, never to the
		// pull request head: a later green main run must not hide a red merge
		// (docs/design.md 10.1).
		sha = merged.MergeCommitSHA
	case active != nil:
		sha = active.HeadSHA
	}
	if sha != "" {
		evaluation, err := checks.Evaluate(ctx, opts.Client, checks.Request{
			Repository: facts.Repository,
			SHA:        sha,
			Branch:     branch,
			Required:   opts.Required,
			MaxPages:   opts.MaxPages,
		})
		remote.Checks = evaluation
		remote.ChecksError = err
	}
	if merged != nil {
		if head, ok, err := metadata.BranchHead(ctx, opts.Client, facts.Repository, facts.DefaultBranch); err == nil && ok {
			remote.DefaultBranchHead = head
		}
		declared, err := metadata.CleanupDeclared(ctx, opts.Client, facts.Repository, merged.Number)
		remote.CleanupReport = declared
		remote.CleanupError = err
	}
	if active != nil {
		// Last read of the command: if the head moved, every statement above
		// is about a commit that is no longer the head.
		head, err := metadata.PRHead(ctx, opts.Client, facts.Repository, active.Number)
		remote.CurrentPRHead = head
		remote.HeadRereadError = err
	}
	facts.Completions = completions(facts, remote, in.Local)
	remote.Facts = facts
	in.Remote = remote
	return in, nil
}

// applyLocalTaskBranch records whether the task branch and its tracking ref
// still exist locally. A failed read leaves the question open rather than
// answering "absent": claiming that a branch is gone when git could not be
// asked is exactly the kind of unread evidence done must never rest on.
func applyLocalTaskBranch(ctx context.Context, git *gitx.Client, dir string, in *Input, issue int) {
	if issue <= 0 {
		return
	}
	branch := metadata.TaskBranch(issue)
	in.Local.TaskBranch = branch
	if _, ok, err := git.RefSHA(ctx, dir, "refs/heads/"+branch); err != nil {
		in.Local.TaskBranchErr = err
	} else {
		in.Local.TaskBranchLocal = ok
	}
	if _, ok, err := git.RefSHA(ctx, dir, "refs/remotes/origin/"+branch); err != nil {
		in.Local.TaskBranchErr = err
	} else {
		in.Local.TaskBranchTracking = ok
	}
}

// completions derives the closing facts of every merged pull request.
//
// The metadata fact is computed with a probe: the labels are compared against
// the projection that would hold if every closing fact were already proven.
// Asking the other way round would be circular (the stage decides the label,
// and a label mismatch decides the stage), and the probe answers the question
// that actually matters: "are the labels already those of a finished task?".
func completions(facts metadata.Facts, remote *Remote, local Local) map[int]lifecycle.Completion {
	out := map[int]lifecycle.Completion{}
	merged := facts.MergedPR()
	if merged == nil {
		return out
	}
	out[merged.Number] = lifecycle.Completion{
		MergeCI:            remote.ChecksError == nil && remote.Checks.State() == checks.StateReady && len(remote.Checks.Items) > 0,
		Metadata:           metadataMatches(facts, merged.Number),
		RemoteBranchAbsent: !facts.BranchExists,
		LocalRefsAbsent:    local.LocalRefsAbsent(),
		CleanupReport:      remote.CleanupReport,
		IssueClosed:        facts.Issue.Closed,
	}
	return out
}

func metadataMatches(facts metadata.Facts, number int) bool {
	probe := map[int]lifecycle.Completion{
		number: {
			MergeCI:            true,
			Metadata:           true,
			RemoteBranchAbsent: true,
			LocalRefsAbsent:    true,
			CleanupReport:      true,
			IssueClosed:        true,
		},
	}
	probed := facts
	probed.Completions = probe
	projection, err := metadata.Project(probed)
	if err != nil {
		return false
	}
	differences, err := metadata.Differences(probed.Issue, probed.PRs, projection)
	if err != nil {
		return false
	}
	return differences.StageLabelsMatch() && differences.MilestonesMatch()
}

// classify turns a failed remote read into the report's problem. A definite
// refusal (the credential or the repository) is a failure; anything that could
// not be trusted to be complete is unknown.
func classify(err error) *Problem {
	problem := &Problem{
		Code:     CodeRemoteUnknown,
		Severity: SeverityUnknown,
		Detail:   "the remote facts could not be read",
	}
	switch {
	case errors.Is(err, metadata.ErrAmbiguousAssociation),
		errors.Is(err, metadata.ErrMultipleClosingReferences),
		errors.Is(err, metadata.ErrCrossRepoReference):
		problem.Code = CodeAmbiguousAssociation
		problem.Severity = SeverityFailed
		problem.Detail = "the issue and its pull requests cannot be associated unambiguously"
		return problem
	case errors.Is(err, metadata.ErrUnassociatedPR):
		problem.Code = CodePRMissing
		problem.Severity = SeverityFailed
		problem.Detail = "the requested pull request carries no unique closing reference"
		return problem
	case errors.Is(err, metadata.ErrNotFound):
		problem.Code = CodeRemoteUnreadable
		problem.Severity = SeverityFailed
		problem.Detail = "the object does not exist or is not visible to this credential"
		return problem
	}
	if e, ok := github.As(err); ok {
		switch {
		case e.Status == 401 || e.Status == 403 || e.Status == 404, e.Category == github.CategoryRateLimited:
			problem.Code = CodeRemoteUnreadable
			problem.Severity = SeverityFailed
			problem.Detail = "the remote refused the read (" + string(e.Kind) + ")"
		case e.Kind == github.KindContract, e.Kind == github.KindIncomplete, e.Kind == github.KindDecode:
			problem.Detail = "the remote answer was not trustworthy (" + string(e.Kind) + ")"
		default:
			problem.Detail = "the remote facts could not be read (" + string(e.Kind) + ")"
		}
		return problem
	}
	problem.Detail = "the remote facts could not be read"
	return problem
}
