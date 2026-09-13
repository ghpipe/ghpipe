// Package gitx is ghpipe's read-only git query layer.
//
// Every command is executed as an argv array through an injected Runner, never
// through a shell, so behaviour is identical on every platform and a value such
// as "a b; rm -rf x" stays one argument. Every method takes the checkout
// directory explicitly, so one Client serves several worktrees. Every method
// also takes a context.Context, so a git process that hangs (a locked index, a
// credential prompt, a stalled network filesystem) is bounded by the caller's
// budget instead of blocking ghpipe forever.
//
// Parsing is strict: output that does not match the documented shape is an
// error, never a guess. Expected negative answers that git reports as exit
// codes (detached HEAD, missing ref, "not an ancestor") are translated into
// normal return values, because they are facts, not failures.
//
// This package implements the read-only surface of docs/design.md 8.1. The
// write path (signed commit, push, cleanup) is deliberately absent.
package gitx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
)

// shaRe is the only SHA form ghpipe accepts: a full, lower-case 40-character
// object name. Abbreviated or upper-case output would make "same commit"
// comparisons unreliable.
var shaRe = regexp.MustCompile(`^[0-9a-f]{40}$`)

// maxDetail caps how much of git's stderr an error carries. The first line is
// already the diagnostic; the rest is usually a hint listing that would bloat
// every caller's error path.
const maxDetail = 200

// Command is the outcome of one git invocation. A non-zero ExitCode is data,
// not an error: callers translate it (see Client.IsAncestor, Client.RefSHA).
type Command struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// Runner executes git in a directory. argv is the argument vector handed
// straight to the process; implementations must never join it into a shell
// command line.
//
// ctx is the timeout/cancellation budget of the invocation and must reach the
// process itself, not just the caller (docs/design.md 4.3): a Runner that
// ignores ctx would let one stalled git process block a command forever. ctx
// must not be nil.
//
// Run returns an error when git could not be started or waited for - a
// non-zero git exit status is reported through Command.ExitCode instead. A
// canceled or expired ctx is an error too, whatever the exit status says.
type Runner interface {
	Run(ctx context.Context, dir string, argv []string) (Command, error)
}

// ExecRunner is the production Runner: os/exec with an argv array and no shell.
type ExecRunner struct {
	// Path overrides the git executable; empty means "git" resolved from PATH.
	Path string
}

// Run implements Runner. The process is started through exec.CommandContext, so
// cancelling ctx kills it rather than leaving it running in the background.
func (r ExecRunner) Run(ctx context.Context, dir string, argv []string) (Command, error) {
	// An expired budget must not start a process at all - a git that is already
	// out of time would keep running (and keep refreshing the index) until
	// someone noticed it.
	if err := ctx.Err(); err != nil {
		return Command{}, err
	}
	path := r.Path
	if path == "" {
		path = "git"
	}
	cmd := exec.CommandContext(ctx, path, argv...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	out := Command{Stdout: stdout.String(), Stderr: stderr.String()}
	if err != nil {
		// A killed process also reports an exit status, so the context is
		// checked first: "out of time" must never look like "git said no".
		if ctxErr := ctx.Err(); ctxErr != nil {
			return out, ctxErr
		}
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			out.ExitCode = exit.ExitCode()
			return out, nil
		}
		// The binary could not be started at all (typically: git is missing).
		return out, err
	}
	return out, nil
}

// Client is the read-only query surface. It holds nothing but the Runner, so
// one Client can be shared and no answer is ever cached: each call re-reads the
// repository, because a stale fact is worse than a slow one.
type Client struct {
	runner Runner
}

// New returns a Client. A nil Runner means ExecRunner, i.e. the real git
// binary on PATH; tests inject a fake Runner to assert the argv contract
// without needing git.
func New(runner Runner) *Client {
	if runner == nil {
		runner = ExecRunner{}
	}
	return &Client{runner: runner}
}

// Error is a failed git invocation. It carries the operation, git's exit code
// and the first line of stderr: enough to diagnose locally, short enough that
// it never becomes a transcript of remote output.
type Error struct {
	Op       string
	ExitCode int
	Detail   string
}

func (e *Error) Error() string {
	if e.Detail == "" {
		return fmt.Sprintf("gitx: %s failed (exit %d)", e.Op, e.ExitCode)
	}
	return fmt.Sprintf("gitx: %s failed (exit %d): %s", e.Op, e.ExitCode, e.Detail)
}

// run executes one argv and turns a non-zero exit into an *Error. Failures that
// are not exit statuses (git missing, ctx canceled) are wrapped with the argv
// so the caller can tell which read ran out of time.
func (c *Client) run(ctx context.Context, dir string, argv ...string) (Command, error) {
	out, err := c.runner.Run(ctx, dir, argv)
	if err != nil {
		return out, fmt.Errorf("gitx: %s: %w", strings.Join(argv, " "), err)
	}
	return out, nil
}

// fail converts a non-zero Command into an *Error.
func fail(argv []string, out Command) error {
	return &Error{Op: "git " + strings.Join(argv, " "), ExitCode: out.ExitCode, Detail: detail(out.Stderr)}
}

// detail keeps the first non-empty line of stderr, trimmed and bounded.
func detail(stderr string) string {
	for _, line := range strings.Split(stderr, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if len(line) > maxDetail {
			line = line[:maxDetail]
		}
		return line
	}
	return ""
}

