// Package checks evaluates the source-bound required status checks of one
// commit.
//
// A check is only evidence when ghpipe can say three things about it
// (docs/design.md 2.4, 10.4):
//
//   - it is the check the repository's effective rules require, with the same
//     integration id - a config entry that merely shares the context name is
//     not the same evidence;
//   - it belongs to the commit being judged, not to some other one that happens
//     to share a name;
//   - every source that published a result under that name passed. When a
//     check-run and a commit status both exist, both must pass: taking the
//     greenest of the two is exactly how a red gate gets merged.
//
// The package is read-only. It reads the effective rules and the commit's
// status rollup and returns an evaluation; it never writes and it never
// retries.
package checks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/ghpipe/ghpipe/internal/github"
)

// shaRe is the only SHA form ghpipe compares: a full, lower-case 40-character
// object name. Anything else makes "same commit" unanswerable.
var shaRe = regexp.MustCompile(`^[0-9a-f]{40}$`)

// repoNameRe matches the characters GitHub allows in an owner or repository
// name.
var repoNameRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// ErrInvalidRequirement means the required-check configuration itself is
// unusable. It is a precondition failure: nothing remote has to be read for it
// to be decided.
var ErrInvalidRequirement = errors.New("checks: invalid required check configuration")

// State is the outcome of one required check.
type State string

// The check states, ordered by severity: failed > unknown > pending > ready.
// "unknown" outranks "pending" because a check whose source could not be
// verified is not merely late - it is unproven.
const (
	StateReady   State = "ready"
	StatePending State = "pending"
	StateUnknown State = "unknown"
	StateFailed  State = "failed"
)

// rank orders the states by severity.
func rank(s State) int {
	switch s {
	case StateFailed:
		return 3
	case StateUnknown:
		return 2
	case StatePending:
		return 1
	default:
		return 0
	}
}

// Worst returns the most severe of the given states.
func Worst(states ...State) State {
	worst := StateReady
	for _, state := range states {
		if rank(state) > rank(worst) {
			worst = state
		}
	}
	return worst
}

// Required is one source-bound required check, as configured.
type Required struct {
	Context       string `json:"context"`
	IntegrationID int    `json:"integration_id"`
}

// Unique validates the configured required checks and returns them in a stable
// order. A context names one check, so a repeated context is refused: two
// entries with the same name but different sources would make "which one is
// required" undecidable, and picking the first would be a guess.
func Unique(required []Required) ([]Required, error) {
	out := make([]Required, 0, len(required))
	seen := make(map[string]bool, len(required))
	for _, check := range required {
		if strings.TrimSpace(check.Context) == "" {
			return nil, fmt.Errorf("%w: a required check needs a non-empty context", ErrInvalidRequirement)
		}
		if check.IntegrationID <= 0 {
			return nil, fmt.Errorf("%w: required check %q needs a positive integration id",
				ErrInvalidRequirement, check.Context)
		}
		if seen[check.Context] {
			return nil, fmt.Errorf("%w: duplicate required check context %q", ErrInvalidRequirement, check.Context)
		}
		seen[check.Context] = true
		out = append(out, check)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Context < out[j].Context })
	return out, nil
}

// Item is one required check's evaluated state.
type Item struct {
	Context       string `json:"context"`
	IntegrationID int    `json:"integration_id"`
	State         State  `json:"state"`
	// Conclusion keeps the raw conclusion or state GitHub reported, joined
	// with "," when several sources published under this context. It is never
	// translated: "skipped" and "neutral" are reported as exactly that, not as
	// "the tests passed" (docs/design.md 2.3).
	Conclusion string `json:"conclusion,omitempty"`
	// Sources names every source that published a result under this context:
	// "check_run" and/or "commit_status".
	Sources []string `json:"sources,omitempty"`
	// Detail is a short local explanation, never remote text.
	Detail string `json:"detail,omitempty"`
}

// Evaluation is the result of judging one commit.
type Evaluation struct {
	SHA    string `json:"sha"`
	Branch string `json:"branch"`
	// RulesRead is false when the effective rules could not be read, in which
	// case no source binding could be verified and every item is unknown.
	RulesRead bool   `json:"rules_read"`
	Items     []Item `json:"items"`
}

// State merges the items into one state: the worst item decides.
func (e Evaluation) State() State {
	states := make([]State, 0, len(e.Items))
	for _, item := range e.Items {
		states = append(states, item.State)
	}
	return Worst(states...)
}

