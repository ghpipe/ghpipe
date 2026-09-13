package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/ghpipe/ghpipe/internal/checks"
	"github.com/ghpipe/ghpipe/internal/doctor"
	"github.com/ghpipe/ghpipe/internal/github"
	"github.com/ghpipe/ghpipe/internal/gitx"
	"github.com/ghpipe/ghpipe/internal/metadata"
	"github.com/ghpipe/ghpipe/internal/project"
	"github.com/ghpipe/ghpipe/internal/result"
	"github.com/ghpipe/ghpipe/internal/status"
)

// Deps holds every seam the commands need. Production passes the defaults; a
// test substitutes the transport, the credential source and the git runner, so
// no test needs a network, a credential file or a real repository.
type Deps struct {
	Streams Streams
	// HTTP is the transport every GitHub call uses. nil means the production
	// client.
	HTTP *http.Client
	// Token resolves the read-only credential of one role. It is only called
	// by a command that needs the network - --offline must never reach it -
	// and nil means DefaultToken.
	Token func(repository, role string) (string, error)
	// Git runs git. nil means the real binary.
	Git gitx.Runner
	// Dir is the checkout directory. "" means the process working directory.
	Dir string
}

func (d Deps) streams() Streams {
	streams := d.Streams
	if streams.Out == nil {
		streams.Out = os.Stdout
	}
	if streams.Err == nil {
		streams.Err = os.Stderr
	}
	return streams
}

func (d Deps) dir() (string, error) {
	if d.Dir != "" {
		return d.Dir, nil
	}
	return os.Getwd()
}

func (d Deps) git() *gitx.Client { return gitx.New(d.Git) }

// roles are the two read-only credentials a handoff state can be read with.
func validRole(role string) bool { return role == "developer" || role == "delivery" }

// DefaultToken resolves the read-only credential of one role.
//
// internal/credentials and internal/trust are not part of this slice, so this
// is the minimal seam: the token file lives outside the checkout at
// <user config dir>/ghpipe/tokens/<owner>/<repo>/<role>.token, the layout
// docs/design.md 4.4 fixes. It is only reached by an online command, and the
// error names the logical path instead of the host path so a report never
// carries a user directory.
func DefaultToken(repository, role string) (string, error) {
	if !validRole(role) {
		return "", fmt.Errorf("role %q has no read-only credential", role)
	}
	owner, name, ok := splitRepositoryName(repository)
	if !ok {
		return "", fmt.Errorf("repository %q is not OWNER/REPO", repository)
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", errors.New("the host configuration directory could not be resolved")
	}
	logical := filepath.Join("tokens", owner, name, role+".token")
	raw, err := os.ReadFile(filepath.Join(dir, "ghpipe", logical))
	if err != nil {
		return "", fmt.Errorf("no read-only credential for %s role %s (expected <config>/ghpipe/%s)",
			repository, role, filepath.ToSlash(logical))
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return "", fmt.Errorf("the credential for %s role %s is empty", repository, role)
	}
	return token, nil
}

func splitRepositoryName(repository string) (string, string, bool) {
	parts := strings.Split(strings.TrimSpace(repository), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	for _, part := range parts {
		for _, r := range part {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
				r == '.', r == '_', r == '-':
			default:
				return "", "", false
			}
		}
	}
	return strings.ToLower(parts[0]), strings.ToLower(parts[1]), true
}

// newClient builds the read-only GitHub client. The token is resolved here and
// nowhere else, which is what makes "offline never reads a credential" a
// structural property rather than a promise.
func newClient(deps Deps, repository, role string) (*github.Client, error) {
	resolve := deps.Token
	if resolve == nil {
		resolve = DefaultToken
	}
	token, err := resolve(repository, role)
	if err != nil {
		return nil, err
	}
	options := []github.Option{github.WithToken(token)}
	if deps.HTTP != nil {
		options = append(options, github.WithHTTPClient(deps.HTTP))
	}
	return github.New(options...), nil
}