// validRef rejects an empty or option-looking ref. Without this guard a value
// such as "--help" would be consumed by git as a flag instead of a revision:
// argv execution removes shell injection, not argument confusion.
func validRef(ref string) error {
	if strings.TrimSpace(ref) == "" {
		return errors.New("gitx: empty ref")
	}
	if strings.HasPrefix(ref, "-") {
		return fmt.Errorf("gitx: ref %q must not start with '-'", ref)
	}
	return nil
}

// CurrentBranch returns the checked-out branch. A detached HEAD returns "" with
// no error: detached is a supported state (read-only review, tests, status),
// not a failure.
func (c *Client) CurrentBranch(ctx context.Context, dir string) (string, error) {
	argv := []string{"symbolic-ref", "--quiet", "--short", "HEAD"}
	out, err := c.run(ctx, dir, argv...)
	if err != nil {
		return "", err
	}
	if branch := strings.TrimSpace(out.Stdout); branch != "" {
		if out.ExitCode != 0 {
			return "", fail(argv, out)
		}
		return branch, nil
	}
	// symbolic-ref exits 1 with no output on a detached HEAD.
	if out.ExitCode == 1 {
		return "", nil
	}
	if out.ExitCode != 0 {
		return "", fail(argv, out)
	}
	return "", nil
}

// HeadSHA returns the full 40-character SHA of HEAD, or an error when HEAD
// cannot be resolved (unborn branch, not a repository, git missing).
func (c *Client) HeadSHA(ctx context.Context, dir string) (string, error) {
	argv := []string{"rev-parse", "--verify", "HEAD"}
	out, err := c.run(ctx, dir, argv...)
	if err != nil {
		return "", err
	}
	if out.ExitCode != 0 {
		return "", fail(argv, out)
	}
	sha := strings.TrimSpace(out.Stdout)
	if !shaRe.MatchString(sha) {
		return "", &Error{Op: "git " + strings.Join(argv, " "), ExitCode: out.ExitCode,
			Detail: "HEAD is not a full 40-character hex SHA"}
	}
	return sha, nil
}

// OriginURL returns the URL configured for the origin remote.
func (c *Client) OriginURL(ctx context.Context, dir string) (string, error) {
	argv := []string{"remote", "get-url", "origin"}
	out, err := c.run(ctx, dir, argv...)
	if err != nil {
		return "", err
	}
	if out.ExitCode != 0 {
		return "", fail(argv, out)
	}
	url := strings.TrimSpace(out.Stdout)
	if url == "" {
		return "", &Error{Op: "git " + strings.Join(argv, " "), Detail: "origin has no URL"}
	}
	return url, nil
}

// IsDirty reports whether the working tree has any change, including untracked
// files: a handoff must not depend on which kind of change the next actor
// forgot to commit.
func (c *Client) IsDirty(ctx context.Context, dir string) (bool, error) {
	argv := []string{"status", "--porcelain"}
	out, err := c.run(ctx, dir, argv...)
	if err != nil {
		return false, err
	}
	if out.ExitCode != 0 {
		return false, fail(argv, out)
	}
	return strings.TrimSpace(out.Stdout) != "", nil
}

// RefSHA resolves a ref to a full commit SHA. A ref that does not exist returns
// ("", false, nil): "there is no such ref" is an answer, not an error. Any
// other failure (not a repository, git missing, malformed ref, expired ctx) is
// an error.
func (c *Client) RefSHA(ctx context.Context, dir, ref string) (string, bool, error) {
	if err := validRef(ref); err != nil {
		return "", false, err
	}
	// ^{commit} makes a tag or tree resolve to the commit it names, and makes a
	// non-commit object a "not found" answer rather than a wrong SHA.
	argv := []string{"rev-parse", "--verify", "--quiet", ref + "^{commit}"}
	out, err := c.run(ctx, dir, argv...)
	if err != nil {
		return "", false, err
	}
	switch {
	case out.ExitCode == 0:
		sha := strings.TrimSpace(out.Stdout)
		if !shaRe.MatchString(sha) {
			return "", false, &Error{Op: "git " + strings.Join(argv, " "), Detail: "ref did not resolve to a full 40-character hex SHA"}
		}
		return sha, true, nil
	case out.ExitCode == 1:
		return "", false, nil
	default:
		return "", false, fail(argv, out)
	}
}

// IsAncestor reports whether ancestor is an ancestor of (or equal to)
// descendant. git's exit codes are the contract: 0 means yes, 1 means no, and
// anything else is a real failure that must not be reported as "no" - the same
// reason a canceled ctx is an error rather than a "no".
func (c *Client) IsAncestor(ctx context.Context, dir, ancestor, descendant string) (bool, error) {
	if err := validRef(ancestor); err != nil {
		return false, err
	}
	if err := validRef(descendant); err != nil {
		return false, err
	}
	argv := []string{"merge-base", "--is-ancestor", ancestor, descendant}
	out, err := c.run(ctx, dir, argv...)
	if err != nil {
		return false, err
	}
	switch out.ExitCode {
	case 0:
		return true, nil
	case 1:
		return false, nil
	default:
		return false, fail(argv, out)
	}
}

// IsShallow reports whether the checkout is a shallow clone. Shallow history
// makes ancestor questions unanswerable, so callers need to know.
func (c *Client) IsShallow(ctx context.Context, dir string) (bool, error) {
	argv := []string{"rev-parse", "--is-shallow-repository"}
	out, err := c.run(ctx, dir, argv...)
	if err != nil {
		return false, err
	}
	if out.ExitCode != 0 {
		return false, fail(argv, out)
	}
	switch strings.TrimSpace(out.Stdout) {
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, &Error{Op: "git " + strings.Join(argv, " "), Detail: "unexpected shallow repository answer"}
	}
}