// Request selects the commit to judge.
type Request struct {
	// Repository is "OWNER/REPO".
	Repository string
	// SHA is the commit the checks must be bound to.
	SHA string
	// Branch is the branch whose effective rules apply: the rules of the base
	// branch a merge would land on.
	Branch string
	// Required is the configured required-check set.
	Required []Required
	// MaxPages bounds the paginated walks. Zero means the package default.
	MaxPages int
}

// Evaluate reads the effective rules and the commit's status rollup and returns
// one item per required check.
//
// An unreadable rule set is not a failure of the command: it means the source
// binding cannot be verified, so every item is unknown (docs/design.md 10.4). A
// rollup that violates its contract - a SHA that does not match, a check run
// belonging to another commit, a malformed context - is returned as an error
// together with items that are explicitly unknown, so a caller can never
// mistake an unproven result for a passing one.
func Evaluate(ctx context.Context, c *github.Client, req Request) (Evaluation, error) {
	ev := Evaluation{SHA: strings.ToLower(strings.TrimSpace(req.SHA)), Branch: req.Branch}
	if c == nil {
		return ev, errors.New("checks: a client is required")
	}
	owner, name, err := splitRepository(req.Repository)
	if err != nil {
		return ev, err
	}
	if !shaRe.MatchString(ev.SHA) {
		return ev, fmt.Errorf("%w: sha %q must be a full 40-character lower-case hex SHA", ErrInvalidRequirement, req.SHA)
	}
	if strings.TrimSpace(req.Branch) == "" {
		return ev, fmt.Errorf("%w: a branch is required to read the effective rules", ErrInvalidRequirement)
	}
	required, err := Unique(req.Required)
	if err != nil {
		return ev, err
	}
	ev.Items = make([]Item, 0, len(required))

	rules, rulesErr := effectiveRules(ctx, c, owner, name, req.Branch, req.MaxPages)
	if rulesErr != nil {
		// Unreadable rules mean the source binding cannot be verified at all.
		// Every required check is unknown, and no rollup is consulted: a green
		// check whose source is unproven is not evidence (docs/design.md 10.4).
		for _, check := range required {
			ev.Items = append(ev.Items, Item{
				Context:       check.Context,
				IntegrationID: check.IntegrationID,
				State:         StateUnknown,
				Detail:        "the effective rules could not be read (" + kindOf(rulesErr) + ")",
			})
		}
		return ev, nil
	}
	ev.RulesRead = true

	bindings := map[string]int{}
	for _, rule := range rules {
		for _, binding := range rule.Parameters.RequiredStatusChecks {
			if binding.Context == "" {
				return ev, contractError("/repos/{owner}/{repo}/rules/branches/{branch}",
					"a required status check rule carries no context")
			}
			bindings[binding.Context] = binding.IntegrationID
		}
	}
	for _, check := range required {
		item := Item{Context: check.Context, IntegrationID: check.IntegrationID}
		switch id, declared := bindings[check.Context]; {
		case !declared:
			item.State = StateUnknown
			item.Detail = "the effective rules do not declare this required check"
		case id != check.IntegrationID:
			item.State = StateFailed
			item.Detail = "source_binding_mismatch"
		default:
			item.State = StateReady
		}
		ev.Items = append(ev.Items, item)
	}

	entries, err := commitRollup(ctx, c, owner, name, ev.SHA, req.MaxPages)
	if err != nil {
		for i := range ev.Items {
			if ev.Items[i].State != StateReady {
				// Already refused above: a mismatched or undeclared source is
				// not made acceptable by a green check under the same name.
				continue
			}
			ev.Items[i].State = StateUnknown
			ev.Items[i].Detail = "the commit status rollup could not be read (" + kindOf(err) + ")"
		}
		return ev, err
	}
	for i := range ev.Items {
		if ev.Items[i].State != StateReady {
			continue
		}
		applyRollup(&ev.Items[i], entries[ev.Items[i].Context])
	}
	return ev, nil
}