// loadProject discovers the project the way inspect does: the explicit
// --project directory first, then upward discovery from the checkout.
func loadProject(deps Deps, global Global) (*project.Project, error) {
	start := global.Project
	if start == "" {
		dir, err := deps.dir()
		if err != nil {
			return nil, err
		}
		start = dir
	}
	proj, err := project.Load(start)
	if err != nil {
		if discovered, discoverErr := project.Discover(start); discoverErr == nil {
			return discovered, nil
		}
		return nil, err
	}
	return proj, nil
}

// preconditionFailure reports a problem that stopped the command before it
// could do anything: bad usage, unusable configuration, a missing credential.
// Nothing was read and nothing was written, which is exactly exit code 3
// (docs/design.md 9.1).
func preconditionFailure(streams Streams, global Global, command, category, detail string) int {
	env := result.New(command, result.StatusFailed)
	env.Error = &result.Error{Category: category, Phase: "precondition", Detail: detail}
	emitEnvelope(env, global, streams)
	return result.ExitUsageError
}

// emitEnvelope writes the machine-readable envelope when --json was asked for.
func emitEnvelope(env *result.Envelope, global Global, streams Streams) {
	if global.JSON {
		if err := env.WriteJSON(streams.Out); err != nil {
			fmt.Fprintf(streams.Err, "cannot write result: %v\n", err)
		}
		return
	}
	if env.Error != nil {
		fmt.Fprintf(streams.Err, "%s\n", env.String())
	}
}

// statusFlags is the parsed invocation of `ghpipe status`.
type statusFlags struct {
	target  status.Target
	role    string
	offline bool
}

func parseStatusFlags(args []string) (statusFlags, error) {
	var out statusFlags
	flags, err := parseFlags(args, map[string]flagKind{
		"issue": valueFlag, "pr": valueFlag, "role": valueFlag, "offline": boolFlag,
	})
	if err != nil {
		return out, err
	}
	if len(flags.rest) > 0 {
		return out, fmt.Errorf("unexpected argument %q", flags.rest[0])
	}
	issue, pr := flags.value("issue"), flags.value("pr")
	switch {
	case issue == "" && pr == "":
		return out, errors.New("exactly one of --issue and --pr is required")
	case issue != "" && pr != "":
		return out, errors.New("--issue and --pr are mutually exclusive")
	}
	if issue != "" {
		number, err := positiveInt("issue", issue)
		if err != nil {
			return out, err
		}
		out.target = status.Target{Kind: "issue", Number: number}
	} else {
		number, err := positiveInt("pr", pr)
		if err != nil {
			return out, err
		}
		out.target = status.Target{Kind: "pr", Number: number}
	}
	out.role = flags.value("role")
	if !validRole(out.role) {
		return out, errors.New("--role must be developer or delivery")
	}
	out.offline = flags.has("offline")
	return out, nil
}

