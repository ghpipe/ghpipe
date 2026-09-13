package project

import (
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
		"bad schema":      `{"schema_version":2}`,
		"bad repository":  `{"schema_version":1,"repository":"not-a-repo"}`,
		"shell command":   `{"schema_version":1,"commands":{"test":{"cwd":".","argv":[]}}}`,
		"absolute cwd":    `{"schema_version":1,"commands":{"test":{"cwd":"/tmp","argv":["go"]}}}`,
		"bad quality":     `{"schema_version":1,"quality":{"mode":"skip"}}`,
		"same app":        `{"schema_version":1,"apps":{"developer":{"app_id":7},"delivery":{"app_id":7}}}`,
		"dup check":       `{"schema_version":1,"ci":{"required_checks":[{"context":"c","integration_id":1},{"context":"c","integration_id":2}]}}`,
		"report no cred":  `{"schema_version":1,"reporting":{"enabled":true,"destination":"OWNER/REPO"}}`,
		"bad legacy sha":  `{"schema_version":1,"attribution":{"legacy_before":"abc"}}`,
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

func TestValidateAllowsEmptyConfig(t *testing.T) {
	root := t.TempDir()
	writeConfig(t, root, `{"schema_version":1}`)
	if _, err := Load(root); err != nil {
		t.Fatalf("empty config must be valid: %v", err)
	}
}
