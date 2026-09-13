// Package metadata collects and projects the read-only metadata facts of one
// ghpipe task: the issue, the pull requests associated with it, their reviews,
// the task branch, and the label/milestone projection derived from them.
//
// Collection is read-only - every call is a GET, or a GraphQL query (a POST to
// /graphql carrying no mutation) - and the projection and difference functions
// are pure. Nothing in this package writes to GitHub, to the checkout or to the
// host: the metadata slice of docs/design.md 10.3 is preview only, and the
// applying half is a later slice.
package metadata

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/ghpipe/ghpipe/internal/github"
	"github.com/ghpipe/ghpipe/internal/lifecycle"
)

// repoNameRe is the character set GitHub allows in an owner, repository and
// milestone-free label name we are willing to build a URL from.
var repoNameRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// maxLabels is the page size used for label connections. A label set larger
// than one page is refused instead of silently truncated: a partial label set
// would make the difference computation wrong, and a wrong difference is worse
// than an unreadable one (docs/design.md 7.3).
const maxLabels = 100

// Association and precondition failures. Each one means "do not guess": no
// stage is projected and no label set is proposed until the association is
// unambiguous (docs/design.md 2.3).
var (
	// ErrAmbiguousAssociation means more than one open pull request claims the
	// issue, so choosing one would be a guess.
	ErrAmbiguousAssociation = errors.New("metadata: more than one active pull request claims the issue")
	// ErrMultipleClosingReferences means a body carries more than one closing
	// reference. GitHub would act on both; ghpipe refuses to pick one.
	ErrMultipleClosingReferences = errors.New("metadata: a body carries more than one closing reference")
	// ErrCrossRepoReference means a body closes something in another
	// repository. Whether that also closes this issue cannot be decided
	// locally, so it is reported instead.
	ErrCrossRepoReference = errors.New("metadata: a body carries a closing reference to another repository")
	// ErrUnassociatedPR means a pull request carries no single standalone
	// closing reference, so it is not the task's pull request.
	ErrUnassociatedPR = errors.New("metadata: the pull request carries no unique standalone closing reference")
	// ErrNotFound means GitHub returned no object: either it does not exist or
	// the credential cannot see it. The two are deliberately not distinguished
	// (docs/design.md 7.4).
	ErrNotFound = errors.New("metadata: the object does not exist or is not visible to this credential")
)

// IssueFacts is the subset of Issue facts the projection needs.
type IssueFacts struct {
	Number    int      `json:"number"`
	Title     string   `json:"title"`
	State     string   `json:"state"` // OPEN | CLOSED
	Closed    bool     `json:"closed"`
	Body      string   `json:"body,omitempty"`
	Labels    []string `json:"labels"`
	Milestone string   `json:"milestone,omitempty"`
}

// PRFacts is the subset of pull request facts the projection and the status
// report need.
type PRFacts struct {
	Number           int      `json:"number"`
	State            string   `json:"state"` // OPEN | CLOSED
	Closed           bool     `json:"closed"`
	Draft            bool     `json:"draft"`
	Merged           bool     `json:"merged"`
	MergeCommitSHA   string   `json:"merge_commit_sha,omitempty"`
	BaseRefName      string   `json:"base_ref_name"`
	HeadRefName      string   `json:"head_ref_name"`
	HeadSHA          string   `json:"head_sha"`
	Body             string   `json:"body,omitempty"`
	Labels           []string `json:"labels"`
	Milestone        string   `json:"milestone,omitempty"`
	ReviewDecision   string   `json:"review_decision,omitempty"`
	MergeStateStatus string   `json:"merge_state_status,omitempty"`
}

// Review is one pull request review as GitHub reports it.
type Review struct {
	ID          int64  `json:"id"`
	Author      string `json:"author"`
	State       string `json:"state"` // APPROVED | CHANGES_REQUESTED | COMMENTED | DISMISSED
	CommitSHA   string `json:"commit_sha,omitempty"`
	SubmittedAt string `json:"submitted_at,omitempty"`
}

