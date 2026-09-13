package gitx

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeRunner is the injectable stand-in for git. It records the argv array it
// was handed, so tests can assert the command contract without git installed
// and without ever touching a shell. It records the context too: a Runner that
// is handed a deadline it never uses would make a hung git process unbounded,
// so the context has to be the caller's own, not a fresh one.
type fakeRunner struct {
	reply func(ctx context.Context, argv []string) (Command, error)

	mu    sync.Mutex
	dirs  []string
	argvs [][]string
	ctxs  []context.Context
}

func (f *fakeRunner) Run(ctx context.Context, dir string, argv []string) (Command, error) {
	f.mu.Lock()
	f.dirs = append(f.dirs, dir)
	f.argvs = append(f.argvs, append([]string(nil), argv...))
	f.ctxs = append(f.ctxs, ctx)
	f.mu.Unlock()
	if f.reply == nil {
		return Command{}, nil
	}
	return f.reply(ctx, argv)
}

func (f *fakeRunner) last() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.argvs) == 0 {
		return nil
	}
	return f.argvs[len(f.argvs)-1]
}

// ctxKey keeps the probe context in the test package's own key space.
type ctxKey struct{}

func TestClientRunsOnlyArgvCommands(t *testing.T) {
	// Every read goes through the injected runner as an argv array. A joined
	// command string would show up here as one element containing spaces. Each
	// read must also hand the caller's context through unchanged.
	ctx := context.WithValue(context.Background(), ctxKey{}, "the caller's budget")
	cases := []struct {
		name string
		call func(context.Context, *Client) error
		want []string
	}{
		{"current branch", func(ctx context.Context, c *Client) error { _, err := c.CurrentBranch(ctx, "/repo"); return err },
			[]string{"symbolic-ref", "--quiet", "--short", "HEAD"}},
		{"head sha", func(ctx context.Context, c *Client) error { _, err := c.HeadSHA(ctx, "/repo"); return err },
			[]string{"rev-parse", "--verify", "HEAD"}},
		{"origin url", func(ctx context.Context, c *Client) error { _, err := c.OriginURL(ctx, "/repo"); return err },
			[]string{"remote", "get-url", "origin"}},
		{"dirty", func(ctx context.Context, c *Client) error { _, err := c.IsDirty(ctx, "/repo"); return err },
			[]string{"status", "--porcelain"}},
		{"shallow", func(ctx context.Context, c *Client) error { _, err := c.IsShallow(ctx, "/repo"); return err },
			[]string{"rev-parse", "--is-shallow-repository"}},
		{
			name: "ref sha",
			call: func(ctx context.Context, c *Client) error {
				_, _, err := c.RefSHA(ctx, "/repo", "refs/heads/main")
				return err
			},
			want: []string{"rev-parse", "--verify", "--quiet", "refs/heads/main^{commit}"},
		},
		{"ancestor", func(ctx context.Context, c *Client) error { _, err := c.IsAncestor(ctx, "/repo", "a", "b"); return err },
			[]string{"merge-base", "--is-ancestor", "a", "b"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := &fakeRunner{reply: plausibleAnswer}
			client := New(runner)
			if err := tc.call(ctx, client); err != nil {
				t.Fatalf("call returned %v, want nil (nothing is a failure here)", err)
			}
			got := runner.last()
			if len(got) != len(tc.want) {
				t.Fatalf("argv = %q, want %q", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("argv = %q, want %q", got, tc.want)
				}
			}
			if runner.dirs[len(runner.dirs)-1] != "/repo" {
				t.Fatalf("dir = %q, want the caller's checkout", runner.dirs[len(runner.dirs)-1])
			}
			if got := runner.ctxs[len(runner.ctxs)-1]; got != ctx {
				t.Fatalf("ctx = %v, want the caller's own context (identity), not a fresh one", got)
			}
		})
	}
}

// plausibleAnswer answers the fake runner with a well-formed answer for
// whichever read was issued, so the argv assertion is not entangled with a
// parsing failure.
func plausibleAnswer(_ context.Context, argv []string) (Command, error) {
	switch argv[0] {
	case "symbolic-ref":
		return Command{Stdout: "ghpipe/issue-3\n"}, nil
	case "rev-parse":
		for _, arg := range argv {
			if arg == "--is-shallow-repository" {
				return Command{Stdout: "false\n"}, nil
			}
		}
		return Command{Stdout: strings.Repeat("a", 40) + "\n"}, nil
	case "remote":
		return Command{Stdout: "git@github.com:ghpipe/ghpipe.git\n"}, nil
	case "merge-base":
		return Command{ExitCode: 0}, nil
	default:
		return Command{}, nil
	}
}

