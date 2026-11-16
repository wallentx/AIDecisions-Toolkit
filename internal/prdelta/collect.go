package prdelta

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"aid/internal/evidence"
	"aid/internal/github"
)

const perPage = 100

// Collect reads the pull request, resolves the reviewer and baseline, and
// gathers the delta evidence. It never mutates GitHub. Failures are recorded
// per source and leave Complete false.
func Collect(ctx context.Context, api API, ref github.PullRequest, opts Options) Result {
	opts = normalizeOptions(opts)
	c := &collector{ctx: ctx, api: api, opts: opts, ref: ref, reserved: 1, result: Result{
		SchemaVersion: SchemaVersion,
		Reference:     ref,
		StartedAt:     time.Now().UTC(),
		HeadRelation:  RelationUnknown,
		Threads:       Threads{Items: []Thread{}},
		CI:            CI{NonSuccess: []CIRecord{}},
		Sources:       make(map[string]evidence.Source),
		Warnings:      []string{},
	}}
	start, ok := c.readPullRequest(false)
	if !ok {
		c.result.NewCommitsNote = "pull request metadata was unavailable"
		c.finish()
		return c.result
	}
	c.result.PR = start.info()
	c.result.CI.HeadSHA = start.Head.SHA
	c.result.HeadStable.Start = start.Head.SHA

	reviewer := c.resolveReviewer()
	baseline := c.resolveBaseline(start, reviewer)
	c.collectStack(start)
	if baseline != nil {
		c.result.Baseline = baseline
		c.collectDelta(start, baseline.Commit)
	} else {
		c.warn("baseline_missing")
		c.result.NewCommitsNote = "no baseline: the reviewer has no submitted review and --since-commit was not given"
	}
	c.collectThreads(reviewer)
	c.collectCI(start.Head.SHA)

	c.checkBaseStable()
	end, ok := c.readPullRequest(true)
	if ok {
		c.result.HeadStable.End = end.Head.SHA
		c.result.HeadStable.Stable = end.Head.SHA == start.Head.SHA
		if !c.result.HeadStable.Stable {
			c.warn("head_moved")
		}
		if end.Base.Ref != start.Base.Ref {
			c.warn("base_ref_changed")
			c.fail(SourcePullRequest, "snapshot_changed", "pull request base changed during collection")
		}
		if c.source(SourcePullRequest).Error == nil {
			c.complete(SourcePullRequest)
		}
	}
	c.finish()
	return c.result
}

func normalizeOptions(opts Options) Options {
	defaults := DefaultOptions()
	if opts.MaxPages <= 0 {
		opts.MaxPages = defaults.MaxPages
	}
	if opts.MaxRequests <= 0 {
		opts.MaxRequests = defaults.MaxRequests
	}
	if opts.MaxCommitPatches < 0 {
		opts.MaxCommitPatches = 0
	} else if opts.MaxCommitPatches == 0 {
		opts.MaxCommitPatches = defaults.MaxCommitPatches
	}
	if opts.MaxHunksPerFile <= 0 {
		opts.MaxHunksPerFile = defaults.MaxHunksPerFile
	}
	if opts.MaxNames <= 0 {
		opts.MaxNames = defaults.MaxNames
	}
	if opts.TextLimit < 0 {
		opts.TextLimit = 0
	}
	return opts
}

type collector struct {
	ctx      context.Context
	api      API
	opts     Options
	ref      github.PullRequest
	result   Result
	requests int
	// reserved counts requests held back for the end-of-run stability reads:
	// the pull request, plus the base branch when the base was resolved from one.
	reserved int
	// baseBranch and baseSHA record the branch the compare base was resolved
	// from and the commit it resolved to, for the end-of-run base check.
	baseBranch string
	baseSHA    string
}

// ---- pull request metadata ----

type restRepo struct {
	FullName      string `json:"full_name"`
	DefaultBranch string `json:"default_branch"`
}

type restBranch struct {
	Ref  string    `json:"ref"`
	SHA  string    `json:"sha"`
	Repo *restRepo `json:"repo"`
}

type restPull struct {
	Number         int        `json:"number"`
	User           *restUser  `json:"user"`
	State          string     `json:"state"`
	Merged         bool       `json:"merged"`
	MergedAt       *string    `json:"merged_at"`
	MergeableState string     `json:"mergeable_state"`
	Head           restBranch `json:"head"`
	Base           restBranch `json:"base"`
}

