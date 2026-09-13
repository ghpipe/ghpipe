package metadata

import (
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// This file holds the "Closes #N" grammar. It is the only rule that associates
// a pull request with an issue (docs/design.md 2.3), so it lives in exactly one
// place, it is pure, and it fails closed: anything that is not exactly one
// standalone closing reference for the issue is not an association.
//
// docs/design.md 4.2 reserves this grammar for internal/attribution, which does
// not exist yet. The task brief for #7 therefore allows the equivalent parser to
// live here; moving this file into internal/attribution later changes no caller
// contract.

// closingRefRe matches one closing reference. Group 1 is the keyword, group 2
// is a URL form and group 3 the number inside it, groups 4 and 5 are the
// repository qualifier of the shorthand form and group 6 its number.
//
// A bare URL is deliberately not a reference: "fixes https://ci.example/1"
// is prose, not a claim on an issue, so the URL alternative requires an
// /issues/N or /pull/N path.
var closingRefRe = regexp.MustCompile(
	`(?i)\b(close[sd]?|fix(?:e[sd])?|resolve[sd]?)\b[ \t]*:?[ \t]+(?:` +
		`(https?://[^\s<>()\[\]"']+/(?:issues|pull)/([0-9]{1,10}))` +
		`|` +
		`(?:([A-Za-z0-9][A-Za-z0-9_.-]*)/([A-Za-z0-9][A-Za-z0-9_.-]*))?[ \t]*#[ \t]*([0-9]{1,10})` +
		`)`)

// ClosingRef is one closing-keyword reference found in a body: "#7" in
// "Closes #7", "other/repo#3" in "Fixes other/repo#3", or the URL form
// "Closes https://github.com/other/repo/issues/3".
type ClosingRef struct {
	// Keyword is the closing keyword as written, lower-cased.
	Keyword string
	// Number is the referenced issue or pull request number.
	Number int
	// Owner and Repo qualify the reference when it names a repository. They
	// are empty for the unqualified "#N" form.
	Owner string
	Repo  string
	// Host is the host of a URL form, empty for every shorthand form.
	Host string
	// URL is the raw URL of a URL form, empty for every shorthand form. It is
	// kept so that a refusal can quote what the author actually wrote: the URL
	// form whose repository cannot be resolved renders as its host alone
	// otherwise, and nobody writes "closes example.com#7".
	URL string
	// Line is the 1-based line the reference starts on.
	Line int
	// Standalone is true when the reference occupies its line alone, apart
	// from an optional markdown list marker and trailing punctuation. The
	// association requires it (docs/design.md 2.3): "Closes #7" inside a
	// sentence is prose, not a claim.
	Standalone bool
}

// Qualified reports whether the reference names a repository explicitly.
func (r ClosingRef) Qualified() bool { return r.Owner != "" && r.Repo != "" }

// CrossRepository reports whether the reference provably names something that
// is not repository, so that it can never be a claim on it.
//
// The URL form fails closed on both counts: a host ghpipe does not speak, and a
// path that does not carry an "owner/repo" pair, both name something ghpipe
// cannot place in this repository. Neither may fall back to the unqualified
// "#N" reading, because that reading means "this repository" - falling back
// would turn an explicit pointer at something else into a claim here, which is
// the one mistake this grammar exists to prevent (docs/design.md 2.3).
func (r ClosingRef) CrossRepository(repository string) bool {
	if r.Host == "" {
		return r.Qualified() && !r.InRepository(repository)
	}
	return !r.InRepository(repository)
}

// InRepository reports whether the reference names repository
// (case-insensitively) on github.com.
func (r ClosingRef) InRepository(repository string) bool {
	if !r.Qualified() {
		return false
	}
	if r.Host != "" && !strings.EqualFold(r.Host, "github.com") {
		// A host ghpipe does not speak cannot be the same repository, however
		// the path is spelled.
		return false
	}
	return strings.EqualFold(r.Owner+"/"+r.Repo, repository)
}

// String renders the reference for a redacted error detail.
func (r ClosingRef) String() string {
	switch {
	case r.URL != "":
		return fmt.Sprintf("%s %s", r.Keyword, r.URL)
	case r.Qualified():
		return fmt.Sprintf("%s %s/%s#%d", r.Keyword, r.Owner, r.Repo, r.Number)
	default:
		return fmt.Sprintf("%s #%d", r.Keyword, r.Number)
	}
}

// ScanClosingRefs returns every closing reference in body, in order.
//
// The whole body is scanned, including code blocks and block quotes: a second
// closing reference anywhere is a second claim on an issue (docs/design.md
// 2.3), so it must be visible to the caller rather than hidden by a markdown
// parser. Nothing is deduplicated - "Closes #7" written twice is two
// references, because "once" is the contract.
func ScanClosingRefs(body string) []ClosingRef {
	matches := closingRefRe.FindAllStringSubmatchIndex(body, -1)
	if len(matches) == 0 {
		return nil
	}
	refs := make([]ClosingRef, 0, len(matches))
	line := 1
	scanned := 0
	for _, m := range matches {
		line += strings.Count(body[scanned:m[0]], "\n")
		scanned = m[0]
		ref := ClosingRef{
			Keyword:    strings.ToLower(body[m[2]:m[3]]),
			Line:       line,
			Standalone: standaloneAt(body, m[0], m[1]),
		}
		if m[4] >= 0 {
			// URL form: the number was captured, the repository comes from
			// the URL path.
			ref.Number = parseNumber(body[m[6]:m[7]])
			ref.URL = body[m[4]:m[5]]
			ref.Host, ref.Owner, ref.Repo = splitIssueURL(body[m[4]:m[5]])
		} else {
			if m[8] >= 0 {
				ref.Owner = body[m[8]:m[9]]
				ref.Repo = body[m[10]:m[11]]
			}
			ref.Number = parseNumber(body[m[12]:m[13]])
		}
		refs = append(refs, ref)
	}
	return refs
}

// splitIssueURL extracts "owner/repo" from a github-style issue URL. Anything
// that is not "<host>/<owner>/<repo>/<issues|pull>/<n>" yields an empty
// repository, which leaves the reference unqualified.
func splitIssueURL(raw string) (host, owner, repo string) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", ""
	}
	segments := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(segments) < 4 {
		return u.Host, "", ""
	}
	owner = segments[len(segments)-4]
	repo = segments[len(segments)-3]
	if !repoNameRe.MatchString(owner) || !repoNameRe.MatchString(repo) {
		return u.Host, "", ""
	}
	return u.Host, owner, repo
}