// Facts is one task as read from the remote. It is the input of Project and of
// the status assembly; every field is an observed fact, never an inference.
type Facts struct {
	Repository    string     `json:"repository"`
	DefaultBranch string     `json:"default_branch"`
	Issue         IssueFacts `json:"issue"`
	// PRs holds every pull request associated with the issue, ordered by
	// number. A pull request is associated only when it is in this repository,
	// targets the default branch, and carries exactly one standalone closing
	// reference to this issue.
	PRs []PRFacts `json:"prs"`
	// Reviews holds every review of the active pull request, in the order
	// GitHub reports them. It is empty when there is no active pull request.
	Reviews []Review `json:"reviews,omitempty"`
	// BranchExists is "the remote still advertises ghpipe/issue-N".
	BranchExists bool `json:"branch_exists"`
	// CrossRepoRefs lists pull request numbers in other repositories that
	// cross-referenced this issue. They can never be the task's pull request
	// (association is same-repository by contract), so they are reported
	// rather than treated as an error.
	CrossRepoRefs []int `json:"cross_repo_refs,omitempty"`
	// Completions carries the closing facts observed for merged pull requests,
	// keyed by pull request number. A missing entry means "nothing proven
	// yet", which projects as closing (see lifecycle.Completion). The
	// fact collector leaves it empty because the closing evidence lives
	// outside this package; status fills it in before projecting.
	Completions map[int]lifecycle.Completion `json:"-"`
	// TargetPR is the pull request the caller asked about, or 0 for an issue
	// target.
	TargetPR int `json:"-"`
}

// PR returns the associated pull request with the given number, or nil.
func (f Facts) PR(number int) *PRFacts {
	for i := range f.PRs {
		if f.PRs[i].Number == number {
			return &f.PRs[i]
		}
	}
	return nil
}

// LifecycleIssue converts the issue facts into the projection's input.
func (f Facts) LifecycleIssue() lifecycle.Issue {
	return lifecycle.Issue{Number: f.Issue.Number, Closed: f.Issue.Closed}
}

// LifecyclePRs converts the associated pull requests into the projection's
// input.
func (f Facts) LifecyclePRs() []lifecycle.PR {
	prs := make([]lifecycle.PR, 0, len(f.PRs))
	for _, pr := range f.PRs {
		prs = append(prs, lifecycle.PR{
			Number: pr.Number,
			Draft:  pr.Draft,
			Merged: pr.Merged,
			Closed: pr.Closed,
		})
	}
	return prs
}

// EffectiveReviews returns the reviews that still decide the stage: for each
// author only the last APPROVED / CHANGES_REQUESTED verdict counts, and a
// dismissal clears that author entirely.
//
// The native reviewDecision is computed the same way, so the projection and the
// reported decision agree instead of a stale "changes requested" that a later
// approval already superseded (docs/design.md 10.3: 有 CHANGES_REQUESTED ->
// changes-requested).
func (f Facts) EffectiveReviews() []lifecycle.Review {
	latest := map[string]string{}
	order := []string{}
	for _, review := range f.Reviews {
		switch review.State {
		case "APPROVED", "CHANGES_REQUESTED":
			if _, seen := latest[review.Author]; !seen {
				order = append(order, review.Author)
			}
			latest[review.Author] = review.State
		case "DISMISSED":
			if _, seen := latest[review.Author]; seen {
				delete(latest, review.Author)
			}
		}
	}
	out := make([]lifecycle.Review, 0, len(latest))
	for _, author := range order {
		if state, ok := latest[author]; ok {
			out = append(out, lifecycle.Review{State: lifecycle.ReviewState(state)})
		}
	}
	return out
}

// ActivePR returns the single open pull request, or nil when there is none.
// More than one is impossible here: Collect refuses that association, so the
// projection can never be given an ambiguous one.
func (f Facts) ActivePR() *PRFacts {
	active, err := lifecycle.ActivePR(f.LifecyclePRs())
	if err != nil || active == nil {
		return nil
	}
	return f.PR(active.Number)
}

