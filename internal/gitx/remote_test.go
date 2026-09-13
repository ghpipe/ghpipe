package gitx

import "testing"

func TestRemoteRepository(t *testing.T) {
	valid := map[string]string{
		"https://github.com/Owner/Repo.git":             "owner/repo",
		"https://github.com/owner/repo":                 "owner/repo",
		"http://github.com/owner/repo.git":              "owner/repo",
		"https://GitHub.com/Owner/Repo.git":             "owner/repo",
		"  https://github.com/owner/repo.git  ":         "owner/repo",
		"https://github.com/owner/repo.git/":            "owner/repo",
		"git@github.com:Owner/Repo.git":                 "owner/repo",
		"git@github.com:owner/repo":                     "owner/repo",
		"ssh://git@github.com/Owner/Repo.git":           "owner/repo",
		"ssh://git@github.com:22/owner/repo.git":        "owner/repo",
		"git+ssh://git@github.com/owner/repo.git":       "owner/repo",
		"git://github.com/owner/repo.git":               "owner/repo",
		"https://user:secret@github.com/owner/repo.git": "owner/repo",
		"https://github.com/ghpipe/ghpipe.git":          "ghpipe/ghpipe",
	}
	for input, want := range valid {
		got, ok := RemoteRepository(input)
		if !ok {
			t.Errorf("RemoteRepository(%q) rejected a valid GitHub remote", input)
			continue
		}
		if got != want {
			t.Errorf("RemoteRepository(%q) = %q, want %q", input, got, want)
		}
	}

	invalid := []string{
		"",
		"   ",
		"https://gitlab.com/owner/repo.git",
		"git@gitlab.com:owner/repo.git",
		"https://github.com/owner",
		"https://github.com/owner/",
		"https://github.com/",
		"https://github.com/owner/repo/tree/main",
		"git@github.com:owner/repo/extra.git",
		"git@github.com:owner",
		"github.com/owner/repo",
		"/Users/ws/code/repo",
		"file:///Users/ws/code/repo",
		"https://github.com/owner/re po.git",
		"https://github.com//repo.git",
		"https://github.com/owner/.git",
	}
	for _, input := range invalid {
		if got, ok := RemoteRepository(input); ok {
			t.Errorf("RemoteRepository(%q) = %q, want rejection", input, got)
		}
	}
}
