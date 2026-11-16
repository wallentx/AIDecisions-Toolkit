package reviewqueue

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"aid/internal/github"
	"aid/internal/prdelta"
)

// Options bounds collection. Zero values use DefaultOptions.
type Options struct {
	Reviewer         string
	Owners           []string
	ScopeSource      string
	Workspace        string
	Host             string
	MaxPRs           int
	NoDelta          bool
	DeltaConcurrency int
	TitleLimit       int
}

func DefaultOptions() Options {
	return Options{MaxPRs: 25, DeltaConcurrency: 4, TitleLimit: 200, Host: "github.com"}
}

const searchPageSize = 100

// searchQuery fetches the search page plus, per pull request, the requested
// reviewers and the count of the reviewer's submitted reviews. One request
// classifies a page of up to 100 pull requests.
const searchQuery = `query($q: String!, $first: Int!, $cursor: String, $reviewer: String!) {
  search(query: $q, type: ISSUE, first: $first, after: $cursor) {
    issueCount
    pageInfo { hasNextPage endCursor }
    nodes {
      __typename
      ... on PullRequest {
        number
        title
        url
        isDraft
        updatedAt
        author { login }
        repository { nameWithOwner }
        reviewRequests(first: 50) {
          totalCount
          nodes {
            requestedReviewer {
              __typename
              ... on User { login }
              ... on Team { slug }
            }
          }
        }
        reviews(author: $reviewer, first: 1, states: [APPROVED, CHANGES_REQUESTED, COMMENTED, DISMISSED]) { totalCount }
      }
    }
  }
}`

type gqlLogin struct {
	Login string `json:"login"`
}

type gqlReviewer struct {
	Typename string `json:"__typename"`
	Login    string `json:"login"`
	Slug     string `json:"slug"`
}

type gqlPull struct {
	Typename   string    `json:"__typename"`
	Number     int       `json:"number"`
	Title      string    `json:"title"`
	URL        string    `json:"url"`
	IsDraft    bool      `json:"isDraft"`
	UpdatedAt  string    `json:"updatedAt"`
	Author     *gqlLogin `json:"author"`
	Repository *struct {
		NameWithOwner string `json:"nameWithOwner"`
	} `json:"repository"`
	ReviewRequests *struct {
		TotalCount int `json:"totalCount"`
		Nodes      []*struct {
			RequestedReviewer *gqlReviewer `json:"requestedReviewer"`
		} `json:"nodes"`
	} `json:"reviewRequests"`
	Reviews *struct {
		TotalCount int `json:"totalCount"`
	} `json:"reviews"`
}

type gqlSearch struct {
	Search *struct {
		IssueCount int `json:"issueCount"`
		PageInfo   struct {
			HasNextPage bool    `json:"hasNextPage"`
			EndCursor   *string `json:"endCursor"`
		} `json:"pageInfo"`
		Nodes []*gqlPull `json:"nodes"`
	} `json:"search"`
}

// BuildQuery returns the GitHub search string for a reviewer and owners.
func BuildQuery(reviewer string, owners []string) string {
	parts := []string{"is:pr", "is:open", "archived:false", "review-requested:" + reviewer}
	for _, owner := range owners {
		parts = append(parts, "user:"+owner)
	}
	return strings.Join(append(parts, "sort:updated-desc"), " ")
}

// Collect resolves the reviewer, searches for pending review requests, and
// computes a delta for each re-review. Failures are recorded per source.
func Collect(ctx context.Context, api API, opts Options) Result {
	opts = normalize(opts)
	result := Result{
		SchemaVersion:  SchemaVersion,
		Reviewer:       opts.Reviewer,
		ReviewerSource: "flag",
		Scope:          Scope{Owners: append([]string{}, opts.Owners...), Source: opts.ScopeSource, Workspace: opts.Workspace},
		AsOf:           time.Now().UTC(),
		Items:          []Item{},
		Sources:        Sources{Search: SourceStatus{Status: StatusSkipped}, Delta: DeltaSource{Status: StatusSkipped}},
		Warnings:       []string{},
	}
	if opts.ScopeSource == ScopeAll {
		result.Warnings = append(result.Warnings, "scope_all_repositories")
	}
	if opts.Reviewer == "" {
		result.ReviewerSource = "authenticated_user"
		source := &SourceStatus{Requests: 1}
		result.Sources.Reviewer = source
		var user struct {
			Login string `json:"login"`
		}
		err := api.REST(ctx, "user", &user)
		switch {
		case err != nil:
			source.Status, source.Message = safeError(err)
		case !prdelta.ValidLogin(user.Login):
			source.Status, source.Message = string(github.KindInvalidResponse), "authenticated user response omitted a valid login"
		default:
			source.Status, source.Items = StatusOK, 1
			result.Reviewer = user.Login
		}
		if source.Status != StatusOK {
			result.Sources.Search = SourceStatus{Status: StatusDependencyFailed, Message: "reviewer login was unavailable"}
			return finish(result)
		}
	}
	result.Scope.Query = BuildQuery(result.Reviewer, opts.Owners)
	pulls, more, ok := search(ctx, api, result.Scope.Query, result.Reviewer, opts.MaxPRs, &result.Sources.Search)
	if !ok {
		return finish(result)
	}
	result.Truncated = more
	if more {
		result.Warnings = append(result.Warnings, "max_prs_reached")
	}
	for _, pull := range pulls {
		result.Items = append(result.Items, toItem(pull, result.Reviewer, opts.TitleLimit))
	}
	collectDeltas(ctx, api, opts, result.Reviewer, result.Items, &result.Sources.Delta)
	return finish(result)
}