// MergedPR returns the merged pull request with the highest number, or nil.
func (f Facts) MergedPR() *PRFacts {
	merged := lifecycle.MergedPR(f.LifecyclePRs())
	if merged == nil {
		return nil
	}
	return f.PR(merged.Number)
}

// Options selects the task to read.
type Options struct {
	// Repository is "OWNER/REPO".
	Repository string
	// Issue is the issue number to read. Exactly one of Issue and PR must be
	// set.
	Issue int
	// PR is the pull request number to read. Its closing reference names the
	// issue.
	PR int
	// MaxPages bounds the paginated walks. Zero means the package default.
	MaxPages int
}

// Collect reads one task's facts. It is the task brief's "Facts(issue N)": it
// never writes, and it fails outright rather than returning a partial ledger.
func Collect(ctx context.Context, c *github.Client, opts Options) (Facts, error) {
	var facts Facts
	if c == nil {
		return facts, errors.New("metadata: a client is required")
	}
	owner, name, err := splitRepository(opts.Repository)
	if err != nil {
		return facts, err
	}
	if (opts.Issue > 0) == (opts.PR > 0) {
		return facts, errors.New("metadata: exactly one of issue and pull request is required")
	}
	facts.Repository = owner + "/" + name

	// The default branch is read from the repository rather than assumed from
	// configuration: association is defined by the base branch GitHub actually
	// merges into (docs/design.md 10.3).
	defaultBranch, err := defaultBranchOf(ctx, c, owner, name)
	if err != nil {
		return Facts{}, err
	}
	facts.DefaultBranch = defaultBranch

	issueNumber := opts.Issue
	if opts.PR > 0 {
		number, err := issueOfPR(ctx, c, owner, name, facts.Repository, opts.PR)
		if err != nil {
			return Facts{}, err
		}
		issueNumber = number
		facts.TargetPR = opts.PR
	}

	issue, err := issueFacts(ctx, c, owner, name, issueNumber)
	if err != nil {
		return Facts{}, err
	}
	facts.Issue = issue

	candidates, crossRepo, err := crossReferencedPRs(ctx, c, owner, name, facts.Repository, issueNumber, defaultBranch, opts.MaxPages)
	if err != nil {
		return Facts{}, err
	}
	facts.CrossRepoRefs = crossRepo

	for _, candidate := range candidates {
		owns, err := classifyClaim(facts.Repository, issueNumber, candidate.Body)
		if err != nil {
			return Facts{}, err
		}
		if owns {
			facts.PRs = append(facts.PRs, candidate)
		}
	}
	sort.Slice(facts.PRs, func(i, j int) bool { return facts.PRs[i].Number < facts.PRs[j].Number })

	if open := countOpen(facts.LifecyclePRs()); open > 1 {
		return Facts{}, fmt.Errorf("%w (%d open)", ErrAmbiguousAssociation, open)
	}
	if opts.PR > 0 && facts.PR(opts.PR) == nil {
		return Facts{}, fmt.Errorf("%w: pull request #%d is not an associated pull request of issue #%d",
			ErrUnassociatedPR, opts.PR, issueNumber)
	}

	if active := facts.ActivePR(); active != nil {
		reviews, err := prReviews(ctx, c, owner, name, active.Number, opts.MaxPages)
		if err != nil {
			return Facts{}, err
		}
		facts.Reviews = reviews
	}

	_, exists, err := BranchHead(ctx, c, facts.Repository, TaskBranch(issueNumber))
	if err != nil {
		return Facts{}, err
	}
	facts.BranchExists = exists
	return facts, nil
}

// TaskBranch is the one development branch of an issue (docs/design.md 2.5).
func TaskBranch(issue int) string { return "ghpipe/issue-" + strconv.Itoa(issue) }

