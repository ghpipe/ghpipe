package github

import (
	"strings"
	"testing"
)

// AC5: caller data becomes {value}; the API vocabulary survives so the
// template stays readable.
func TestRedactEndpoint(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"pull request", "/repos/ghpipe/ghpipe/pulls/7", "/repos/{value}/{value}/pulls/{value}"},
		{"issue comment collection", "/repos/ghpipe/ghpipe/issues/42/comments", "/repos/{value}/{value}/issues/{value}/comments"},
		{"branch ref", "/repos/ghpipe/ghpipe/git/ref/heads/ghpipe/issue-1", "/repos/{value}/{value}/git/ref/heads/{value}/{value}"},
		{"check runs", "/repos/ghpipe/ghpipe/commits/0123456789/check-runs", "/repos/{value}/{value}/commits/{value}/check-runs"},
		{"org ruleset", "/orgs/ghpipe/rulesets/1234", "/orgs/{value}/rulesets/{value}"},
		{"app installation", "/app/installations/55/access_tokens", "/app/installations/{value}/access_tokens"},
		{"installation repositories", "/installation/repositories", "/installation/repositories"},
		{"authenticated user", "/user", "/user"},
		{"graphql", "/graphql", "/graphql"},
		{"absolute url", "https://api.github.com/repos/ghpipe/ghpipe", "/repos/{value}/{value}"},
		{"query is collapsed", "/repos/ghpipe/ghpipe/issues?state=open&per_page=100", "/repos/{value}/{value}/issues?{query}"},
		{"empty", "", ""},
		// The positional rule must win over the vocabulary, otherwise a repo
		// that happens to be named after an API resource would be printed.
		{"resource-shaped owner and repo", "/repos/user/issues", "/repos/{value}/{value}"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := RedactEndpoint(tc.in); got != tc.want {
				t.Errorf("RedactEndpoint(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// AC5: no error string may contain the token, a raw URL or remote text.
func TestErrorTextNeverLeaksCredentialsOrRemoteText(t *testing.T) {
	cases := map[string]*Error{
		"http": {
			Kind:     KindHTTP,
			Method:   "GET",
			Endpoint: RedactEndpoint("/repos/ghpipe/ghpipe/pulls/7"),
			Status:   403,
			Category: CategoryRepositoryAccess,
			Detail:   "the repository is not visible or the credential lacks access",
			cause:    &testError{},
		},
		"transport": {
			Kind:     KindTimeout,
			Method:   "GET",
			Endpoint: RedactEndpoint("/repos/ghpipe/ghpipe"),
			Detail:   transportDetail(KindTimeout),
			// A net/http error would render the full URL, so the cause must
			// never reach Error().
			cause: &testError{},
		},
	}

	for name, err := range cases {
		t.Run(name, func(t *testing.T) {
			text := err.Error()
			for _, leak := range []string{testToken, "ghpipe/ghpipe", "http://", "https://", "boom"} {
				if strings.Contains(text, leak) {
					t.Errorf("error text leaks %q: %s", leak, text)
				}
			}
			if err.Unwrap() == nil {
				t.Error("the cause must stay reachable through Unwrap for errors.As")
			}
		})
	}
}

// IsContract is the predicate callers use to decide "do not trust this
// result"; it must cover both the shape errors and the incompleteness errors.
func TestIsContractCoversShapeAndIncompleteness(t *testing.T) {
	contract := &Error{Kind: KindContract}
	incomplete := &Error{Kind: KindIncomplete}
	decode := &Error{Kind: KindDecode}

	if !IsContract(contract) {
		t.Error("KindContract must satisfy IsContract")
	}
	if !IsContract(incomplete) {
		t.Error("KindIncomplete must satisfy IsContract")
	}
	if IsContract(decode) {
		t.Error("KindDecode must not satisfy IsContract")
	}
	if IsContract(nil) {
		t.Error("nil must not satisfy IsContract")
	}
}

func TestRateLimitedResponsesAreLabelled(t *testing.T) {
	err := &Error{Kind: KindHTTP, Status: 403, Category: CategoryRateLimited}
	if !IsRateLimited(err) {
		t.Fatal("CategoryRateLimited must satisfy IsRateLimited")
	}
	if IsRateLimited(&Error{Kind: KindHTTP, Status: 403}) {
		t.Fatal("a plain 403 is not a rate limit")
	}
}

type testError struct{}

func (e *testError) Error() string {
	return "Get \"https://api.github.com/repos/ghpipe/ghpipe\": boom"
}