func (pull restPull) stateName() string {
	switch {
	case pull.Merged || pull.MergedAt != nil:
		return "merged"
	case pull.State == "open":
		return "open"
	default:
		return "closed"
	}
}

func (pull restPull) info() PRInfo {
	info := PRInfo{Number: pull.Number, State: pull.stateName(), BaseRef: pull.Base.Ref, BaseSHA: pull.Base.SHA,
		HeadRef: pull.Head.Ref, HeadSHA: pull.Head.SHA, MergeState: strings.ToLower(pull.MergeableState)}
	if pull.Base.Repo != nil {
		info.DefaultBranch = pull.Base.Repo.DefaultBranch
	}
	if pull.User != nil {
		info.Author = pull.User.Login
	}
	return info
}

func (c *collector) readPullRequest(final bool) (restPull, bool) {
	var pull restPull
	if !c.rest(SourcePullRequest, c.repoEndpoint()+"/pulls/"+strconv.Itoa(c.ref.Number), &pull, final) {
		return restPull{}, false
	}
	if pull.Number != c.ref.Number || !isSHA(pull.Head.SHA) || pull.Base.Ref == "" || pull.Head.Ref == "" ||
		pull.State == "" || pull.Base.Repo == nil || !strings.EqualFold(pull.Base.Repo.FullName, c.ref.Owner+"/"+c.ref.Name) {
		c.fail(SourcePullRequest, "invalid_response", "pull request metadata omitted identity fields")
		return restPull{}, false
	}
	c.addItems(SourcePullRequest, 1)
	return pull, true
}

// ---- reviewer and baseline ----

func (c *collector) resolveReviewer() string {
	if c.opts.Reviewer != "" {
		c.result.Reviewer = c.opts.Reviewer
		c.result.ReviewerSource = "flag"
		return c.opts.Reviewer
	}
	c.result.ReviewerSource = "authenticated_user"
	var user struct {
		Login string `json:"login"`
	}
	if !c.rest(SourceReviewer, "user", &user, false) {
		return ""
	}
	if !ValidLogin(user.Login) {
		c.fail(SourceReviewer, "invalid_response", "authenticated user response omitted a valid login")
		return ""
	}
	c.result.Reviewer = user.Login
	c.addItems(SourceReviewer, 1)
	c.complete(SourceReviewer)
	return user.Login
}

type restReview struct {
	ID          int64      `json:"id"`
	User        *restUser  `json:"user"`
	State       string     `json:"state"`
	CommitID    string     `json:"commit_id"`
	SubmittedAt *time.Time `json:"submitted_at"`
}

type restUser struct {
	Login string `json:"login"`
}

func (c *collector) resolveBaseline(pull restPull, reviewer string) *Baseline {
	key := SourceBaseline
	if c.opts.SinceCommit != "" {
		commit := strings.ToLower(c.opts.SinceCommit)
		if len(commit) != 40 {
			var resolved struct {
				SHA string `json:"sha"`
			}
			if !c.rest(key, c.repoEndpoint()+"/commits/"+commit, &resolved, false) {
				return nil
			}
			if !isSHA(resolved.SHA) || !strings.HasPrefix(resolved.SHA, commit) {
				c.fail(key, "invalid_response", "commit lookup did not return the requested commit")
				return nil
			}
			commit = resolved.SHA
		}
		c.addItems(key, 1)
		c.complete(key)
		return &Baseline{Source: "flag", Commit: commit}
	}
	if reviewer == "" {
		c.fail(key, "dependency_failed", "reviewer login was unavailable")
		return nil
	}
	var latest *restReview
	for page := 1; ; page++ {
		var reviews []restReview
		endpoint := c.repoEndpoint() + "/pulls/" + strconv.Itoa(pull.Number) + "/reviews?" + pageQuery(page)
		if !c.rest(key, endpoint, &reviews, false) {
			return nil
		}
		c.addItems(key, len(reviews))
		for index := range reviews {
			review := reviews[index]
			if review.User == nil || !sameLogin(review.User.Login, reviewer) || review.State == "" ||
				strings.EqualFold(review.State, "PENDING") || review.SubmittedAt == nil {
				continue
			}
			if latest == nil || review.SubmittedAt.After(*latest.SubmittedAt) ||
				(review.SubmittedAt.Equal(*latest.SubmittedAt) && review.ID > latest.ID) {
				latest = &review
			}
		}
		if len(reviews) < perPage {
			break
		}
		if page >= c.opts.MaxPages {
			c.fail(key, "page_limit", "review page limit reached; the latest review may be missing")
			return nil
		}
	}
	c.complete(key)
	if latest == nil {
		return nil
	}
	if !isSHA(latest.CommitID) {
		c.warn("baseline_commit_missing")
		return nil
	}
	id := latest.ID
	return &Baseline{Source: "review", ReviewID: &id, ReviewState: strings.ToUpper(latest.State),
		Commit: strings.ToLower(latest.CommitID), SubmittedAt: latest.SubmittedAt}
}