func parseNumber(raw string) int {
	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0
	}
	return value
}

// standaloneAt reports whether the matched reference is the whole line, apart
// from an optional markdown list marker and trailing sentence punctuation.
//
// A trailing "." is accepted because "#7." is still the reference occupying
// the line; trailing words are not, because "#7 fixes the flaky test" is a
// sentence whose closing keyword GitHub would still honour. ghpipe refuses to
// guess which of the two the author meant.
func standaloneAt(body string, start, end int) bool {
	lineStart := strings.LastIndexByte(body[:start], '\n') + 1
	lineEnd := len(body)
	if i := strings.IndexByte(body[end:], '\n'); i >= 0 {
		lineEnd = end + i
	}
	prefix := trimListMarker(strings.TrimSpace(body[lineStart:start]))
	suffix := strings.TrimRight(strings.TrimSpace(body[end:lineEnd]), ".,;:")
	return prefix == "" && suffix == ""
}

// trimListMarker drops a leading markdown list marker ("-", "*", "+" or
// "1." / "1)") together with the space after it, so "- Closes #7" still counts
// as the line being the reference.
func trimListMarker(s string) string {
	if s == "" {
		return s
	}
	switch s[0] {
	case '-', '*', '+':
		return strings.TrimSpace(s[1:])
	}
	digits := 0
	for digits < len(s) && s[digits] >= '0' && s[digits] <= '9' {
		digits++
	}
	if digits > 0 && digits < len(s) && (s[digits] == '.' || s[digits] == ')') {
		return strings.TrimSpace(s[digits+1:])
	}
	return s
}