func normalize(opts Options) Options {
	defaults := DefaultOptions()
	if opts.MaxPRs <= 0 {
		opts.MaxPRs = defaults.MaxPRs
	}
	if opts.DeltaConcurrency <= 0 {
		opts.DeltaConcurrency = defaults.DeltaConcurrency
	}
	if opts.TitleLimit <= 0 {
		opts.TitleLimit = defaults.TitleLimit
	}
	if opts.Host == "" {
		opts.Host = defaults.Host
	}
	if opts.ScopeSource == "" {
		opts.ScopeSource = ScopeAll
		if len(opts.Owners) > 0 {
			opts.ScopeSource = ScopeFlag
		}
	}
	return opts
}

// search pages through results until maxPRs pull requests are held. It
// reports whether more matches existed beyond the bound.
func search(ctx context.Context, api API, query, reviewer string, maxPRs int, source *SourceStatus) ([]*gqlPull, bool, bool) {
	var pulls []*gqlPull
	seen := map[string]bool{}
	var cursor any
	// GitHub search returns at most 1,000 results; the page bound also stops
	// a response that keeps reporting more pages without pull requests.
	maxPages := (maxPRs+searchPageSize-1)/searchPageSize + 2
	for page := 1; ; page++ {
		if page > maxPages {
			source.Status, source.Message = "page_limit", "search page limit reached"
			return nil, false, false
		}
		if err := ctx.Err(); err != nil {
			source.Status, source.Message = safeError(err)
			return nil, false, false
		}
		first := min(searchPageSize, maxPRs-len(pulls))
		var data gqlSearch
		source.Requests++
		err := api.GraphQL(ctx, searchQuery, map[string]any{"q": query, "first": first, "cursor": cursor, "reviewer": reviewer}, &data)
		if err != nil {
			source.Status, source.Message = safeError(err)
			return nil, false, false
		}
		if data.Search == nil {
			source.Status, source.Message = string(github.KindInvalidResponse), "search response omitted results"
			return nil, false, false
		}
		for _, node := range data.Search.Nodes {
			if node == nil || node.Typename != "PullRequest" {
				continue
			}
			if node.Number <= 0 || node.Repository == nil || node.Repository.NameWithOwner == "" {
				source.Status, source.Message = string(github.KindInvalidResponse), "search result omitted pull request identity"
				return nil, false, false
			}
			key := strings.ToLower(node.Repository.NameWithOwner) + "#" + strconv.Itoa(node.Number)
			if seen[key] || len(pulls) >= maxPRs {
				continue
			}
			seen[key] = true
			pulls = append(pulls, node)
		}
		source.Items = len(pulls)
		info := data.Search.PageInfo
		if len(pulls) >= maxPRs {
			source.Status = StatusOK
			return pulls, info.HasNextPage || data.Search.IssueCount > len(pulls), true
		}
		if !info.HasNextPage {
			source.Status = StatusOK
			return pulls, false, true
		}
		if info.EndCursor == nil {
			source.Status, source.Message = string(github.KindInvalidResponse), "search page omitted its cursor"
			return nil, false, false
		}
		cursor = *info.EndCursor
	}
}

func toItem(pull *gqlPull, reviewer string, titleLimit int) Item {
	item := Item{Repo: pull.Repository.NameWithOwner, Number: pull.Number, URL: pull.URL, Draft: pull.IsDraft,
		UpdatedAt: pull.UpdatedAt, RequestedVia: requestedVia(pull, reviewer), Kind: KindUnknown}
	item.Title, item.TitleClipped = clip(strings.Join(strings.Fields(pull.Title), " "), titleLimit)
	if pull.Author != nil {
		item.Author = pull.Author.Login
	}
	if pull.Reviews != nil {
		item.Kind = KindNew
		if pull.Reviews.TotalCount > 0 {
			item.Kind = KindReReview
		}
	}
	return item
}