// ---- stack ----

// stackQuery looks up the base branch and the pull requests whose head is that
// branch. GraphQL takes the branch name as a variable, so names that are not
// valid REST path segments (for example "+" or "#") need no path escaping.
const stackQuery = `query($owner: String!, $name: String!, $qualified: String!, $branch: String!) {
  repository(owner: $owner, name: $name) {
    ref(qualifiedName: $qualified) { name }
    pullRequests(headRefName: $branch, states: [OPEN, CLOSED, MERGED], first: 100, orderBy: {field: CREATED_AT, direction: DESC}) {
      totalCount
      nodes { number state headRepository { nameWithOwner } }
    }
  }
}`

type gqlStackPull struct {
	Number         int    `json:"number"`
	State          string `json:"state"`
	HeadRepository *struct {
		NameWithOwner string `json:"nameWithOwner"`
	} `json:"headRepository"`
}

type gqlStackData struct {
	Repository *struct {
		Ref *struct {
			Name string `json:"name"`
		} `json:"ref"`
		PullRequests *struct {
			TotalCount int             `json:"totalCount"`
			Nodes      []*gqlStackPull `json:"nodes"`
		} `json:"pullRequests"`
	} `json:"repository"`
}

func (c *collector) collectStack(pull restPull) {
	key := SourceStack
	info := c.result.PR
	stack := &c.result.Stack
	stack.BaseIsDefaultBranch = info.DefaultBranch != "" && info.BaseRef == info.DefaultBranch
	if stack.BaseIsDefaultBranch {
		exists := true
		stack.BaseBranchExists = &exists
		c.complete(key)
		return
	}
	var data gqlStackData
	variables := map[string]any{"owner": c.ref.Owner, "name": c.ref.Name,
		"qualified": "refs/heads/" + info.BaseRef, "branch": info.BaseRef}
	if !c.graphQL(key, stackQuery, variables, &data, false) {
		return
	}
	if data.Repository == nil || data.Repository.PullRequests == nil {
		c.fail(key, "invalid_response", "stack response omitted the repository or its pull requests")
		return
	}
	exists := data.Repository.Ref != nil
	stack.BaseBranchExists = &exists
	if !exists {
		c.warn("base_branch_missing")
	}
	pulls := data.Repository.PullRequests
	c.addItems(key, len(pulls.Nodes))
	var chosen *gqlStackPull
	for _, candidate := range pulls.Nodes {
		if candidate == nil || candidate.HeadRepository == nil ||
			!strings.EqualFold(candidate.HeadRepository.NameWithOwner, c.ref.Owner+"/"+c.ref.Name) || candidate.Number == pull.Number {
			continue
		}
		open := candidate.State == "OPEN"
		if chosen == nil || open != (chosen.State == "OPEN") && open ||
			open == (chosen.State == "OPEN") && candidate.Number > chosen.Number {
			chosen = candidate
		}
	}
	if chosen != nil {
		merged := chosen.State == "MERGED"
		stack.BasePR = &BasePR{Number: chosen.Number, State: strings.ToLower(chosen.State), Merged: merged}
	}
	if pulls.TotalCount > len(pulls.Nodes) {
		c.addLimitation(key, "base pull request search matched more than 100 pull requests; older matches were not read")
	}
	c.complete(key)
}

// ---- review threads ----