// applyRollup folds every rollup entry published under one context into the
// item. All of them must pass.
func applyRollup(item *Item, entries []rollupEntry) {
	if len(entries) == 0 {
		item.State = StatePending
		item.Detail = "no result was reported for this commit"
		return
	}
	states := make([]State, 0, len(entries))
	conclusions := make([]string, 0, len(entries))
	sources := map[string]bool{}
	for _, entry := range entries {
		states = append(states, entry.State)
		conclusions = append(conclusions, entry.Raw)
		sources[entry.Source] = true
	}
	item.State = Worst(states...)
	item.Conclusion = strings.Join(uniqueSorted(conclusions), ",")
	item.Sources = sortedKeys(sources)
	if len(entries) > 1 {
		item.Detail = "several sources reported this context; all of them must pass"
	}
}

// rollupEntry is one result published under a check context.
type rollupEntry struct {
	Source string
	State  State
	Raw    string
}

type rule struct {
	Type       string `json:"type"`
	Parameters struct {
		RequiredStatusChecks []struct {
			Context       string `json:"context"`
			IntegrationID int    `json:"integration_id"`
		} `json:"required_status_checks"`
	} `json:"parameters"`
}

// effectiveRules reads the rules that apply to a branch. The endpoint answers
// with a plain array, but it is still walked as a paginated list: a repository
// with many rules must not look rule-less.
func effectiveRules(ctx context.Context, c *github.Client, owner, name, branch string, maxPages int) ([]rule, error) {
	var rules []rule
	path := fmt.Sprintf("/repos/%s/%s/rules/branches/%s", owner, name, url.PathEscape(branch))
	if err := c.Paginate(ctx, path, &rules, github.PaginateOptions{MaxPages: maxPages}); err != nil {
		return nil, err
	}
	return rules, nil
}

const commitQuery = `query ($owner: String!, $name: String!, $oid: GitObjectID!) {
  repository(owner: $owner, name: $name) {
    object(oid: $oid) {
      __typename
      ... on Commit {
        oid
        statusCheckRollup { contexts(first: 1) { totalCount } }
      }
    }
  }
}`

const rollupQuery = `query ($owner: String!, $name: String!, $oid: GitObjectID!, $endCursor: String) {
  repository(owner: $owner, name: $name) {
    object(oid: $oid) {
      ... on Commit {
        statusCheckRollup {
          contexts(first: 100, after: $endCursor) {
            totalCount
            pageInfo { hasNextPage endCursor }
            nodes {
              __typename
              ... on CheckRun {
                name
                status
                conclusion
                checkSuite {
                  commit { oid }
                  app { databaseId }
                }
              }
              ... on StatusContext {
                context
                state
                creator { login }
              }
            }
          }
        }
      }
    }
  }
}`

