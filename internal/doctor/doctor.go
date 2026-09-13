// Package doctor answers "is this checkout ready for the step I am about to
// take?".
//
// This slice implements the local half of docs/design.md 10.2: `--offline
// --for plan|develop|review`. Offline means exactly that - no credential is
// read, no request is made, no project command is run - so every answer here is
// about the machine the command runs on.
//
// A passing preflight is not acceptance. The design requires the output to say
// so, and this package says it in a field rather than in prose a caller would
// have to parse.
package doctor

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ghpipe/ghpipe/internal/checks"
	"github.com/ghpipe/ghpipe/internal/gitx"
	"github.com/ghpipe/ghpipe/internal/project"
	"github.com/ghpipe/ghpipe/internal/version"
)

// ErrUnsupported means the requested preflight is not implemented by this
// slice. It is a precondition failure: nothing was read and nothing was run.
var ErrUnsupported = errors.New("doctor: unsupported preflight")

// Check statuses.
const (
	StatusPass = "pass"
	StatusFail = "fail"
)

// Check is one preflight result.
type Check struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	// Detail is a short local explanation. It never contains a token or a
	// remote message.
	Detail string `json:"detail,omitempty"`
}

// BusinessAcceptance is always false here: a preflight proves the local
// environment, never that the work is acceptable (docs/design.md 10.2, 14.3).
type BusinessAcceptance struct {
	Evaluated bool `json:"evaluated"`
}

// Report is one doctor run.
type Report struct {
	For                string             `json:"for"`
	Offline            bool               `json:"offline"`
	Checks             []Check            `json:"checks"`
	Notice             string             `json:"notice"`
	BusinessAcceptance BusinessAcceptance `json:"business_acceptance"`
}

// Passed reports whether every check passed.
func (r Report) Passed() bool {
	for _, check := range r.Checks {
		if check.Status != StatusPass {
			return false
		}
	}
	return true
}

// Options selects the preflight.
type Options struct {
	// For is one of "plan", "develop", "review".
	For string
	// Offline must be true: this slice only implements the local preflight.
	Offline bool
	// Dir is the checkout directory to inspect.
	Dir string
	// Config is the discovered project configuration. It may be nil when the
	// project could not be loaded, in which case the configuration check
	// fails.
	Config *project.Config
	// Git reads the checkout. A nil value means the real git binary.
	Git *gitx.Client
}

// Offline runs the local preflight.
//
// The flags are validated here rather than silently ignored: asking for a
// preflight this slice does not implement must fail loudly, not report a
// cheerful green for checks that never ran (docs/design.md 4.5.1: 明确报错 +
// 给出替代做法).
func Offline(ctx context.Context, opts Options) (Report, error) {
	report := Report{
		For:                opts.For,
		Offline:            true,
		Notice:             "this is a local preflight, not business acceptance",
		BusinessAcceptance: BusinessAcceptance{Evaluated: false},
	}
	switch opts.For {
	case "plan", "develop", "review":
	default:
		return report, fmt.Errorf("%w: --for %q (this slice implements plan, develop and review)",
			ErrUnsupported, opts.For)
	}
	if !opts.Offline {
		return report, fmt.Errorf("%w: only --offline is implemented; online doctor (connectivity, bootstrap, handoff) is not part of this slice",
			ErrUnsupported)
	}

	report.Checks = append(report.Checks, checkVersion())
	report.Checks = append(report.Checks, checkConfiguration(opts.Config))
	report.Checks = append(report.Checks, checkCommands(opts.Config))
	if opts.For == "develop" || opts.For == "review" {
		report.Checks = append(report.Checks, checkBranchGuard(ctx, opts)...)
	}
	return report, nil
}

// checkVersion reports the CLI version. The resource version is a constant of
// the embedded resources and lands with the resources slice; pretending to
// compare it here would be a check that cannot fail.
func checkVersion() Check {
	if strings.TrimSpace(version.Version) == "" {
		return Check{Name: "cli_version", Status: StatusFail, Detail: "the binary reports no version"}
	}
	return Check{Name: "cli_version", Status: StatusPass, Detail: version.String()}
}

// checkConfiguration re-validates the project configuration. project.Load
// already validated it; a caller that hands over an unvalidated Config (a test,
// or a future command that builds one) gets the same answer here.
func checkConfiguration(cfg *project.Config) Check {
	if cfg == nil {
		return Check{Name: "configuration", Status: StatusFail, Detail: "no ghpipe configuration was loaded"}
	}
	if err := cfg.Validate(); err != nil {
		return Check{Name: "configuration", Status: StatusFail, Detail: err.Error()}
	}
	if strings.TrimSpace(cfg.Repository) == "" {
		return Check{Name: "configuration", Status: StatusFail, Detail: "repository is not configured"}
	}
	if strings.TrimSpace(cfg.DefaultBranch) == "" {
		return Check{Name: "configuration", Status: StatusFail, Detail: "default_branch is not configured"}
	}
	required := make([]checks.Required, 0, len(cfg.CI.RequiredChecks))
	for _, check := range cfg.CI.RequiredChecks {
		required = append(required, checks.Required{Context: check.Context, IntegrationID: check.IntegrationID})
	}
	if _, err := checks.Unique(required); err != nil {
		return Check{Name: "configuration", Status: StatusFail, Detail: err.Error()}
	}
	return Check{Name: "configuration", Status: StatusPass, Detail: cfg.Repository + " on " + cfg.DefaultBranch}
}