// runStatus reports the read-only handoff state.
func runStatus(deps Deps, global Global, args []string) int {
	streams := deps.streams()
	flags, err := parseStatusFlags(args)
	if err != nil {
		return preconditionFailure(streams, global, "status", "usage", err.Error())
	}
	proj, err := loadProject(deps, global)
	if err != nil {
		return preconditionFailure(streams, global, "status", "configuration", err.Error())
	}
	cfg := proj.Config
	if strings.TrimSpace(cfg.Repository) == "" {
		return preconditionFailure(streams, global, "status", "configuration",
			"the project configuration has no repository")
	}
	required := make([]checks.Required, 0, len(cfg.CI.RequiredChecks))
	for _, check := range cfg.CI.RequiredChecks {
		required = append(required, checks.Required{Context: check.Context, IntegrationID: check.IntegrationID})
	}
	if _, err := checks.Unique(required); err != nil {
		return preconditionFailure(streams, global, "status", "configuration", err.Error())
	}
	dir, err := deps.dir()
	if err != nil {
		return preconditionFailure(streams, global, "status", "precondition", err.Error())
	}

	options := status.Options{
		Target:        flags.target,
		Role:          flags.role,
		Offline:       flags.offline,
		Dir:           dir,
		Repository:    cfg.Repository,
		DefaultBranch: cfg.DefaultBranch,
		Required:      required,
		Git:           deps.git(),
	}
	if !flags.offline {
		// The only place a credential is resolved. In offline mode this branch
		// is not reachable, so no credential can be read (AC4).
		client, err := newClient(deps, cfg.Repository, flags.role)
		if err != nil {
			return preconditionFailure(streams, global, "status", "credential", err.Error())
		}
		options.Client = client
	}

	input, err := status.Collect(context.Background(), options)
	if err != nil {
		return preconditionFailure(streams, global, "status", "precondition", err.Error())
	}
	report := status.Assemble(input)

	env := result.New("status", result.Status(report.Severity))
	// One observation time for the whole answer: the envelope and the report
	// must not disagree about when the facts were observed.
	report.ObservedAt = env.ObservedAt
	env.Repository = cfg.Repository
	env.Target = &result.Target{Kind: flags.target.Kind, Number: flags.target.Number}
	env.Data = report
	// The envelope's next list and the report's next_actions carry the same
	// advice, so a caller that only reads the envelope still gets it.
	env.Next = report.NextActions
	if global.JSON {
		emitEnvelope(env, global, streams)
	} else {
		renderStatus(streams.Out, report)
	}
	// status uses the severity itself as the exit code contract: 0 means ready,
	// 1 means anything else. The design's generic 2 ("unknown") belongs to
	// write commands, where an unknown write must never look like a plain
	// failure (docs/design.md 10.1).
	if report.Severity == status.SeverityReady {
		return result.ExitOK
	}
	return result.ExitFailed
}

func renderStatus(w io.Writer, report status.Report) {
	fmt.Fprintf(w, "status: %s\n", report.Severity)
	fmt.Fprintf(w, "target: %s #%d\n", report.Target.Kind, report.Target.Number)
	branch := report.Checkout.Branch
	if branch == "" {
		branch = "(detached)"
	}
	fmt.Fprintf(w, "checkout: %s @ %s\n", branch, report.Checkout.Head)
	fmt.Fprintf(w, "role: %s\n", report.Role)
	fmt.Fprintf(w, "stage: %s\n", report.Stage)
	if report.Issue != nil {
		fmt.Fprintf(w, "issue: #%d %s\n", report.Issue.Number, report.Issue.State)
	}
	if report.PR != nil {
		fmt.Fprintf(w, "pr: #%d %s (head %s)\n", report.PR.Number, report.PR.State, report.PR.HeadSHA)
	}
	if len(report.Blockers) == 0 {
		fmt.Fprintln(w, "blockers: none")
	} else {
		fmt.Fprintln(w, "blockers:")
		for _, blocker := range report.Blockers {
			if blocker.Detail == "" {
				fmt.Fprintf(w, "  - %s (%s)\n", blocker.Code, blocker.Severity)
				continue
			}
			fmt.Fprintf(w, "  - %s (%s): %s\n", blocker.Code, blocker.Severity, blocker.Detail)
		}
	}
	if len(report.NextActions) > 0 {
		fmt.Fprintln(w, "next:")
		for _, action := range report.NextActions {
			fmt.Fprintf(w, "  - %s\n", action)
		}
	}
}