// requestedVia reports "user" for a direct request and "team:<slug>" only
// when exactly one team is requested and the full request list was read.
// Anything else is "unknown" rather than a guess about team membership.
func requestedVia(pull *gqlPull, reviewer string) string {
	requests := pull.ReviewRequests
	if requests == nil {
		return "unknown"
	}
	var teams []string
	unidentified := false
	for _, node := range requests.Nodes {
		if node == nil || node.RequestedReviewer == nil {
			unidentified = true
			continue
		}
		switch requested := node.RequestedReviewer; requested.Typename {
		case "User":
			if sameLogin(requested.Login, reviewer) {
				return "user"
			}
		case "Team":
			if requested.Slug == "" {
				unidentified = true
			} else {
				teams = append(teams, requested.Slug)
			}
		default:
			unidentified = true
		}
	}
	if !unidentified && requests.TotalCount == len(requests.Nodes) && len(teams) == 1 {
		return "team:" + teams[0]
	}
	return "unknown"
}

func collectDeltas(ctx context.Context, api API, opts Options, reviewer string, items []Item, source *DeltaSource) {
	var indexes []int
	for index, item := range items {
		if item.Kind == KindReReview {
			indexes = append(indexes, index)
		}
	}
	if opts.NoDelta || len(indexes) == 0 {
		source.Status = StatusOK
		if opts.NoDelta {
			source.Status = StatusSkipped
			source.Skipped = len(indexes)
		}
		return
	}
	jobs := make(chan int)
	var wait sync.WaitGroup
	for range min(opts.DeltaConcurrency, len(indexes)) {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for index := range jobs {
				items[index].Delta = itemDelta(ctx, api, opts.Host, reviewer, items[index])
			}
		}()
	}
	for _, index := range indexes {
		jobs <- index
	}
	close(jobs)
	wait.Wait()
	for _, index := range indexes {
		source.Computed++
		if !items[index].Delta.Complete {
			source.Incomplete++
		}
	}
	source.Status = StatusOK
	if source.Incomplete > 0 {
		source.Status = StatusIncomplete
	}
}

func itemDelta(ctx context.Context, api API, host, reviewer string, item Item) *Delta {
	ref, err := github.ParsePullRequest("", item.Repo, item.Number, host)
	if err != nil {
		return &Delta{HeadRelation: prdelta.RelationUnknown, Verdict: prdelta.VerdictUnknown,
			IncompleteSources: []string{"reference=invalid_response"}}
	}
	result := prdelta.Collect(ctx, api, ref, prdelta.Options{Reviewer: reviewer})
	delta := &Delta{HeadRelation: result.HeadRelation, Verdict: prdelta.VerdictUnknown, Complete: result.Complete,
		ThreadsUnresolved: result.Threads.Unresolved, ThreadsUnresolvedWithReply: result.Threads.UnresolvedWithReply,
		CI:       DeltaCI{Success: result.CI.Counts.Success, Failure: result.CI.Counts.Failure, Pending: result.CI.Counts.Pending},
		Warnings: result.Warnings}
	if len(delta.Warnings) == 0 {
		delta.Warnings = nil
	}
	if result.Baseline != nil {
		delta.BaselineState, delta.BaselineCommit = result.Baseline.ReviewState, result.Baseline.Commit
	}
	if diff := result.PRDiff; diff != nil {
		delta.Verdict = diff.Verdict
		for _, file := range diff.Files {
			if file.Status != prdelta.VerdictUnchanged {
				delta.ChangedFiles++
			}
		}
	}
	for key, status := range result.Sources {
		switch {
		case status.Error != nil:
			delta.IncompleteSources = append(delta.IncompleteSources, key+"="+status.Error.Code)
		case !status.Complete:
			delta.IncompleteSources = append(delta.IncompleteSources, key+"=incomplete")
		}
	}
	sort.Strings(delta.IncompleteSources)
	return delta
}

func finish(result Result) Result {
	summary := Summary{Total: len(result.Items)}
	for _, item := range result.Items {
		switch item.Kind {
		case KindNew:
			summary.New++
		case KindReReview:
			summary.ReReview++
			if delta := item.Delta; delta != nil && delta.Complete &&
				(delta.Verdict == prdelta.VerdictUnchanged || delta.Verdict == prdelta.VerdictMovedOnly) {
				summary.ReReviewUnchanged++
			}
		default:
			summary.Unknown++
		}
	}
	result.Summary = summary
	complete := !result.Truncated && result.Sources.Search.Status == StatusOK && summary.Unknown == 0 &&
		result.Sources.Delta.Status != StatusIncomplete
	for _, item := range result.Items {
		if item.Delta != nil && !item.Delta.Complete {
			complete = false
		}
	}
	result.Complete = complete
	return result
}

func safeError(err error) (string, string) {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "canceled", "collection context was canceled"
	}
	var githubErr *github.Error
	if errors.As(err, &githubErr) {
		return string(githubErr.Kind), githubErr.Message
	}
	return "api_error", "GitHub request failed"
}

func sameLogin(left, right string) bool {
	return strings.EqualFold(strings.TrimSuffix(left, "[bot]"), strings.TrimSuffix(right, "[bot]"))
}

func clip(text string, limit int) (string, bool) {
	if utf8.RuneCountInString(text) <= limit {
		return text, false
	}
	return string([]rune(text)[:limit]), true
}
