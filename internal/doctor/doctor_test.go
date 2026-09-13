package doctor

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/ghpipe/ghpipe/internal/gitx"
	"github.com/ghpipe/ghpipe/internal/project"
)

// fakeGit scripts the git Runner so the preflight asserts the questions it
// asks, not the state of whatever repository happens to be checked out.
type fakeGit struct {
	mu        sync.Mutex
	asked     []string
	responses map[string]gitx.Command
}

func (f *fakeGit) Run(ctx context.Context, dir string, argv []string) (gitx.Command, error) {
	key := strings.Join(argv, " ")
	f.mu.Lock()
	f.asked = append(f.asked, key)
	f.mu.Unlock()
	if command, ok := f.responses[key]; ok {
		return command, nil
	}
	return gitx.Command{ExitCode: 1, Stderr: "not scripted"}, nil
}

func fakeRunner(branch, porcelain, shallow string) *fakeGit {
	return &fakeGit{responses: map[string]gitx.Command{
		"symbolic-ref --quiet --short HEAD": {Stdout: branch},
		"rev-parse --verify HEAD":           {Stdout: "cccccccccccccccccccccccccccccccccccccccc\n"},
		"status --porcelain":                {Stdout: porcelain},
		"rev-parse --is-shallow-repository": {Stdout: shallow},
	}}
}

// checkout wires the scripted runner into the read-only git client.
func checkout(branch, porcelain, shallow string) *gitx.Client {
	return gitx.New(fakeRunner(branch, porcelain, shallow))
}

func validConfig() *project.Config {
	return &project.Config{
		SchemaVersion: project.SchemaVersion,
		Repository:    "ghpipe/ghpipe",
		DefaultBranch: "main",
		CI: project.CI{RequiredChecks: []project.RequiredCheck{
			{Context: "ghpipe-quality", IntegrationID: 15368},
		}},
		Commands: map[string]project.Command{
			"test": {Cwd: ".", Argv: []string{"go", "test", "-count=1", "./..."}},
		},
	}
}

func run(t *testing.T, opts Options) Report {
	t.Helper()
	report, err := Offline(context.Background(), opts)
	if err != nil {
		t.Fatalf("Offline: %v", err)
	}
	return report
}

func checkNamed(report Report, name string) (Check, bool) {
	for _, check := range report.Checks {
		if check.Name == name {
			return check, true
		}
	}
	return Check{}, false
}

func TestOfflinePlanChecksTheLocalConfigurationOnly(t *testing.T) {
	git := fakeRunner("main\n", "", "false\n")
	report := run(t, Options{For: "plan", Offline: true, Config: validConfig(), Git: gitx.New(git)})
	if !report.Passed() {
		t.Fatalf("report failed: %+v", report.Checks)
	}
	if report.BusinessAcceptance.Evaluated {
		t.Error("business_acceptance.evaluated = true, want false")
	}
	if _, ok := checkNamed(report, "branch_guard"); ok {
		t.Error("plan must not run the branch guard")
	}
	git.mu.Lock()
	defer git.mu.Unlock()
	if len(git.asked) != 0 {
		t.Errorf("plan asked git: %v", git.asked)
	}
}

func TestOfflineDevelopRequiresABranch(t *testing.T) {
	// Detached HEAD: there is nowhere to commit.
	detached := run(t, Options{For: "develop", Offline: true, Config: validConfig(),
		Git: checkout("", "", "false\n")})
	if check, _ := checkNamed(detached, "branch_guard"); check.Status != StatusFail {
		t.Errorf("branch_guard = %+v, want a failure", check)
	}

	// On the default branch with uncommitted changes: the mistake status and
	// doctor are supposed to surface (docs/design.md 8.2).
	dirty := run(t, Options{For: "develop", Offline: true, Config: validConfig(),
		Git: checkout("main\n", " M x.go\n", "false\n")})
	if check, _ := checkNamed(dirty, "branch_guard"); check.Status != StatusFail {
		t.Errorf("branch_guard = %+v, want a failure", check)
	}

	// On the task branch with a clean tree: ready to develop.
	ready := run(t, Options{For: "develop", Offline: true, Config: validConfig(),
		Git: checkout("ghpipe/issue-7\n", "", "false\n")})
	if !ready.Passed() {
		t.Errorf("report failed: %+v", ready.Checks)
	}
}

func TestOfflineReviewRequiresACleanTree(t *testing.T) {
	dirty := run(t, Options{For: "review", Offline: true, Config: validConfig(),
		Git: checkout("ghpipe/issue-7\n", " M x.go\n", "false\n")})
	if check, _ := checkNamed(dirty, "branch_guard"); check.Status != StatusFail {
		t.Errorf("branch_guard = %+v, want a failure", check)
	}

	// A detached HEAD is a supported state for read-only review.
	detached := run(t, Options{For: "review", Offline: true, Config: validConfig(),
		Git: checkout("", "", "false\n")})
	if check, _ := checkNamed(detached, "branch_guard"); check.Status != StatusPass {
		t.Errorf("branch_guard = %+v, want a pass for a detached review checkout", check)
	}
}

func TestOfflineReportsAShallowClone(t *testing.T) {
	report := run(t, Options{For: "develop", Offline: true, Config: validConfig(),
		Git: checkout("ghpipe/issue-7\n", "", "true\n")})
	check, ok := checkNamed(report, "attribution_boundary")
	if !ok || check.Status != StatusFail {
		t.Fatalf("attribution_boundary = %+v, want a failure", check)
	}
	if report.Passed() {
		t.Error("Passed = true, want false")
	}
}

func TestOfflineRefusesWhatItDoesNotImplement(t *testing.T) {
	cases := []Options{
		{For: "bootstrap", Offline: true, Config: validConfig()},
		{For: "handoff", Offline: true, Config: validConfig()},
		{For: "plan", Offline: false, Config: validConfig()},
		{For: "", Offline: true, Config: validConfig()},
	}
	for _, opts := range cases {
		t.Run(opts.For+"/offline="+map[bool]string{true: "yes", false: "no"}[opts.Offline], func(t *testing.T) {
			if _, err := Offline(context.Background(), opts); !errors.Is(err, ErrUnsupported) {
				t.Fatalf("err = %v, want ErrUnsupported", err)
			}
		})
	}
}

func TestOfflineReportsABrokenConfiguration(t *testing.T) {
	broken := validConfig()
	broken.Repository = ""
	report := run(t, Options{For: "plan", Offline: true, Config: broken, Git: checkout("main\n", "", "false\n")})
	if check, _ := checkNamed(report, "configuration"); check.Status != StatusFail {
		t.Errorf("configuration = %+v, want a failure", check)
	}

	noCommands := validConfig()
	noCommands.Commands = map[string]project.Command{"test": {Cwd: "../outside"}}
	report = run(t, Options{For: "plan", Offline: true, Config: noCommands, Git: checkout("main\n", "", "false\n")})
	if check, _ := checkNamed(report, "commands"); check.Status != StatusFail {
		t.Errorf("commands = %+v, want a failure for a cwd outside the project", check)
	}
}

func TestOfflineWithoutAConfigurationFails(t *testing.T) {
	report := run(t, Options{For: "plan", Offline: true, Git: checkout("main\n", "", "false\n")})
	if check, _ := checkNamed(report, "configuration"); check.Status != StatusFail {
		t.Errorf("configuration = %+v, want a failure", check)
	}
}