func TestCurrentBranchDetachedIsEmptyAndNoError(t *testing.T) {
	// symbolic-ref exits 1 with no output on a detached HEAD; that is a fact,
	// not a failure.
	runner := &fakeRunner{reply: func(context.Context, []string) (Command, error) {
		return Command{ExitCode: 1, Stderr: ""}, nil
	}}
	branch, err := New(runner).CurrentBranch(context.Background(), "/repo")
	if err != nil {
		t.Fatalf("detached HEAD returned %v, want nil", err)
	}
	if branch != "" {
		t.Fatalf("branch = %q, want empty", branch)
	}
}

func TestCurrentBranchFailureIsAnError(t *testing.T) {
	runner := &fakeRunner{reply: func(context.Context, []string) (Command, error) {
		return Command{ExitCode: 128, Stderr: "fatal: not a git repository\nhint: ...\n"}, nil
	}}
	_, err := New(runner).CurrentBranch(context.Background(), "/repo")
	if err == nil {
		t.Fatal("exit 128 reported success")
	}
	var gitErr *Error
	if !errors.As(err, &gitErr) || gitErr.ExitCode != 128 {
		t.Fatalf("error = %v, want *Error with exit 128", err)
	}
	if strings.Contains(gitErr.Error(), "hint:") {
		t.Fatalf("error kept more than git's first stderr line: %q", gitErr.Error())
	}
}

func TestHeadSHARejectsAnythingButFortyHex(t *testing.T) {
	for _, stdout := range []string{"", "deadbeef", strings.Repeat("A", 40), strings.Repeat("g", 40)} {
		runner := &fakeRunner{reply: func(context.Context, []string) (Command, error) {
			return Command{Stdout: stdout, ExitCode: 0}, nil
		}}
		if _, err := New(runner).HeadSHA(context.Background(), "/repo"); err == nil {
			t.Fatalf("stdout %q accepted as HEAD", stdout)
		}
	}
	sha := strings.Repeat("a", 40)
	runner := &fakeRunner{reply: func(context.Context, []string) (Command, error) {
		return Command{Stdout: sha + "\n"}, nil
	}}
	got, err := New(runner).HeadSHA(context.Background(), "/repo")
	if err != nil || got != sha {
		t.Fatalf("HeadSHA = %q, %v; want %q", got, err, sha)
	}
}

func TestRefSHAMissingRefIsNotAnError(t *testing.T) {
	runner := &fakeRunner{reply: func(context.Context, []string) (Command, error) {
		return Command{ExitCode: 1}, nil
	}}
	sha, ok, err := New(runner).RefSHA(context.Background(), "/repo", "refs/heads/gone")
	if err != nil || ok || sha != "" {
		t.Fatalf("missing ref = (%q, %v, %v), want (\"\", false, nil)", sha, ok, err)
	}
}

func TestRefSHAOtherExitCodesAreErrors(t *testing.T) {
	runner := &fakeRunner{reply: func(context.Context, []string) (Command, error) {
		return Command{ExitCode: 128, Stderr: "fatal: not a git repository"}, nil
	}}
	if _, _, err := New(runner).RefSHA(context.Background(), "/repo", "refs/heads/main"); err == nil {
		t.Fatal("exit 128 reported as 'no such ref'")
	}
}

func TestRefSHARefusesOptionLikeAndEmptyRefs(t *testing.T) {
	runner := &fakeRunner{}
	c := New(runner)
	for _, ref := range []string{"", "   ", "--help", "-c"} {
		if _, _, err := c.RefSHA(context.Background(), "/repo", ref); err == nil {
			t.Fatalf("ref %q was accepted", ref)
		}
	}
	if len(runner.argvs) != 0 {
		t.Fatalf("git was invoked with a rejected ref: %q", runner.argvs)
	}
}