// commitRollup reads the commit's check results. It first reads the commit
// itself, because the rollup is only evidence for the commit it belongs to: the
// returned oid must equal the requested SHA, and so must the commit of every
// check run in the rollup (docs/design.md 7.3).
func commitRollup(ctx context.Context, c *github.Client, owner, name, sha string, maxPages int) (map[string][]rollupEntry, error) {
	var head struct {
		Repository *struct {
			Object *struct {
				OID               string `json:"oid"`
				StatusCheckRollup *struct {
					Contexts *struct {
						TotalCount int `json:"totalCount"`
					} `json:"contexts"`
				} `json:"statusCheckRollup"`
			} `json:"object"`
		} `json:"repository"`
	}
	if err := c.GraphQL(ctx, commitQuery, map[string]any{"owner": owner, "name": name, "oid": sha}, &head); err != nil {
		return nil, err
	}
	if head.Repository == nil || head.Repository.Object == nil {
		return nil, contractError("/graphql", "the requested commit is not visible in this repository")
	}
	if !strings.EqualFold(head.Repository.Object.OID, sha) {
		return nil, contractError("/graphql", "the returned commit oid does not match the requested SHA")
	}
	if head.Repository.Object.StatusCheckRollup == nil || head.Repository.Object.StatusCheckRollup.Contexts == nil {
		// A commit nobody has checked has no rollup at all. That is an answer:
		// every required check is pending, not an error.
		return map[string][]rollupEntry{}, nil
	}

	result, err := c.PaginateGraphQL(ctx, github.GraphQLPagination{
		Query:          rollupQuery,
		Variables:      map[string]any{"owner": owner, "name": name, "oid": sha},
		ConnectionPath: []string{"repository", "object", "statusCheckRollup", "contexts"},
		MaxPages:       maxPages,
	})
	if err != nil {
		return nil, err
	}

	entries := map[string][]rollupEntry{}
	for _, raw := range result.Nodes {
		var node struct {
			Typename   string `json:"__typename"`
			Name       string `json:"name"`
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
			CheckSuite *struct {
				Commit *struct {
					OID string `json:"oid"`
				} `json:"commit"`
			} `json:"checkSuite"`
			Context string `json:"context"`
			State   string `json:"state"`
		}
		if err := json.Unmarshal(raw, &node); err != nil {
			return nil, contractError("/graphql", "a status check rollup entry does not match the requested shape")
		}
		switch node.Typename {
		case "CheckRun":
			if node.Name == "" {
				return nil, contractError("/graphql", "a check run carries no name")
			}
			if node.CheckSuite == nil || node.CheckSuite.Commit == nil || node.CheckSuite.Commit.OID == "" {
				return nil, contractError("/graphql", "a check run carries no commit")
			}
			if !strings.EqualFold(node.CheckSuite.Commit.OID, sha) {
				return nil, contractError("/graphql", "a check run in the rollup belongs to another commit")
			}
			status := strings.ToUpper(node.Status)
			conclusion := strings.ToUpper(node.Conclusion)
			switch status {
			case "COMPLETED":
				switch conclusion {
				case "SUCCESS", "SKIPPED", "NEUTRAL":
					entries[node.Name] = append(entries[node.Name], rollupEntry{
						Source: "check_run", State: StateReady, Raw: strings.ToLower(conclusion),
					})
				case "":
					return nil, contractError("/graphql", "a completed check run carries no conclusion")
				default:
					entries[node.Name] = append(entries[node.Name], rollupEntry{
						Source: "check_run", State: StateFailed, Raw: strings.ToLower(conclusion),
					})
				}
			case "QUEUED", "IN_PROGRESS", "WAITING", "REQUESTED", "PENDING":
				entries[node.Name] = append(entries[node.Name], rollupEntry{
					Source: "check_run", State: StatePending, Raw: strings.ToLower(status),
				})
			case "":
				return nil, contractError("/graphql", "a check run carries no status")
			default:
				// An unrecognised status is never treated as a pass.
				entries[node.Name] = append(entries[node.Name], rollupEntry{
					Source: "check_run", State: StatePending, Raw: strings.ToLower(status),
				})
			}
		case "StatusContext":
			if node.Context == "" {
				return nil, contractError("/graphql", "a commit status carries no context")
			}
			state := strings.ToUpper(node.State)
			raw := strings.ToLower(state)
			switch state {
			case "SUCCESS":
				entries[node.Context] = append(entries[node.Context], rollupEntry{
					Source: "commit_status", State: StateReady, Raw: raw,
				})
			case "PENDING", "EXPECTED":
				entries[node.Context] = append(entries[node.Context], rollupEntry{
					Source: "commit_status", State: StatePending, Raw: raw,
				})
			case "ERROR", "FAILURE":
				entries[node.Context] = append(entries[node.Context], rollupEntry{
					Source: "commit_status", State: StateFailed, Raw: raw,
				})
			case "":
				return nil, contractError("/graphql", "a commit status carries no state")
			default:
				entries[node.Context] = append(entries[node.Context], rollupEntry{
					Source: "commit_status", State: StateFailed, Raw: raw,
				})
			}
		default:
			return nil, contractError("/graphql", "the status check rollup carries an unknown context kind")
		}
	}
	return entries, nil
}

func splitRepository(repository string) (string, string, error) {
	parts := strings.Split(strings.TrimSpace(repository), "/")
	if len(parts) != 2 || !repoNameRe.MatchString(parts[0]) || !repoNameRe.MatchString(parts[1]) {
		return "", "", fmt.Errorf("%w: %q is not an OWNER/REPO repository", ErrInvalidRequirement, repository)
	}
	return strings.ToLower(parts[0]), strings.ToLower(parts[1]), nil
}

func contractError(endpoint, detail string) *github.Error {
	return &github.Error{
		Kind:     github.KindContract,
		Endpoint: github.RedactEndpoint(endpoint),
		Category: github.CategoryGraphQL,
		Detail:   detail,
	}
}

// kindOf renders a failure kind for an item detail. Only the kind is used: the
// message of a transport error can carry the URL, which must never reach a
// report.
func kindOf(err error) string {
	if kind, ok := github.KindOf(err); ok {
		return string(kind)
	}
	return string(github.KindUnknown)
}

func uniqueSorted(values []string) []string {
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for key := range set {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}