// BranchHead returns the SHA advertised for branch. A branch that does not
// exist is ("", false, nil): the GitHub ref endpoint answers 404 for it, and a
// repository whose issues we can already read is not "invisible", so the 404
// is an answer here rather than the ambiguous case of docs/design.md 7.4.
func BranchHead(ctx context.Context, c *github.Client, repository, branch string) (string, bool, error) {
	owner, name, err := splitRepository(repository)
	if err != nil {
		return "", false, err
	}
	if branch == "" {
		return "", false, errors.New("metadata: an empty branch name is not a branch")
	}
	path := fmt.Sprintf("/repos/%s/%s/git/ref/heads/%s", owner, name, url.PathEscape(branch))
	var payload struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err := c.Get(ctx, path, &payload); err != nil {
		if e, ok := github.As(err); ok && e.Status == http.StatusNotFound {
			return "", false, nil
		}
		return "", false, err
	}
	return payload.Object.SHA, true, nil
}

// PRHead re-reads the current head SHA of one pull request. It exists so a
// caller can confirm that the SHA it has been working with did not move while
// it read the other facts (docs/design.md 10.1: 结束前回读 PR head).
func PRHead(ctx context.Context, c *github.Client, repository string, number int) (string, error) {
	owner, name, err := splitRepository(repository)
	if err != nil {
		return "", err
	}
	var payload struct {
		Head struct {
			SHA string `json:"sha"`
		} `json:"head"`
	}
	if err := c.Get(ctx, fmt.Sprintf("/repos/%s/%s/pulls/%d", owner, name, number), &payload); err != nil {
		return "", err
	}
	if payload.Head.SHA == "" {
		return "", &github.Error{
			Kind:     github.KindContract,
			Endpoint: github.RedactEndpoint("/repos/{owner}/{repo}/pulls/{number}"),
			Detail:   "the pull request carries no head SHA",
		}
	}
	return payload.Head.SHA, nil
}

// CleanupDeclared reports whether a delivery cleanup declaration for this pull
// request exists among its comments.
//
// The declaration is "this checkout scope is cleaned up" (docs/design.md 8.5).
// cleanup itself is a later slice, so the marker parsed here is the marker that
// slice is expected to write; until it does, the fact is simply never proven
// and a merged task stays in closing. That is the safe direction: ghpipe never
// claims done on evidence it did not read.
func CleanupDeclared(ctx context.Context, c *github.Client, repository string, number int) (bool, error) {
	owner, name, err := splitRepository(repository)
	if err != nil {
		return false, err
	}
	marker := fmt.Sprintf("<!-- ghpipe:cleanup:%d -->", number)
	var comments []struct {
		Body string `json:"body"`
	}
	path := fmt.Sprintf("/repos/%s/%s/issues/%d/comments", owner, name, number)
	if err := c.Paginate(ctx, path, &comments, github.PaginateOptions{}); err != nil {
		return false, err
	}
	for _, comment := range comments {
		if strings.Contains(comment.Body, marker) {
			return true, nil
		}
	}
	return false, nil
}

// classifyClaim reports whether body claims the issue, or refuses the whole
// body. The rules, in order, are the ones docs/design.md 2.3 fixes:
//
//	another repository named          -> refuse (cannot be decided here)
//	more than one closing reference   -> refuse (two claims)
//	one standalone reference to issue -> claim
//	anything else                     -> no claim
func classifyClaim(repository string, issue int, body string) (bool, error) {
	refs := ScanClosingRefs(body)
	if len(refs) == 0 {
		return false, nil
	}
	for _, ref := range refs {
		if ref.CrossRepository(repository) {
			return false, fmt.Errorf("%w: %s", ErrCrossRepoReference, ref)
		}
	}
	if len(refs) > 1 {
		return false, fmt.Errorf("%w: found %d (%s and %s)",
			ErrMultipleClosingReferences, len(refs), refs[0], refs[1])
	}
	ref := refs[0]
	if ref.Number != issue || !ref.Standalone {
		return false, nil
	}
	return true, nil
}

func countOpen(prs []lifecycle.PR) int {
	open := 0
	for _, pr := range prs {
		if !pr.Closed && !pr.Merged {
			open++
		}
	}
	return open
}