const reviewThreadsQuery = `query($owner: String!, $name: String!, $number: Int!, $cursor: String) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      reviewDecision
      reviewThreads(first: 100, after: $cursor) {
        pageInfo { hasNextPage endCursor }
        nodes {
          id
          isResolved
          isOutdated
          path
          line
          comments(first: 100) {
            pageInfo { hasNextPage endCursor }
            nodes { fullDatabaseId author { login } body createdAt }
          }
        }
      }
    }
  }
}`

const threadCommentsQuery = `query($id: ID!, $cursor: String) {
  node(id: $id) {
    ... on PullRequestReviewThread {
      comments(first: 100, after: $cursor) {
        pageInfo { hasNextPage endCursor }
        nodes { fullDatabaseId author { login } body createdAt }
      }
    }
  }
}`

type gqlPageInfo struct {
	HasNextPage bool    `json:"hasNextPage"`
	EndCursor   *string `json:"endCursor"`
}

type gqlComment struct {
	FullDatabaseID json.RawMessage `json:"fullDatabaseId"`
	Author         *restUser       `json:"author"`
	Body           string          `json:"body"`
}

type gqlComments struct {
	PageInfo gqlPageInfo   `json:"pageInfo"`
	Nodes    []*gqlComment `json:"nodes"`
}

type gqlThread struct {
	ID         string       `json:"id"`
	IsResolved bool         `json:"isResolved"`
	IsOutdated bool         `json:"isOutdated"`
	Path       string       `json:"path"`
	Line       *int         `json:"line"`
	Comments   *gqlComments `json:"comments"`
}

type gqlThreadsData struct {
	Repository *struct {
		PullRequest *struct {
			ReviewDecision *string `json:"reviewDecision"`
			ReviewThreads  *struct {
				PageInfo gqlPageInfo  `json:"pageInfo"`
				Nodes    []*gqlThread `json:"nodes"`
			} `json:"reviewThreads"`
		} `json:"pullRequest"`
	} `json:"repository"`
}

type gqlThreadCommentsData struct {
	Node *struct {
		Comments *gqlComments `json:"comments"`
	} `json:"node"`
}

func (c *collector) collectThreads(reviewer string) {
	key := SourceThreads
	if reviewer == "" {
		c.fail(key, "dependency_failed", "reviewer login was unavailable")
		return
	}
	author := c.result.PR.Author
	if author == "" {
		c.addLimitation(key, "pull request author login was unavailable; author_replied_after is false")
	}
	var cursor any
	seen := map[string]bool{}
	for page := 1; ; page++ {
		var data gqlThreadsData
		variables := map[string]any{"owner": c.ref.Owner, "name": c.ref.Name, "number": c.ref.Number, "cursor": cursor}
		if !c.graphQL(key, reviewThreadsQuery, variables, &data, false) {
			return
		}
		if data.Repository == nil || data.Repository.PullRequest == nil || data.Repository.PullRequest.ReviewThreads == nil {
			c.fail(key, "invalid_response", "review thread response omitted the pull request")
			return
		}
		pr := data.Repository.PullRequest
		if pr.ReviewDecision != nil {
			c.result.PR.ReviewDecision = *pr.ReviewDecision
		}
		for _, node := range pr.ReviewThreads.Nodes {
			if node == nil || node.ID == "" || node.Comments == nil {
				c.fail(key, "invalid_response", "review thread omitted identity or comments")
				return
			}
			if seen[node.ID] {
				c.fail(key, "duplicate_item", "review thread response repeated a thread")
				return
			}
			seen[node.ID] = true
			c.addItems(key, 1)
			comments := node.Comments.Nodes
			if len(comments) == 0 || comments[0] == nil || comments[0].Author == nil ||
				!sameLogin(comments[0].Author.Login, reviewer) {
				continue
			}
			if node.Comments.PageInfo.HasNextPage {
				more, ok := c.threadComments(node.ID, node.Comments.PageInfo.EndCursor)
				if !ok {
					return
				}
				comments = append(comments, more...)
			}
			c.addThread(node, comments, reviewer, author)
		}
		info := pr.ReviewThreads.PageInfo
		if !info.HasNextPage {
			break
		}
		if info.EndCursor == nil || page >= c.opts.MaxPages {
			c.fail(key, "page_limit", "review thread page limit reached")
			return
		}
		cursor = *info.EndCursor
	}
	c.complete(key)
}

