package gitx

import (
	"net/url"
	"regexp"
	"strings"
)

// scpLikeRe matches git's scp-like syntax, "user@host:path", which is not a
// URL and cannot be parsed by net/url.
var scpLikeRe = regexp.MustCompile(`^[^@/]+@([^/:]+):(.+)$`)

// nameRe is the character set GitHub allows in an owner or repository name.
var nameRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// RemoteRepository extracts the canonical "OWNER/REPO" from a GitHub remote
// URL. It accepts the forms git actually writes into a clone:
//
//	https://github.com/owner/repo.git
//	http://github.com/owner/repo
//	git@github.com:owner/repo.git
//	ssh://git@github.com/owner/repo.git
//	ssh://git@github.com:22/owner/repo.git
//
// The result is lower-case, because GitHub treats owner and repository names
// case-insensitively and two spellings must not look like two repositories. A
// URL that is not a GitHub repository - a different host, a missing component,
// extra path segments, characters GitHub does not allow - returns ok=false.
// This is a pure function: no network, no filesystem, no environment.
func RemoteRepository(rawURL string) (string, bool) {
	host, path, ok := splitRemote(strings.TrimSpace(rawURL))
	if !ok || !strings.EqualFold(host, "github.com") {
		return "", false
	}
	segments := strings.Split(strings.Trim(path, "/"), "/")
	if len(segments) != 2 {
		return "", false
	}
	owner := segments[0]
	repo := strings.TrimSuffix(segments[1], ".git")
	if !nameRe.MatchString(owner) || !nameRe.MatchString(repo) {
		return "", false
	}
	return strings.ToLower(owner) + "/" + strings.ToLower(repo), true
}

// splitRemote returns the host and path of a git remote URL in either URL or
// scp-like form.
func splitRemote(rawURL string) (host, path string, ok bool) {
	if rawURL == "" {
		return "", "", false
	}
	if strings.Contains(rawURL, "://") {
		parsed, err := url.Parse(rawURL)
		if err != nil {
			return "", "", false
		}
		switch parsed.Scheme {
		case "https", "http", "ssh", "git", "git+ssh":
		default:
			return "", "", false
		}
		if parsed.Hostname() == "" {
			return "", "", false
		}
		return parsed.Hostname(), parsed.Path, true
	}
	match := scpLikeRe.FindStringSubmatch(rawURL)
	if match == nil {
		return "", "", false
	}
	return match[1], match[2], true
}
