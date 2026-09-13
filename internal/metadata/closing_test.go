package metadata

import "testing"

func TestScanClosingRefs(t *testing.T) {
	cases := []struct {
		name string
		body string
		refs []ClosingRef
	}{
		{
			name: "standalone line",
			body: "Closes #7",
			refs: []ClosingRef{{Keyword: "closes", Number: 7, Line: 1, Standalone: true}},
		},
		{
			name: "standalone with trailing punctuation",
			body: "closes #7.",
			refs: []ClosingRef{{Keyword: "closes", Number: 7, Line: 1, Standalone: true}},
		},
		{
			name: "list marker still occupies the line",
			body: "- Closes #7",
			refs: []ClosingRef{{Keyword: "closes", Number: 7, Line: 1, Standalone: true}},
		},
		{
			name: "ordered list marker",
			body: "1. Fixes #8",
			refs: []ClosingRef{{Keyword: "fixes", Number: 8, Line: 1, Standalone: true}},
		},
		{
			name: "keyword with colon",
			body: "Resolves: #12",
			refs: []ClosingRef{{Keyword: "resolves", Number: 12, Line: 1, Standalone: true}},
		},
		{
			name: "inside a sentence is not a claim",
			body: "This pull request closes #7 while touching other files",
			refs: []ClosingRef{{Keyword: "closes", Number: 7, Line: 1}},
		},
		{
			name: "repository qualified",
			body: "Fixes other/repo#7",
			refs: []ClosingRef{{Keyword: "fixes", Number: 7, Owner: "other", Repo: "repo", Line: 1, Standalone: true}},
		},
		{
			name: "url form",
			body: "Closes https://github.com/ghpipe/ghpipe/issues/7",
			refs: []ClosingRef{{
				Keyword: "closes", Number: 7, Host: "github.com", Owner: "ghpipe", Repo: "ghpipe",
				URL:  "https://github.com/ghpipe/ghpipe/issues/7",
				Line: 1, Standalone: true,
			}},
		},
		{
			name: "url form pull path",
			body: "Closes https://github.com/ghpipe/ghpipe/pull/12",
			refs: []ClosingRef{{
				Keyword: "closes", Number: 12, Host: "github.com", Owner: "ghpipe", Repo: "ghpipe",
				URL:  "https://github.com/ghpipe/ghpipe/pull/12",
				Line: 1, Standalone: true,
			}},
		},
		{
			name: "a url on a host ghpipe does not speak",
			body: "Closes https://gitlab.com/foo/issues/7",
			refs: []ClosingRef{{
				Keyword: "closes", Number: 7, Host: "gitlab.com",
				URL:  "https://gitlab.com/foo/issues/7",
				Line: 1, Standalone: true,
			}},
		},
		{
			name: "a url whose path carries no owner and repository",
			body: "Closes https://example.com/issues/7",
			refs: []ClosingRef{{
				Keyword: "closes", Number: 7, Host: "example.com",
				URL:  "https://example.com/issues/7",
				Line: 1, Standalone: true,
			}},
		},
		{
			name: "a url that is not an issue is prose",
			body: "Fixes https://ci.example.com/build/1",
			refs: nil,
		},
		{
			name: "two references are two claims",
			body: "Closes #7\nFixes #8",
			refs: []ClosingRef{
				{Keyword: "closes", Number: 7, Line: 1, Standalone: true},
				{Keyword: "fixes", Number: 8, Line: 2, Standalone: true},
			},
		},
		{
			name: "code fences are scanned too",
			body: "```\nCloses #7\n```",
			refs: []ClosingRef{{Keyword: "closes", Number: 7, Line: 2, Standalone: true}},
		},
		{
			name: "nothing to close",
			body: "see #7 for context",
			refs: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ScanClosingRefs(tc.body)
			if len(got) != len(tc.refs) {
				t.Fatalf("got %d references (%v), want %d (%v)", len(got), got, len(tc.refs), tc.refs)
			}
			for i := range got {
				if got[i] != tc.refs[i] {
					t.Errorf("reference %d = %+v, want %+v", i, got[i], tc.refs[i])
				}
			}
		})
	}
}

func TestClosingRefInRepository(t *testing.T) {
	cases := []struct {
		name string
		ref  ClosingRef
		want bool
	}{
		{"unqualified is not a repository claim", ClosingRef{Number: 7}, false},
		{"same repository", ClosingRef{Owner: "ghpipe", Repo: "ghpipe", Number: 7}, true},
		{"same repository, different case", ClosingRef{Owner: "GHpipe", Repo: "GHpipe", Number: 7}, true},
		{"another repository", ClosingRef{Owner: "other", Repo: "ghpipe", Number: 7}, false},
		{"another host", ClosingRef{Host: "github.example.com", Owner: "ghpipe", Repo: "ghpipe", Number: 7}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.ref.InRepository("ghpipe/ghpipe"); got != tc.want {
				t.Errorf("InRepository = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestClosingRefCrossRepository is the regression test for the defect the
// reviewer found in a571dd5: a URL the parser cannot place in this repository
// used to fall back to the unqualified reading and was therefore accepted as a
// claim. Every URL form that is not provably this repository must be refused.
func TestClosingRefCrossRepository(t *testing.T) {
	const repository = "ghpipe/ghpipe"
	cases := []struct {
		name string
		ref  ClosingRef
		want bool
	}{
		{"the unqualified form means this repository", ClosingRef{Number: 7}, false},
		{"the shorthand form names this repository", ClosingRef{Owner: "ghpipe", Repo: "ghpipe", Number: 7}, false},
		{"the shorthand form names another repository", ClosingRef{Owner: "other", Repo: "ghpipe", Number: 7}, true},
		{"a github.com url names this repository", ClosingRef{
			Host: "github.com", Owner: "ghpipe", Repo: "ghpipe", Number: 7,
			URL: "https://github.com/ghpipe/ghpipe/issues/7",
		}, false},
		{"a github.com url names another repository", ClosingRef{
			Host: "github.com", Owner: "other", Repo: "repo", Number: 7,
			URL: "https://github.com/other/repo/issues/7",
		}, true},
		{"a github.com url with no owner and repository", ClosingRef{
			Host: "github.com", Number: 7, URL: "https://github.com/issues/7",
		}, true},
		{"another host, however the path is spelled", ClosingRef{
			Host: "github.example.com", Owner: "ghpipe", Repo: "ghpipe", Number: 7,
			URL: "https://github.example.com/ghpipe/ghpipe/issues/7",
		}, true},
		{"another host whose path resolves", ClosingRef{
			Host: "gitlab.com", Owner: "foo", Repo: "bar", Number: 7,
			URL: "https://gitlab.com/foo/bar/issues/7",
		}, true},
		{"another host whose path does not resolve", ClosingRef{
			Host: "example.com", Number: 7, URL: "https://example.com/issues/7",
		}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.ref.CrossRepository(repository); got != tc.want {
				t.Errorf("CrossRepository(%q) = %v, want %v", repository, got, tc.want)
			}
		})
	}
}