func TestIsAncestorExitCodeContract(t *testing.T) {
	cases := []struct {
		exit    int
		want    bool
		wantErr bool
	}{
		{exit: 0, want: true},
		{exit: 1, want: false},
		{exit: 2, wantErr: true},
		{exit: 128, wantErr: true},
	}
	for _, tc := range cases {
		runner := &fakeRunner{reply: func(context.Context, []string) (Command, error) {
			return Command{ExitCode: tc.exit, Stderr: "fatal: bad revision"}, nil
		}}
		got, err := New(runner).IsAncestor(context.Background(), "/repo", "a", "b")
		if tc.wantErr {
			if err == nil {
				t.Fatalf("exit %d reported %v, want an error", tc.exit, got)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Fatalf("exit %d = (%v, %v), want (%v, nil)", tc.exit, got, err, tc.want)
		}
	}
}

func TestIsShallowParsesGitAnswer(t *testing.T) {
	cases := []struct {
		stdout  string
		want    bool
		wantErr bool
	}{
		{"true\n", true, false},
		{"false\n", false, false},
		{"maybe\n", false, true},
		{"\n", false, true},
	}
	for _, tc := range cases {
		runner := &fakeRunner{reply: func(context.Context, []string) (Command, error) {
			return Command{Stdout: tc.stdout}, nil
		}}
		got, err := New(runner).IsShallow(context.Background(), "/repo")
		if tc.wantErr {
			if err == nil {
				t.Fatalf("stdout %q accepted as %v", tc.stdout, got)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Fatalf("stdout %q = (%v, %v), want (%v, nil)", tc.stdout, got, err, tc.want)
		}
	}
}

func TestExecRunnerMissingGitIsAnError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "definitely-not-git")
	_, err := ExecRunner{Path: missing}.Run(context.Background(), t.TempDir(), []string{"status"})
	if err == nil {
		t.Fatal("a runner whose binary does not exist reported success")
	}
}

func TestExecRunnerPassesArgvWithoutShell(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake git below is a POSIX shell script")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "fake-git")
	body := "#!/bin/sh\nfor arg in \"$@\"; do printf '%s\\n' \"$arg\"; done\nexit 0\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	args := []string{"status", "a b", "$(echo pwned)", "; rm -rf /", "*", "`whoami`"}
	out, err := ExecRunner{Path: script}.Run(context.Background(), dir, args)
	if err != nil {
		t.Fatal(err)
	}
	if out.ExitCode != 0 {
		t.Fatalf("exit = %d, want 0", out.ExitCode)
	}
	got := strings.Split(strings.TrimRight(out.Stdout, "\n"), "\n")
	if len(got) != len(args) {
		t.Fatalf("argv arrived as %q, want %q", got, args)
	}
	for i := range args {
		if got[i] != args[i] {
			t.Fatalf("argument %d = %q, want %q unchanged (a shell would have expanded it)", i, got[i], args[i])
		}
	}
}

func TestExecRunnerReportsExitCodeWithoutError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake git below is a POSIX shell script")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "fake-git")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho boom >&2\nexit 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := ExecRunner{Path: script}.Run(context.Background(), dir, []string{"status"})
	if err != nil {
		t.Fatalf("exit 3 surfaced as a Go error: %v", err)
	}
	if out.ExitCode != 3 || strings.TrimSpace(out.Stderr) != "boom" {
		t.Fatalf("out = %+v, want exit 3 and stderr boom", out)
	}
}

// --- context ---------------------------------------------------------------

