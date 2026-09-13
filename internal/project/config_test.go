package project

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func writeConfig(t *testing.T, root, body string) {
	t.Helper()
	dir := filepath.Join(root, ConfigDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ConfigFile), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadValidConfig(t *testing.T) {
	root := t.TempDir()
	writeConfig(t, root, `{
	  "schema_version": 1,
	  "repository": "OWNER/REPO",
	  "default_branch": "main",
	  "apps": {
	    "developer": {"app_id": 1, "installation_id": 11, "slug": "owner-dev", "credential_ref": "OWNER/REPO/developer"},
	    "delivery": {"app_id": 2, "installation_id": 22, "slug": "owner-delivery", "credential_ref": "OWNER/REPO/delivery"}
	  },
	  "ci": {"required_checks": [{"context": "ghpipe-quality", "integration_id": 15368}], "workflow_path": ".github/workflows/ghpipe-quality.yml"},
	  "commands": {"test": {"cwd": ".", "argv": ["npm", "run", "test"]}}
	}`)
	proj, err := Load(root)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if proj.Config.Repository != "OWNER/REPO" {
		t.Fatalf("repository = %q", proj.Config.Repository)
	}
	if proj.Config.Mode() != "strict" {
		t.Fatalf("mode = %q, want strict by default", proj.Config.Mode())
	}
	if names := proj.Config.CommandNames(); len(names) != 1 || names[0] != "test" {
		t.Fatalf("commands = %v", names)
	}
}

func TestDiscoverWalksUpToRoot(t *testing.T) {
	root := t.TempDir()
	writeConfig(t, root, `{"schema_version":1,"repository":"OWNER/REPO"}`)
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(root, "services", "payments")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	proj, err := Discover(nested)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if proj.Root != root {
		t.Fatalf("root = %q, want %q", proj.Root, root)
	}
}

func TestValidateRejectsBadInput(t *testing.T) {
	cases := map[string]string{
		"bad schema":     `{"schema_version":2}`,
		"bad repository": `{"schema_version":1,"repository":"not-a-repo"}`,
		"shell command":  `{"schema_version":1,"commands":{"test":{"cwd":".","argv":[]}}}`,
		"absolute cwd":   `{"schema_version":1,"commands":{"test":{"cwd":"/tmp","argv":["go"]}}}`,
		// The next entries guard the cross-platform defect: on Windows these
		// resolve outside the project even though filepath.IsAbs says they are
		// not absolute, and "\foo" / "..\x" slipped through on Linux too.
		"rooted cwd":           `{"schema_version":1,"commands":{"test":{"cwd":"\\foo","argv":["go"]}}}`,
		"drive backslash cwd":  `{"schema_version":1,"commands":{"test":{"cwd":"C:\\foo","argv":["go"]}}}`,
		"drive slash cwd":      `{"schema_version":1,"commands":{"test":{"cwd":"C:/foo","argv":["go"]}}}`,
		"unc cwd":              `{"schema_version":1,"commands":{"test":{"cwd":"\\\\server\\share","argv":["go"]}}}`,
		"parent cwd":           `{"schema_version":1,"commands":{"test":{"cwd":"..","argv":["go"]}}}`,
		"parent slash cwd":     `{"schema_version":1,"commands":{"test":{"cwd":"../x","argv":["go"]}}}`,
		"parent backslash cwd": `{"schema_version":1,"commands":{"test":{"cwd":"..\\x","argv":["go"]}}}`,
		"empty cwd":            `{"schema_version":1,"commands":{"test":{"cwd":"","argv":["go"]}}}`,
		"workflow escape":      `{"schema_version":1,"ci":{"workflow_path":".github/workflows/../secrets.yml"}}`,
		"workflow absolute":    `{"schema_version":1,"ci":{"workflow_path":"/etc/ghpipe.yml"}}`,
		"workflow drive":       `{"schema_version":1,"ci":{"workflow_path":"C:/ghpipe.yml"}}`,
		"workflow outside dir": `{"schema_version":1,"ci":{"workflow_path":".github/ghpipe.yml"}}`,
		"bad quality":          `{"schema_version":1,"quality":{"mode":"skip"}}`,
		"same app":             `{"schema_version":1,"apps":{"developer":{"app_id":7},"delivery":{"app_id":7}}}`,
		"dup check":            `{"schema_version":1,"ci":{"required_checks":[{"context":"c","integration_id":1},{"context":"c","integration_id":2}]}}`,
		"report no cred":       `{"schema_version":1,"reporting":{"enabled":true,"destination":"OWNER/REPO"}}`,
		"bad legacy sha":       `{"schema_version":1,"attribution":{"legacy_before":"abc"}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			writeConfig(t, root, body)
			if _, err := Load(root); err == nil {
				t.Fatalf("expected validation error for %s", name)
			}
		})
	}
}

// commandBody builds a config with one command, marshalling the cwd so the
// exact bytes under test are unambiguous even for backslash-heavy values.
func commandBody(t *testing.T, cwd string, argv []string) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"schema_version": 1,
		"commands": map[string]any{
			"test": map[string]any{"cwd": cwd, "argv": argv},
		},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(body)
}

// TestIsProjectRelative pins the predicate itself, so each rejected shape is
// exercised on every platform regardless of how filepath behaves locally.
func TestIsProjectRelative(t *testing.T) {
	rejected := map[string]string{
		"empty":                 "",
		"posix absolute":        "/tmp",
		"posix bare root":       "/",
		"windows rooted":        `\foo`,
		"windows rooted deeper": `\tmp\foo`,
		"drive backslash":       `C:\foo`,
		"drive slash":           `C:/foo`,
		"drive relative":        `C:foo`,
		"unc":                   `\\server\share`,
		"unc in front":          `//server/share`,
		"parent":                "..",
		"parent slash prefix":   "../x",
		"parent backslash":      `..\x`,
		"parent in middle":      "sub/../x",
		"parent back middle":    `sub\..\x`,
		"parent suffix":         "sub/..",
	}
	for name, p := range rejected {
		t.Run("rejects/"+name, func(t *testing.T) {
			if IsProjectRelative(p) {
				t.Fatalf("IsProjectRelative(%q) = true, want false", p)
			}
		})
	}
	allowed := map[string]string{
		"dot":              ".",
		"segment":          "sub",
		"nested":           "sub/dir",
		"dot prefix":       "./x",
		"nested backslash": `sub\dir`,
		"dotted name":      "..hidden",
		"dot dot name":     "a..b/c",
	}
	for name, p := range allowed {
		t.Run("allows/"+name, func(t *testing.T) {
			if !IsProjectRelative(p) {
				t.Fatalf("IsProjectRelative(%q) = false, want true", p)
			}
		})
	}
}

// TestValidateRejectsBadCwdShapes feeds the predicate's rejected shapes through
// the real config loader, marshalled rather than hand-escaped.
func TestValidateRejectsBadCwdShapes(t *testing.T) {
	cases := map[string]string{
		"rooted":           `\foo`,
		"drive":            `C:\foo`,
		"drive slash":      `C:/foo`,
		"unc":              `\\server\share`,
		"parent":           "..",
		"parent from here": "../x",
		"parent backslash": `..\x`,
		"empty":            "",
	}
	for name, cwd := range cases {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			writeConfig(t, root, commandBody(t, cwd, []string{"go"}))
			if _, err := Load(root); err == nil {
				t.Fatalf("expected validation error for cwd %q", cwd)
			}
		})
	}
}