// runDoctor runs the local preflight.
func runDoctor(deps Deps, global Global, args []string) int {
	streams := deps.streams()
	flags, err := parseFlags(args, map[string]flagKind{"for": valueFlag, "offline": boolFlag})
	if err != nil {
		return preconditionFailure(streams, global, "doctor", "usage", err.Error())
	}
	if len(flags.rest) > 0 {
		return preconditionFailure(streams, global, "doctor", "usage", fmt.Sprintf("unexpected argument %q", flags.rest[0]))
	}
	if flags.value("for") == "" {
		return preconditionFailure(streams, global, "doctor", "usage", "--for plan|develop|review is required")
	}
	dir, err := deps.dir()
	if err != nil {
		return preconditionFailure(streams, global, "doctor", "precondition", err.Error())
	}
	proj, err := loadProject(deps, global)
	if err != nil {
		return preconditionFailure(streams, global, "doctor", "configuration", err.Error())
	}

	report, err := doctor.Offline(context.Background(), doctor.Options{
		For:     flags.value("for"),
		Offline: flags.has("offline"),
		Dir:     dir,
		Config:  proj.Config,
		Git:     deps.git(),
	})
	if err != nil {
		return preconditionFailure(streams, global, "doctor", "usage", err.Error())
	}

	statusValue := result.StatusSucceeded
	if !report.Passed() {
		statusValue = result.StatusFailed
	}
	env := result.New("doctor", statusValue)
	env.Repository = proj.Config.Repository
	env.Data = report
	if global.JSON {
		emitEnvelope(env, global, streams)
	} else {
		renderDoctor(streams.Out, report)
	}
	return env.ExitCode()
}

func renderDoctor(w io.Writer, report doctor.Report) {
	fmt.Fprintf(w, "doctor --for %s (offline: %t)\n", report.For, report.Offline)
	for _, check := range report.Checks {
		if check.Detail == "" {
			fmt.Fprintf(w, "%-4s %s\n", check.Status, check.Name)
			continue
		}
		fmt.Fprintf(w, "%-4s %s: %s\n", check.Status, check.Name, check.Detail)
	}
	fmt.Fprintf(w, "business_acceptance: evaluated=%t (%s)\n", report.BusinessAcceptance.Evaluated, report.Notice)
}

// runMetadata prints the expected projection and the differences to it. It is
// preview only: this command has no --apply, and every request it makes is a
// read.
func runMetadata(deps Deps, global Global, args []string) int {
	streams := deps.streams()
	flags, err := parseFlags(args, map[string]flagKind{"issue": valueFlag, "pr": valueFlag})
	if err != nil {
		return preconditionFailure(streams, global, "metadata", "usage", err.Error())
	}
	if len(flags.rest) > 0 {
		return preconditionFailure(streams, global, "metadata", "usage", fmt.Sprintf("unexpected argument %q", flags.rest[0]))
	}
	issue, pr := flags.value("issue"), flags.value("pr")
	switch {
	case issue == "" && pr == "":
		return preconditionFailure(streams, global, "metadata", "usage", "exactly one of --issue and --pr is required")
	case issue != "" && pr != "":
		return preconditionFailure(streams, global, "metadata", "usage", "--issue and --pr are mutually exclusive")
	}
	options := metadata.Options{}
	if issue != "" {
		number, err := positiveInt("issue", issue)
		if err != nil {
			return preconditionFailure(streams, global, "metadata", "usage", err.Error())
		}
		options.Issue = number
	} else {
		number, err := positiveInt("pr", pr)
		if err != nil {
			return preconditionFailure(streams, global, "metadata", "usage", err.Error())
		}
		options.PR = number
	}

	proj, err := loadProject(deps, global)
	if err != nil {
		return preconditionFailure(streams, global, "metadata", "configuration", err.Error())
	}
	cfg := proj.Config
	if strings.TrimSpace(cfg.Repository) == "" {
		return preconditionFailure(streams, global, "metadata", "configuration",
			"the project configuration has no repository")
	}
	options.Repository = cfg.Repository
	client, err := newClient(deps, cfg.Repository, "developer")
	if err != nil {
		return preconditionFailure(streams, global, "metadata", "credential", err.Error())
	}

	facts, collectErr := metadata.Collect(context.Background(), client, options)
	if collectErr != nil {
		env := result.New("metadata", result.StatusFailed)
		env.Repository = cfg.Repository
		env.Error = &result.Error{Category: "association", Phase: "facts", Detail: detailFor(collectErr)}
		emitEnvelope(env, global, streams)
		return env.ExitCode()
	}
	projection, err := metadata.Project(facts)
	if err != nil {
		env := result.New("metadata", result.StatusFailed)
		env.Repository = cfg.Repository
		env.Error = &result.Error{Category: "projection", Phase: "facts", Detail: detailFor(err)}
		emitEnvelope(env, global, streams)
		return env.ExitCode()
	}
	differences, err := metadata.Differences(facts.Issue, facts.PRs, projection)
	if err != nil {
		env := result.New("metadata", result.StatusFailed)
		env.Repository = cfg.Repository
		env.Error = &result.Error{Category: "projection", Phase: "facts", Detail: detailFor(err)}
		emitEnvelope(env, global, streams)
		return env.ExitCode()
	}

	env := result.New("metadata", result.StatusSucceeded)
	env.Repository = facts.Repository
	env.Target = &result.Target{Kind: targetKind(options), Number: targetNumber(options)}
	env.Data = metadataData(facts, projection, differences)
	if global.JSON {
		emitEnvelope(env, global, streams)
	} else {
		renderMetadata(streams.Out, facts, projection, differences)
	}
	return env.ExitCode()
}