func (c *collector) threadComments(id string, cursor *string) ([]*gqlComment, bool) {
	var comments []*gqlComment
	for page := 1; cursor != nil; page++ {
		if page > c.opts.MaxPages {
			c.fail(SourceThreads, "page_limit", "review thread comment page limit reached")
			return nil, false
		}
		var data gqlThreadCommentsData
		if !c.graphQL(SourceThreads, threadCommentsQuery, map[string]any{"id": id, "cursor": *cursor}, &data, false) {
			return nil, false
		}
		if data.Node == nil || data.Node.Comments == nil {
			c.fail(SourceThreads, "invalid_response", "review thread comment response omitted the thread")
			return nil, false
		}
		comments = append(comments, data.Node.Comments.Nodes...)
		cursor = nil
		if data.Node.Comments.PageInfo.HasNextPage {
			cursor = data.Node.Comments.PageInfo.EndCursor
			if cursor == nil {
				c.fail(SourceThreads, "invalid_response", "review thread comment page omitted its cursor")
				return nil, false
			}
		}
	}
	return comments, true
}

// addThread records one reviewer thread. Only a comment by the pull request
// author after the reviewer's last comment sets AuthorRepliedAfter; the latest
// comment after it from anyone is kept as the last reply.
func (c *collector) addThread(node *gqlThread, comments []*gqlComment, reviewer, author string) {
	thread := Thread{ThreadID: node.ID, FirstCommentID: bigInt(comments[0].FullDatabaseID), Path: node.Path,
		Line: node.Line, IsResolved: node.IsResolved, IsOutdated: node.IsOutdated}
	lastReviewer := -1
	for index, comment := range comments {
		if comment != nil && comment.Author != nil && sameLogin(comment.Author.Login, reviewer) {
			lastReviewer = index
		}
	}
	for _, comment := range comments[lastReviewer+1:] {
		if comment == nil {
			continue
		}
		login := ""
		if comment.Author != nil {
			login = comment.Author.Login
		}
		if author != "" && login != "" && sameLogin(login, author) {
			thread.AuthorRepliedAfter = true
		}
		thread.LastReplyBy = login
		thread.LastReplyExcerpt, thread.LastReplyClipped = excerpt(comment.Body, c.opts.TextLimit)
	}
	threads := &c.result.Threads
	threads.Items = append(threads.Items, thread)
	threads.Total++
	if !thread.IsResolved {
		threads.Unresolved++
		if thread.AuthorRepliedAfter {
			threads.UnresolvedWithAuthorReply++
		}
	}
}

func excerpt(body string, limit int) (string, bool) {
	body = strings.Join(strings.Fields(body), " ")
	if utf8.RuneCountInString(body) <= limit {
		return body, false
	}
	return string([]rune(body)[:limit]), true
}

func bigInt(raw json.RawMessage) int64 {
	text := strings.Trim(string(raw), `"`)
	value, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return 0
	}
	return value
}

// ---- CI ----

type restCheckRuns struct {
	TotalCount *int `json:"total_count"`
	CheckRuns  []struct {
		Name       string `json:"name"`
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
	} `json:"check_runs"`
}

type restCombinedStatus struct {
	TotalCount *int `json:"total_count"`
	Statuses   []struct {
		Context string `json:"context"`
		State   string `json:"state"`
	} `json:"statuses"`
}

func (c *collector) collectCI(sha string) {
	var records []CIRecord
	checksDone := c.collectChecks(sha, &records)
	statusesDone := c.collectStatuses(sha, &records)
	if checksDone {
		c.complete(SourceChecks)
	}
	if statusesDone {
		c.complete(SourceStatuses)
	}
	sort.SliceStable(records, func(i, j int) bool {
		if rank(records[i].Conclusion) != rank(records[j].Conclusion) {
			return rank(records[i].Conclusion) < rank(records[j].Conclusion)
		}
		return records[i].Name < records[j].Name
	})
	if len(records) > c.opts.MaxNames {
		c.result.CI.NonSuccessOmitted = len(records) - c.opts.MaxNames
		records = records[:c.opts.MaxNames]
	}
	c.result.CI.NonSuccess = append(c.result.CI.NonSuccess, records...)
}

func rank(conclusion string) int {
	switch conclusion {
	case "failure", "error", "timed_out", "startup_failure":
		return 0
	case "pending", "queued", "in_progress", "waiting", "requested", "expected":
		return 1
	case "action_required", "cancelled", "stale":
		return 2
	default:
		return 3
	}
}