// TestExecRunnerKillsTheProcessWhenTheContextExpires is the reason the Runner
// takes a context at all: without it one stalled git process would block the
// command forever (docs/design.md 4.3).
func TestExecRunnerKillsTheProcessWhenTheContextExpires(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake git below is a POSIX shell script")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "slow-git")
	// "exec" matters: it makes the shell replace itself with sleep, so killing
	// the process really ends the command instead of orphaning a child that
	// keeps the output pipe open past the deadline.
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()

	started := time.Now()
	_, err := ExecRunner{Path: script}.Run(ctx, dir, []string{"status"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatalf("Run blocked for %s despite a 250ms deadline", elapsed)
	}
}

// TestExecRunnerRefusesAnAlreadyCanceledContext pins the other half of the
// contract: an expired budget must not start a process at all.
func TestExecRunnerRefusesAnAlreadyCanceledContext(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake git below is a POSIX shell script")
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran")
	script := filepath.Join(dir, "fake-git")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n: > \""+marker+"\"\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	runner := ExecRunner{Path: script}
	if _, err := runner.Run(ctx, dir, []string{"status"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("git ran even though the context was already canceled")
	}
}

// TestContextFailureIsNeverAnAnswer checks every read: a runner that reports
// "out of time" must reach the caller as an error, including from the reads
// whose normal contract is a boolean or a "not found" answer, where a false
// answer would be mistaken for a fact.
func TestContextFailureIsNeverAnAnswer(t *testing.T) {
	runner := &fakeRunner{reply: func(context.Context, []string) (Command, error) {
		return Command{ExitCode: -1}, context.DeadlineExceeded
	}}
	c := New(runner)
	ctx := context.Background()

	checks := []struct {
		name string
		call func() error
	}{
		{"current branch", func() error { _, err := c.CurrentBranch(ctx, "/repo"); return err }},
		{"head sha", func() error { _, err := c.HeadSHA(ctx, "/repo"); return err }},
		{"origin url", func() error { _, err := c.OriginURL(ctx, "/repo"); return err }},
		{"dirty", func() error { _, err := c.IsDirty(ctx, "/repo"); return err }},
		{"shallow", func() error { _, err := c.IsShallow(ctx, "/repo"); return err }},
		{"ref sha", func() error { _, _, err := c.RefSHA(ctx, "/repo", "refs/heads/main"); return err }},
		{"ancestor", func() error { _, err := c.IsAncestor(ctx, "/repo", "a", "b"); return err }},
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			err := check.call()
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("err = %v, want context.DeadlineExceeded", err)
			}
			// The wrapped message names the read, so an operator can tell which
			// command ran out of budget.
			if !strings.Contains(err.Error(), "gitx: ") {
				t.Fatalf("error %q does not name the git invocation", err)
			}
		})
	}
}

// --- integration -----------------------------------------------------------
//
// These tests run the real git binary in a throwaway directory. They never
// listen on a socket and never touch the developer's repository. When git is
// not installed they skip: the contract is still covered by the fake-runner
// tests above.

type testRepo struct {
	dir    string
	branch string
	sha    string
}

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed; skipping integration test")
	}
}

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	full := append([]string{
		"-c", "user.name=Git Test",
		"-c", "user.email=git@example.invalid",
		"-c", "commit.gpgsign=false",
	}, args...)
	cmd := exec.Command("git", full...)
	cmd.Dir = dir
	var out, errOut strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, errOut.String())
	}
	return strings.TrimSpace(out.String())
}

func newTestRepo(t *testing.T) testRepo {
	t.Helper()
	requireGit(t)
	// Keep the developer's global configuration (hooks, gpgsign, fsmonitor) out
	// of the fixture: the fixture must behave the same on every machine.
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	dir := t.TempDir()
	gitRun(t, dir, "init", "--quiet")
	gitRun(t, dir, "commit", "--quiet", "--allow-empty", "-m", "one")
	return testRepo{
		dir:    dir,
		branch: gitRun(t, dir, "rev-parse", "--abbrev-ref", "HEAD"),
		sha:    gitRun(t, dir, "rev-parse", "HEAD"),
	}
}

func TestIntegrationCurrentBranchAndDetachedHead(t *testing.T) {
	repo := newTestRepo(t)
	c := New(nil)
	ctx := t.Context()

	branch, err := c.CurrentBranch(ctx, repo.dir)
	if err != nil {
		t.Fatal(err)
	}
	if branch != repo.branch {
		t.Fatalf("branch = %q, want %q", branch, repo.branch)
	}

	gitRun(t, repo.dir, "checkout", "--quiet", "--detach", repo.sha)
	branch, err = c.CurrentBranch(ctx, repo.dir)
	if err != nil {
		t.Fatalf("detached HEAD returned %v, want nil", err)
	}
	if branch != "" {
		t.Fatalf("detached branch = %q, want empty", branch)
	}
}

func TestIntegrationHeadSHA(t *testing.T) {
	repo := newTestRepo(t)
	sha, err := New(nil).HeadSHA(t.Context(), repo.dir)
	if err != nil {
		t.Fatal(err)
	}
	if sha != repo.sha {
		t.Fatalf("HEAD = %q, want %q", sha, repo.sha)
	}
	if !shaRe.MatchString(sha) {
		t.Fatalf("HEAD %q is not 40 lower-case hex characters", sha)
	}
}

func TestIntegrationHeadSHAOutsideRepositoryFails(t *testing.T) {
	requireGit(t)
	if _, err := New(nil).HeadSHA(t.Context(), t.TempDir()); err == nil {
		t.Fatal("HEAD resolved outside a repository")
	}
}

