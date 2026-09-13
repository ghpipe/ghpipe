// Package project discovers a ghpipe project and validates its non-secret
// configuration. Configuration is never an authorization source.
package project

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
)

// ConfigDir is the directory, relative to a project root, holding ghpipe files.
const ConfigDir = ".ghpipe"

// ConfigFile is the project configuration file name inside ConfigDir.
const ConfigFile = "config.json"

// SchemaVersion is the only supported configuration schema version.
const SchemaVersion = 1

// workflowDir is the only directory allowed to hold ghpipe gate workflows.
// Workflow paths are always compared with "/" separators so a config behaves
// identically on every platform.
const workflowDir = ".github/workflows"

var (
	nameRe    = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
	repoRe    = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	versionRe = regexp.MustCompile(`^\d+\.\d+\.\d+$`)
	commandRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
	shaRe     = regexp.MustCompile(`^[0-9a-f]{40}$`)
	credRefRe = regexp.MustCompile(`^[A-Za-z0-9_./:-]{1,160}$`)
)

// Command is one registered native command. It is executed as an argv array -
// never through a shell - so behaviour is identical on every platform.
type Command struct {
	Cwd  string   `json:"cwd"`
	Argv []string `json:"argv"`
}

// App binds one role to a GitHub App installation.
type App struct {
	AppID          int    `json:"app_id"`
	InstallationID int    `json:"installation_id"`
	Slug           string `json:"slug"`
	CredentialRef  string `json:"credential_ref"`
}

// RequiredCheck is a source-bound required status check.
type RequiredCheck struct {
	Context       string `json:"context"`
	IntegrationID int    `json:"integration_id"`
}

// CI holds the gates the project relies on.
type CI struct {
	RequiredChecks []RequiredCheck `json:"required_checks,omitempty"`
	WorkflowPath   string          `json:"workflow_path,omitempty"`
}

// Quality controls the bootstrap/strict gate. It is a policy selector, not a
// general "skip checks" switch.
type Quality struct {
	Mode string `json:"mode,omitempty"` // bootstrap | strict (default strict)
}

// Attribution controls author verification on push.
type Attribution struct {
	Required     bool   `json:"required"`
	LegacyBefore string `json:"legacy_before,omitempty"`
}

// Execution controls the child environment of project commands.
type Execution struct {
	StripEnv []string `json:"strip_env,omitempty"`
}

// Reporting is the opt-in defect-reporting path (off by default).
type Reporting struct {
	Enabled       bool   `json:"enabled"`
	Destination   string `json:"destination,omitempty"`
	CredentialRef string `json:"credential_ref,omitempty"`
	MaxPerHour    int    `json:"max_per_hour,omitempty"`
}

// Config is the parsed .ghpipe/config.json.
type Config struct {
	SchemaVersion int                `json:"schema_version"`
	Repository    string             `json:"repository,omitempty"`
	DefaultBranch string             `json:"default_branch,omitempty"`
	Quality       Quality            `json:"quality,omitempty"`
	Apps          map[string]App     `json:"apps,omitempty"`
	CI            CI                 `json:"ci,omitempty"`
	Commands      map[string]Command `json:"commands,omitempty"`
	Attribution   Attribution        `json:"attribution,omitempty"`
	Execution     Execution          `json:"execution,omitempty"`
	Reporting     Reporting          `json:"reporting,omitempty"`
}

// Mode returns the effective quality mode (missing means strict).
func (c *Config) Mode() string {
	if c.Quality.Mode == "" {
		return "strict"
	}
	return c.Quality.Mode
}

// Project is a discovered project: its root directory and parsed config.
type Project struct {
	Root   string
	Path   string
	Config *Config
}

// Discover walks up from start looking for .ghpipe/config.json, stopping at the
// nearest .git directory. It returns the project root and config path.
func Discover(start string) (*Project, error) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return nil, err
	}
	if info, statErr := os.Stat(dir); statErr == nil && !info.IsDir() {
		dir = filepath.Dir(dir)
	}
	for {
		candidate := filepath.Join(dir, ConfigDir, ConfigFile)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return Load(dir)
		}
		if info, err := os.Stat(filepath.Join(dir, ".git")); err == nil && info.IsDir() {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return nil, fmt.Errorf("no ghpipe configuration found above %s; run ghpipe init", start)
}