// TestValidateAllowsProjectRelativeCwd is the false-kill guard: these are the
// documented shapes and must keep loading.
func TestValidateAllowsProjectRelativeCwd(t *testing.T) {
	for _, cwd := range []string{".", "sub", "sub/dir", "./x"} {
		t.Run(cwd, func(t *testing.T) {
			root := t.TempDir()
			writeConfig(t, root, commandBody(t, cwd, []string{"go", "test"}))
			if _, err := Load(root); err != nil {
				t.Fatalf("cwd %q must be valid: %v", cwd, err)
			}
		})
	}
}

func TestValidateWorkflowPath(t *testing.T) {
	valid := map[string]string{
		"slash":         ".github/workflows/ghpipe-quality.yml",
		"backslash dir": `.github\workflows\ghpipe-quality.yml`,
	}
	for name, workflow := range valid {
		t.Run("allows/"+name, func(t *testing.T) {
			root := t.TempDir()
			body, err := json.Marshal(map[string]any{
				"schema_version": 1,
				"ci":             map[string]any{"workflow_path": workflow},
			})
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			writeConfig(t, root, string(body))
			if _, err := Load(root); err != nil {
				t.Fatalf("workflow_path %q must be valid: %v", workflow, err)
			}
		})
	}
}

func TestValidateAllowsEmptyConfig(t *testing.T) {
	root := t.TempDir()
	writeConfig(t, root, `{"schema_version":1}`)
	if _, err := Load(root); err != nil {
		t.Fatalf("empty config must be valid: %v", err)
	}
}