// splitRepository validates and lower-cases "OWNER/REPO". GitHub treats owner
// and repository names case-insensitively, so two spellings must not look like
// two repositories.
func splitRepository(repository string) (string, string, error) {
	parts := strings.Split(strings.TrimSpace(repository), "/")
	if len(parts) != 2 || !repoNameRe.MatchString(parts[0]) || !repoNameRe.MatchString(parts[1]) {
		return "", "", fmt.Errorf("metadata: %q is not an OWNER/REPO repository", repository)
	}
	return strings.ToLower(parts[0]), strings.ToLower(parts[1]), nil
}

const issueQuery = `query ($owner: String!, $name: String!, $number: Int!) {
  repository(owner: $owner, name: $name) {
    issue(number: $number) {
      number
      title
      state
      body
      milestone { title }
      labels(first: 100) { totalCount nodes { name } }
    }
  }
}`

const timelineQuery = `query ($owner: String!, $name: String!, $number: Int!, $endCursor: String) {
  repository(owner: $owner, name: $name) {
    issue(number: $number) {
      timelineItems(first: 100, after: $endCursor, itemTypes: [CROSS_REFERENCED_EVENT]) {
        totalCount
        pageInfo { hasNextPage endCursor }
        nodes {
          __typename
          ... on CrossReferencedEvent {
            source {
              __typename
              ... on PullRequest {
                number
                body
                state
                isDraft
                merged
                mergeCommit { oid }
                baseRefName
                headRefName
                headRefOid
                reviewDecision
                mergeStateStatus
                milestone { title }
                labels(first: 100) { totalCount nodes { name } }
                repository { nameWithOwner }
              }
            }
          }
        }
      }
    }
  }
}`

type labelConnection struct {
	TotalCount int `json:"totalCount"`
	Nodes      []struct {
		Name string `json:"name"`
	} `json:"nodes"`
}

func (l labelConnection) names(path string) ([]string, error) {
	if l.TotalCount > maxLabels || len(l.Nodes) > maxLabels {
		return nil, &github.Error{
			Kind:     github.KindIncomplete,
			Endpoint: github.RedactEndpoint(path),
			Detail:   fmt.Sprintf("the label set has %d entries, more than one page", l.TotalCount),
		}
	}
	names := make([]string, 0, len(l.Nodes))
	for _, node := range l.Nodes {
		if node.Name == "" {
			return nil, &github.Error{
				Kind:     github.KindContract,
				Endpoint: github.RedactEndpoint(path),
				Detail:   "a label has no name",
			}
		}
		names = append(names, node.Name)
	}
	sort.Strings(names)
	return names, nil
}

type milestoneField struct {
	Title string `json:"title"`
}

func defaultBranchOf(ctx context.Context, c *github.Client, owner, name string) (string, error) {
	var payload struct {
		DefaultBranch string `json:"default_branch"`
	}
	path := fmt.Sprintf("/repos/%s/%s", owner, name)
	if err := c.Get(ctx, path, &payload); err != nil {
		return "", err
	}
	if payload.DefaultBranch == "" {
		return "", &github.Error{
			Kind:     github.KindContract,
			Endpoint: github.RedactEndpoint(path),
			Detail:   "the repository carries no default branch",
		}
	}
	return payload.DefaultBranch, nil
}

// issueOfPR resolves the issue a pull request claims, using only the pull
// request body: branch naming is deliberately not consulted (docs/design.md
// 2.3: 不靠分支命名猜).
func issueOfPR(ctx context.Context, c *github.Client, owner, name, repository string, pr int) (int, error) {
	var payload struct {
		Body string `json:"body"`
	}
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d", owner, name, pr)
	if err := c.Get(ctx, path, &payload); err != nil {
		return 0, err
	}
	refs := ScanClosingRefs(payload.Body)
	if len(refs) == 0 {
		return 0, fmt.Errorf("%w: pull request #%d", ErrUnassociatedPR, pr)
	}
	for _, ref := range refs {
		if ref.CrossRepository(repository) {
			return 0, fmt.Errorf("%w: %s", ErrCrossRepoReference, ref)
		}
	}
	if len(refs) > 1 {
		return 0, fmt.Errorf("%w: found %d", ErrMultipleClosingReferences, len(refs))
	}
	if !refs[0].Standalone || refs[0].Number <= 0 {
		return 0, fmt.Errorf("%w: pull request #%d", ErrUnassociatedPR, pr)
	}
	return refs[0].Number, nil
}