func (c *collector) collectChecks(sha string, records *[]CIRecord) bool {
	key := SourceChecks
	base := c.repoEndpoint() + "/commits/" + sha + "/check-runs?filter=latest&"
	seen := 0
	for page := 1; ; page++ {
		var response restCheckRuns
		if !c.rest(key, base+pageQuery(page), &response, false) {
			return false
		}
		if response.TotalCount == nil {
			c.fail(key, "invalid_response", "check run response omitted total_count")
			return false
		}
		for _, run := range response.CheckRuns {
			seen++
			conclusion := run.Conclusion
			bucket := checkBucket(run.Status, run.Conclusion)
			if run.Status != "completed" {
				conclusion = run.Status
			}
			c.count(bucket)
			if bucket != "success" {
				*records = append(*records, CIRecord{Name: run.Name, Kind: "check_run", Conclusion: conclusion})
			}
		}
		c.addItems(key, len(response.CheckRuns))
		if seen >= *response.TotalCount || len(response.CheckRuns) < perPage {
			if seen != *response.TotalCount {
				c.fail(key, "incomplete_response", "check run count did not match the API total")
				return false
			}
			return true
		}
		if page >= c.opts.MaxPages {
			c.fail(key, "page_limit", "check run page limit reached")
			return false
		}
	}
}

func (c *collector) collectStatuses(sha string, records *[]CIRecord) bool {
	key := SourceStatuses
	base := c.repoEndpoint() + "/commits/" + sha + "/status?"
	seen := 0
	for page := 1; ; page++ {
		var response restCombinedStatus
		if !c.rest(key, base+pageQuery(page), &response, false) {
			return false
		}
		if response.TotalCount == nil {
			c.fail(key, "invalid_response", "combined status response omitted total_count")
			return false
		}
		for _, status := range response.Statuses {
			seen++
			bucket := statusBucket(status.State)
			c.count(bucket)
			if bucket != "success" {
				*records = append(*records, CIRecord{Name: status.Context, Kind: "status", Conclusion: status.State})
			}
		}
		c.addItems(key, len(response.Statuses))
		if seen >= *response.TotalCount || len(response.Statuses) < perPage {
			if seen != *response.TotalCount {
				c.fail(key, "incomplete_response", "status count did not match the API total")
				return false
			}
			return true
		}
		if page >= c.opts.MaxPages {
			c.fail(key, "page_limit", "status page limit reached")
			return false
		}
	}
}

func checkBucket(status, conclusion string) string {
	if status != "completed" {
		return "pending"
	}
	switch conclusion {
	case "success", "failure", "skipped", "neutral", "cancelled":
		return conclusion
	default:
		return "other"
	}
}

func statusBucket(state string) string {
	switch state {
	case "success", "pending":
		return state
	case "failure", "error":
		return "failure"
	default:
		return "other"
	}
}

func (c *collector) count(bucket string) {
	counts := &c.result.CI.Counts
	switch bucket {
	case "success":
		counts.Success++
	case "failure":
		counts.Failure++
	case "pending":
		counts.Pending++
	case "skipped":
		counts.Skipped++
	case "neutral":
		counts.Neutral++
	case "cancelled":
		counts.Cancelled++
	default:
		counts.Other++
	}
}

// ---- completion ----

func (c *collector) finish() {
	c.result.FinishedAt = time.Now().UTC()
	complete := c.result.Baseline != nil && c.result.HeadStable.Stable &&
		c.result.HeadRelation != RelationUnknown &&
		c.result.PRDiff != nil && c.result.PRDiff.Verdict != VerdictUnknown
	for _, source := range c.result.Sources {
		complete = complete && source.Complete
	}
	c.result.Complete = complete
}

func (c *collector) warn(code string) {
	c.result.Warnings = appendUnique(c.result.Warnings, code)
}

// ---- transport helpers ----

func (c *collector) repoEndpoint() string {
	return "repos/" + c.ref.Owner + "/" + c.ref.Name
}

func pageQuery(page int) string {
	return "per_page=" + strconv.Itoa(perPage) + "&page=" + strconv.Itoa(page)
}

func (c *collector) rest(key, endpoint string, out any, final bool) bool {
	if !c.takeRequest(key, final) {
		return false
	}
	err := c.api.REST(c.ctx, endpoint, out)
	c.touch(key)
	if err != nil {
		c.failWithError(key, err)
		return false
	}
	return true
}