func TestIntegrationRefSHA(t *testing.T) {
	repo := newTestRepo(t)
	c := New(nil)
	ctx := t.Context()

	sha, ok, err := c.RefSHA(ctx, repo.dir, "refs/heads/"+repo.branch)
	if err != nil || !ok || sha != repo.sha {
		t.Fatalf("existing ref = (%q, %v, %v), want (%q, true, nil)", sha, ok, err, repo.sha)
	}
	if sha, ok, err = c.RefSHA(ctx, repo.dir, "refs/heads/ghpipe/issue-3"); err != nil || ok || sha != "" {
		t.Fatalf("missing ref = (%q, %v, %v), want (\"\", false, nil)", sha, ok, err)
	}
}

func TestIntegrationIsAncestor(t *testing.T) {
	repo := newTestRepo(t)
	c := New(nil)
	ctx := t.Context()

	gitRun(t, repo.dir, "commit", "--quiet", "--allow-empty", "-m", "two")
	head := gitRun(t, repo.dir, "rev-parse", "HEAD")

	if ok, err := c.IsAncestor(ctx, repo.dir, repo.sha, head); err != nil || !ok {
		t.Fatalf("ancestor = (%v, %v), want (true, nil)", ok, err)
	}
	if ok, err := c.IsAncestor(ctx, repo.dir, head, repo.sha); err != nil || ok {
		t.Fatalf("descendant = (%v, %v), want (false, nil)", ok, err)
	}
	if ok, err := c.IsAncestor(ctx, repo.dir, head, head); err != nil || !ok {
		t.Fatalf("self = (%v, %v), want (true, nil)", ok, err)
	}
	if _, err := c.IsAncestor(ctx, repo.dir, "refs/heads/does-not-exist", head); err == nil {
		t.Fatal("an unresolvable revision was reported as 'not an ancestor'")
	}
}

func TestIntegrationIsShallow(t *testing.T) {
	repo := newTestRepo(t)
	shallow, err := New(nil).IsShallow(t.Context(), repo.dir)
	if err != nil {
		t.Fatal(err)
	}
	if shallow {
		t.Fatal("a fresh local repository reported shallow")
	}
}

func TestIntegrationIsDirty(t *testing.T) {
	repo := newTestRepo(t)
	c := New(nil)
	ctx := t.Context()

	dirty, err := c.IsDirty(ctx, repo.dir)
	if err != nil {
		t.Fatal(err)
	}
	if dirty {
		t.Fatal("a fresh repository reported dirty")
	}

	untracked := filepath.Join(repo.dir, "notes.txt")
	if err := os.WriteFile(untracked, []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if dirty, err = c.IsDirty(ctx, repo.dir); err != nil || !dirty {
		t.Fatalf("untracked file = (%v, %v), want (true, nil)", dirty, err)
	}
	if err := os.Remove(untracked); err != nil {
		t.Fatal(err)
	}

	tracked := filepath.Join(repo.dir, "tracked.txt")
	if err := os.WriteFile(tracked, []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo.dir, "add", "tracked.txt")
	if dirty, err = c.IsDirty(ctx, repo.dir); err != nil || !dirty {
		t.Fatalf("staged file = (%v, %v), want (true, nil)", dirty, err)
	}
}

func TestIntegrationOriginURL(t *testing.T) {
	repo := newTestRepo(t)
	c := New(nil)
	ctx := t.Context()

	if _, err := c.OriginURL(ctx, repo.dir); err == nil {
		t.Fatal("a repository without origin reported a URL")
	}
	want := "https://github.com/ghpipe/ghpipe.git"
	gitRun(t, repo.dir, "remote", "add", "origin", want)
	got, err := c.OriginURL(ctx, repo.dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("origin = %q, want %q", got, want)
	}
	ownerRepo, ok := RemoteRepository(got)
	if !ok || ownerRepo != "ghpipe/ghpipe" {
		t.Fatalf("RemoteRepository(%q) = (%q, %v)", got, ownerRepo, ok)
	}
}

func TestIntegrationCanceledContextStopsRealGit(t *testing.T) {
	repo := newTestRepo(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := New(nil).HeadSHA(ctx, repo.dir); err == nil {
		t.Fatal("a canceled context still produced a git answer")
	} else if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