func issueFacts(ctx context.Context, c *github.Client, owner, name string, number int) (IssueFacts, error) {
	var payload struct {
		Repository *struct {
			Issue *struct {
				Number    int             `json:"number"`
				Title     string          `json:"title"`
				State     string          `json:"state"`
				Body      string          `json:"body"`
				Milestone *milestoneField `json:"milestone"`
				Labels    labelConnection `json:"labels"`
			} `json:"issue"`
		} `json:"repository"`
	}
	if err := c.GraphQL(ctx, issueQuery, map[string]any{
		"owner": owner, "name": name, "number": number,
	}, &payload); err != nil {
		return IssueFacts{}, err
	}
	if payload.Repository == nil || payload.Repository.Issue == nil {
		return IssueFacts{}, fmt.Errorf("%w: issue #%d", ErrNotFound, number)
	}
	issue := payload.Repository.Issue
	if issue.Number != number {
		return IssueFacts{}, &github.Error{
			Kind:     github.KindContract,
			Endpoint: github.RedactEndpoint("/graphql"),
			Category: github.CategoryGraphQL,
			Detail:   "the returned issue number does not match the requested one",
		}
	}
	labels, err := issue.Labels.names("/graphql")
	if err != nil {
		return IssueFacts{}, err
	}
	out := IssueFacts{
		Number: issue.Number,
		Title:  issue.Title,
		State:  strings.ToUpper(issue.State),
		Body:   issue.Body,
		Labels: labels,
	}
	out.Closed = out.State == "CLOSED"
	if issue.Milestone != nil {
		out.Milestone = issue.Milestone.Title
	}
	return out, nil
}