// restErr returns the transport error without recording it, so a caller can
// treat an expected not-found response as evidence.
func (c *collector) restErr(key, endpoint string, out any) error {
	if !c.takeRequest(key, false) {
		return errRequestLimit
	}
	err := c.api.REST(c.ctx, endpoint, out)
	c.touch(key)
	return err
}

func (c *collector) graphQL(key, query string, variables map[string]any, out any, final bool) bool {
	if !c.takeRequest(key, final) {
		return false
	}
	err := c.api.GraphQL(c.ctx, query, variables, out)
	c.touch(key)
	if err != nil {
		c.failWithError(key, err)
		return false
	}
	return true
}

var errRequestLimit = errors.New("request limit reached")

// takeRequest holds back c.reserved requests for the final stability reads.
func (c *collector) takeRequest(key string, final bool) bool {
	if c.ctx.Err() != nil {
		c.fail(key, "canceled", "collection context was canceled")
		return false
	}
	limit := c.opts.MaxRequests
	if !final {
		limit -= c.reserved
	}
	if c.requests >= limit {
		c.fail(key, "request_limit", "API request limit reached")
		return false
	}
	c.requests++
	source := c.source(key)
	source.Requests++
	c.result.Sources[key] = source
	return true
}

func (c *collector) source(key string) evidence.Source {
	return c.result.Sources[key]
}

func (c *collector) touch(key string) {
	source := c.source(key)
	source.CapturedAt = time.Now().UTC().Format(time.RFC3339Nano)
	c.result.Sources[key] = source
}

func (c *collector) addItems(key string, count int) {
	source := c.source(key)
	source.Items += count
	c.result.Sources[key] = source
}

func (c *collector) complete(key string) {
	source := c.source(key)
	if source.CapturedAt == "" {
		source.CapturedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	source.Complete = source.Error == nil
	c.result.Sources[key] = source
}

func (c *collector) fail(key, code, message string) {
	source := c.source(key)
	if source.CapturedAt == "" {
		source.CapturedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	source.Complete = false
	if source.Error == nil {
		source.Error = &evidence.Error{Code: code, Message: message}
	} else if source.Error.Code != code {
		source.Limitations = appendUnique(source.Limitations, code+": "+message)
	}
	c.result.Sources[key] = source
}

func (c *collector) addLimitation(key, limitation string) {
	source := c.source(key)
	source.Limitations = appendUnique(source.Limitations, limitation)
	c.result.Sources[key] = source
}

func (c *collector) failWithError(key string, err error) {
	safe := sourceError(err)
	c.fail(key, safe.Code, safe.Message)
}

func sourceError(err error) *evidence.Error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return &evidence.Error{Code: "canceled", Message: "collection context was canceled"}
	}
	if errors.Is(err, errRequestLimit) {
		return &evidence.Error{Code: "request_limit", Message: "API request limit reached"}
	}
	var githubErr *github.Error
	if errors.As(err, &githubErr) {
		return &evidence.Error{Code: string(githubErr.Kind), Message: githubErr.Message}
	}
	return &evidence.Error{Code: "api_error", Message: "GitHub request failed"}
}

func isKind(err error, kind github.Kind) bool {
	var githubErr *github.Error
	return errors.As(err, &githubErr) && githubErr.Kind == kind
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func isSHA(value string) bool {
	if len(value) != 40 {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') && (char < 'A' || char > 'F') {
			return false
		}
	}
	return true
}

// ValidLogin accepts GitHub user and app logins, including a "[bot]" suffix.
func ValidLogin(login string) bool {
	name := strings.TrimSuffix(login, "[bot]")
	if name == "" || len(name) > 39 || name[0] == '-' {
		return false
	}
	for _, char := range name {
		if (char < 'a' || char > 'z') && (char < 'A' || char > 'Z') && (char < '0' || char > '9') && char != '-' {
			return false
		}
	}
	return true
}

// sameLogin compares REST and GraphQL logins. GraphQL reports app logins
// without the "[bot]" suffix that REST includes.
func sameLogin(left, right string) bool {
	return strings.EqualFold(strings.TrimSuffix(left, "[bot]"), strings.TrimSuffix(right, "[bot]"))
}