// metadataData is the JSON "data" payload: the expected projection and the
// differences to it, plus the facts they were derived from.
func metadataData(facts metadata.Facts, projection metadata.Projection, differences metadata.DiffSet) map[string]any {
	return map[string]any{
		"repository":     facts.Repository,
		"default_branch": facts.DefaultBranch,
		"issue":          facts.Issue,
		"prs":            facts.PRs,
		"projection":     projection,
		"differences":    differences,
	}
}

func targetKind(options metadata.Options) string {
	if options.PR > 0 {
		return "pr"
	}
	return "issue"
}

func targetNumber(options metadata.Options) int {
	if options.PR > 0 {
		return options.PR
	}
	return options.Issue
}

// detailFor describes a failure without copying remote text. The metadata
// errors are local sentences; a typed GitHub error is reduced to its kind.
func detailFor(err error) string {
	switch {
	case errors.Is(err, metadata.ErrAmbiguousAssociation),
		errors.Is(err, metadata.ErrMultipleClosingReferences),
		errors.Is(err, metadata.ErrCrossRepoReference),
		errors.Is(err, metadata.ErrUnassociatedPR),
		errors.Is(err, metadata.ErrNotFound):
		return err.Error()
	}
	if typed, ok := github.As(err); ok {
		return string(typed.Kind)
	}
	return "the facts could not be read"
}

func renderMetadata(w io.Writer, facts metadata.Facts, projection metadata.Projection, differences metadata.DiffSet) {
	fmt.Fprintf(w, "metadata %s on %s\n", facts.Repository, facts.DefaultBranch)
	fmt.Fprintf(w, "issue #%d -> %s %v\n", projection.Issue.Number, projection.Issue.Stage, projection.Issue.Labels)
	for _, object := range projection.PRs {
		fmt.Fprintf(w, "pr #%d -> %s %v\n", object.Number, object.Stage, object.Labels)
	}
	if differences.Empty() {
		fmt.Fprintln(w, "differences: none")
		return
	}
	fmt.Fprintln(w, "differences:")
	for _, diff := range append([]metadata.ObjectDiff{differences.Issue}, differences.PRs...) {
		fmt.Fprintf(w, "  %s #%d: add %v remove %v", diff.Kind, diff.Number, diff.AddLabels, diff.RemoveLabels)
		if len(diff.InheritLabels) > 0 {
			fmt.Fprintf(w, " inherit %v", diff.InheritLabels)
		}
		if diff.MilestoneCleared {
			fmt.Fprint(w, " milestone->empty")
		}
		fmt.Fprintln(w)
	}
}