// crossReferencedPRs walks the issue's CROSS_REFERENCED_EVENT timeline and
// returns the pull requests in this repository that target the default branch.
// Pull requests in other repositories are reported separately; they can never
// be the task's pull request because association is same-repository.
func crossReferencedPRs(
	ctx context.Context,
	c *github.Client,
	owner, name, repository string,
	issue int,
	defaultBranch string,
	maxPages int,
) ([]PRFacts, []int, error) {
	result, err := c.PaginateGraphQL(ctx, github.GraphQLPagination{
		Query:          timelineQuery,
		Variables:      map[string]any{"owner": owner, "name": name, "number": issue},
		ConnectionPath: []string{"repository", "issue", "timelineItems"},
		MaxPages:       maxPages,
	})
	if err != nil {
		return nil, nil, err
	}

	byNumber := map[int]PRFacts{}
	order := []int{}
	crossRepo := []int{}
	for _, raw := range result.Nodes {
		var node struct {
			Typename string `json:"__typename"`
			Source   *struct {
				Typename    string `json:"__typename"`
				Number      int    `json:"number"`
				Body        string `json:"body"`
				State       string `json:"state"`
				IsDraft     bool   `json:"isDraft"`
				Merged      bool   `json:"merged"`
				MergeCommit *struct {
					OID string `json:"oid"`
				} `json:"mergeCommit"`
				BaseRefName      string          `json:"baseRefName"`
				HeadRefName      string          `json:"headRefName"`
				HeadRefOid       string          `json:"headRefOid"`
				ReviewDecision   string          `json:"reviewDecision"`
				MergeStateStatus string          `json:"mergeStateStatus"`
				Milestone        *milestoneField `json:"milestone"`
				Labels           labelConnection `json:"labels"`
				Repository       *struct {
					NameWithOwner string `json:"nameWithOwner"`
				} `json:"repository"`
			} `json:"source"`
		}
		if err := json.Unmarshal(raw, &node); err != nil {
			return nil, nil, &github.Error{
				Kind:     github.KindContract,
				Endpoint: github.RedactEndpoint("/graphql"),
				Category: github.CategoryGraphQL,
				Detail:   "a timeline item does not match the requested shape",
			}
		}
		if node.Source == nil || node.Source.Typename != "PullRequest" {
			// A cross-reference from an issue, a commit or a discussion cannot
			// be the task's pull request.
			continue
		}
		source := node.Source
		if source.Repository == nil || source.Repository.NameWithOwner == "" {
			return nil, nil, &github.Error{
				Kind:     github.KindContract,
				Endpoint: github.RedactEndpoint("/graphql"),
				Category: github.CategoryGraphQL,
				Detail:   "a cross-referenced pull request carries no repository",
			}
		}
		if !strings.EqualFold(source.Repository.NameWithOwner, repository) {
			crossRepo = append(crossRepo, source.Number)
			continue
		}
		if source.BaseRefName != defaultBranch {
			// A pull request against another branch is not a claim on this
			// task: the merge that closes the issue is the one into the
			// default branch (docs/design.md 2.3).
			continue
		}
		if source.Number <= 0 {
			return nil, nil, &github.Error{
				Kind:     github.KindContract,
				Endpoint: github.RedactEndpoint("/graphql"),
				Category: github.CategoryGraphQL,
				Detail:   "a cross-referenced pull request carries no number",
			}
		}
		labels, err := source.Labels.names("/graphql")
		if err != nil {
			return nil, nil, err
		}
		pr := PRFacts{
			Number:           source.Number,
			State:            strings.ToUpper(source.State),
			Draft:            source.IsDraft,
			Merged:           source.Merged,
			BaseRefName:      source.BaseRefName,
			HeadRefName:      source.HeadRefName,
			HeadSHA:          source.HeadRefOid,
			Body:             source.Body,
			Labels:           labels,
			ReviewDecision:   source.ReviewDecision,
			MergeStateStatus: source.MergeStateStatus,
		}
		pr.Closed = pr.State == "CLOSED" || pr.Merged
		if source.MergeCommit != nil {
			pr.MergeCommitSHA = source.MergeCommit.OID
		}
		if source.Milestone != nil {
			pr.Milestone = source.Milestone.Title
		}
		if pr.Merged && pr.MergeCommitSHA == "" {
			return nil, nil, &github.Error{
				Kind:     github.KindContract,
				Endpoint: github.RedactEndpoint("/graphql"),
				Category: github.CategoryGraphQL,
				Detail:   "a merged pull request carries no merge commit",
			}
		}
		if _, seen := byNumber[pr.Number]; !seen {
			order = append(order, pr.Number)
		}
		byNumber[pr.Number] = pr
	}

	prs := make([]PRFacts, 0, len(order))
	for _, number := range order {
		prs = append(prs, byNumber[number])
	}
	sort.Ints(crossRepo)
	return prs, crossRepo, nil
}

// prReviews reads every review of one pull request. Reviews decide a stage, so
// a short page would decide it wrongly: the walk is complete or it fails
// (docs/design.md 7.2).
func prReviews(ctx context.Context, c *github.Client, owner, name string, number, maxPages int) ([]Review, error) {
	var raw []struct {
		ID    int64  `json:"id"`
		State string `json:"state"`
		User  *struct {
			Login string `json:"login"`
		} `json:"user"`
		CommitID    string `json:"commit_id"`
		SubmittedAt string `json:"submitted_at"`
	}
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d/reviews", owner, name, number)
	if err := c.Paginate(ctx, path, &raw, github.PaginateOptions{MaxPages: maxPages}); err != nil {
		return nil, err
	}
	reviews := make([]Review, 0, len(raw))
	for _, item := range raw {
		review := Review{
			ID:          item.ID,
			State:       strings.ToUpper(item.State),
			CommitSHA:   item.CommitID,
			SubmittedAt: item.SubmittedAt,
		}
		if item.User != nil {
			review.Author = item.User.Login
		}
		reviews = append(reviews, review)
	}
	return reviews, nil
}