// Load reads and validates the configuration of the project rooted at root.
func Load(root string) (*Project, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(absRoot, ConfigDir, ConfigFile)
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &Project{Root: absRoot, Path: path, Config: &cfg}, nil
}

// Validate enforces the configuration contract. Any failure here is a usage /
// precondition error: nothing has been written and no remote call was made.
func (c *Config) Validate() error {
	if c.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported schema_version %d (want %d)", c.SchemaVersion, SchemaVersion)
	}
	if c.Repository != "" && !repoRe.MatchString(c.Repository) {
		return fmt.Errorf("repository must be OWNER/REPO")
	}
	if c.DefaultBranch != "" {
		if !nameRe.MatchString(c.DefaultBranch) || c.DefaultBranch == ".." {
			return fmt.Errorf("invalid default_branch")
		}
	}
	if m := c.Mode(); m != "bootstrap" && m != "strict" {
		return fmt.Errorf("quality.mode must be bootstrap or strict (missing means strict)")
	}
	if err := c.validateApps(); err != nil {
		return err
	}
	for name, cmd := range c.Commands {
		if !commandRe.MatchString(name) {
			return fmt.Errorf("commands[%q]: name allows letters, digits, _, . and -", name)
		}
		if len(cmd.Argv) == 0 {
			return fmt.Errorf("commands[%q].argv must be a non-empty array (no shell strings)", name)
		}
		for _, arg := range cmd.Argv {
			if arg == "" {
				return fmt.Errorf("commands[%q].argv entries must be non-empty", name)
			}
		}
		if !IsProjectRelative(cmd.Cwd) {
			return fmt.Errorf("commands[%q].cwd must be a non-empty project-relative path", name)
		}
	}
	seen := map[string]bool{}
	for _, check := range c.CI.RequiredChecks {
		if check.Context == "" || check.IntegrationID <= 0 {
			return fmt.Errorf("each required check needs a non-empty context and a positive integration_id")
		}
		if seen[check.Context] {
			return fmt.Errorf("duplicate required check context %q", check.Context)
		}
		seen[check.Context] = true
	}
	if c.CI.WorkflowPath != "" {
		workflow := slashPath(c.CI.WorkflowPath)
		if !IsProjectRelative(c.CI.WorkflowPath) || path.Dir(workflow) != workflowDir || path.Base(workflow) == "." {
			return fmt.Errorf("ci.workflow_path must be a file directly under %s/", workflowDir)
		}
	}
	if c.Attribution.LegacyBefore != "" && !shaRe.MatchString(c.Attribution.LegacyBefore) {
		return fmt.Errorf("attribution.legacy_before must be a full 40-character SHA")
	}
	if c.Reporting.Enabled {
		if c.Reporting.Destination == "" || !repoRe.MatchString(c.Reporting.Destination) {
			return fmt.Errorf("reporting.destination must be OWNER/REPO when reporting is enabled")
		}
		if c.Reporting.CredentialRef == "" || !credRefRe.MatchString(c.Reporting.CredentialRef) {
			return fmt.Errorf("reporting.credential_ref must be a logical reference, never a token")
		}
		if c.Reporting.MaxPerHour < 0 {
			return fmt.Errorf("reporting.max_per_hour must not be negative")
		}
	}
	return nil
}

func (c *Config) validateApps() error {
	ids := []int{}
	for _, role := range []string{"developer", "delivery"} {
		app, ok := c.Apps[role]
		if !ok {
			continue
		}
		if app.AppID < 0 || app.InstallationID < 0 {
			return fmt.Errorf("apps.%s identifiers must be positive", role)
		}
		if app.CredentialRef != "" && !credRefRe.MatchString(app.CredentialRef) {
			return fmt.Errorf("apps.%s.credential_ref is a logical name, never a secret", role)
		}
		if app.Slug != "" && !nameRe.MatchString(app.Slug) {
			return fmt.Errorf("apps.%s.slug is invalid", role)
		}
		if app.AppID > 0 {
			ids = append(ids, app.AppID)
		}
	}
	if len(ids) == 2 && ids[0] == ids[1] {
		return fmt.Errorf("developer and delivery must use different GitHub Apps (or different accounts)")
	}
	return nil
}

// CommandNames returns the registered command names, sorted.
func (c *Config) CommandNames() []string {
	names := make([]string, 0, len(c.Commands))
	for name := range c.Commands {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