// checkCommands validates the registered native commands. They are executed as
// argv arrays, never through a shell, so a missing or empty argv is a
// configuration defect rather than a runtime surprise.
func checkCommands(cfg *project.Config) Check {
	if cfg == nil {
		return Check{Name: "commands", Status: StatusFail, Detail: "no ghpipe configuration was loaded"}
	}
	names := cfg.CommandNames()
	for _, name := range names {
		command := cfg.Commands[name]
		if len(command.Argv) == 0 {
			return Check{Name: "commands", Status: StatusFail, Detail: fmt.Sprintf("commands[%q].argv is empty", name)}
		}
		if !project.IsProjectRelative(command.Cwd) {
			return Check{Name: "commands", Status: StatusFail, Detail: fmt.Sprintf("commands[%q].cwd is not a project-relative path", name)}
		}
		for _, arg := range command.Argv {
			if arg == "" {
				return Check{Name: "commands", Status: StatusFail, Detail: fmt.Sprintf("commands[%q].argv has an empty entry", name)}
			}
		}
	}
	if len(names) == 0 {
		return Check{Name: "commands", Status: StatusPass, Detail: "no project command is registered"}
	}
	return Check{Name: "commands", Status: StatusPass, Detail: strings.Join(names, ", ")}
}

// checkBranchGuard runs the local branch checks of docs/design.md 8.2. The real
// enforcement is the GitHub ruleset; this only stops the mistakes that are
// visible locally before anything is written.
//
// develop: the developer must be on a branch (a detached HEAD has nowhere to
// commit) and must not carry uncommitted changes on the default branch.
// review: the tree must be clean, because a review is bound to an exact SHA and
// local edits would make the evidence unreproducible. Detached and default
// branch checkouts are explicitly allowed for read-only review.
func checkBranchGuard(ctx context.Context, opts Options) []Check {
	git := opts.Git
	if git == nil {
		git = gitx.New(nil)
	}
	head, err := git.HeadSHA(ctx, opts.Dir)
	if err != nil {
		return []Check{
			{Name: "checkout", Status: StatusFail, Detail: "HEAD could not be resolved: " + err.Error()},
			{Name: "branch_guard", Status: StatusFail, Detail: "the checkout could not be read"},
		}
	}
	results := []Check{{Name: "checkout", Status: StatusPass, Detail: head}}

	branch, err := git.CurrentBranch(ctx, opts.Dir)
	if err != nil {
		results = append(results, Check{Name: "branch_guard", Status: StatusFail, Detail: "the current branch could not be read"})
		return results
	}
	dirty, err := git.IsDirty(ctx, opts.Dir)
	if err != nil {
		results = append(results, Check{Name: "branch_guard", Status: StatusFail, Detail: "the working tree state could not be read"})
		return results
	}
	shallow, err := git.IsShallow(ctx, opts.Dir)
	if err == nil && shallow {
		// A shallow clone makes the attribution boundary unanswerable, which
		// the push path refuses later; saying it here is cheaper.
		results = append(results, Check{Name: "attribution_boundary", Status: StatusFail,
			Detail: "the checkout is a shallow clone; attribution.required cannot be verified"})
	}
	defaultBranch := ""
	if opts.Config != nil {
		defaultBranch = opts.Config.DefaultBranch
	}

	switch opts.For {
	case "develop":
		if branch == "" {
			return append(results, Check{Name: "branch_guard", Status: StatusFail,
				Detail: "HEAD is detached: development needs a branch"})
		}
		if dirty && defaultBranch != "" && branch == defaultBranch {
			return append(results, Check{Name: "branch_guard", Status: StatusFail,
				Detail: "the default branch has uncommitted changes; commit or stash them before branching"})
		}
		detail := "on " + branch
		if dirty {
			detail += " (working tree has uncommitted changes)"
		}
		return append(results, Check{Name: "branch_guard", Status: StatusPass, Detail: detail})
	default:
		if dirty {
			return append(results, Check{Name: "branch_guard", Status: StatusFail,
				Detail: "the working tree has uncommitted changes; a review is bound to an exact SHA"})
		}
		detail := "clean working tree"
		if branch == "" {
			detail += ", detached HEAD (supported for read-only review)"
		} else {
			detail += ", on " + branch
		}
		return append(results, Check{Name: "branch_guard", Status: StatusPass, Detail: detail})
	}
}
