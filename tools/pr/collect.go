package pr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"aid/internal/evidence"
	"aid/internal/github"
)

const (
	graphQLPageSize = 100
	filesAPIMax     = 3000
)

var sourceKeys = []string{
	SourcePullRequest,
	SourceFiles,
	SourceChecks,
	SourceStatuses,
	SourceReviewThreads,
	SourceIssueComments,
	SourceReviews,
	SourceFreshness,
}

// Collect reads a pull request without mutating GitHub. It returns partial
// evidence with explicit source errors whenever a bound or request fails.
func Collect(ctx context.Context, api API, ref github.PullRequest, opts Options) Snapshot {
	opts = normalizeOptions(opts)
	now := time.Now().UTC()
	c := collector{
		ctx:  ctx,
		api:  api,
		opts: opts,
		snapshot: Snapshot{
			SchemaVersion: SchemaVersion,
			Reference:     ref,
			StartedAt:     now,
			Sources:       make(map[string]evidence.Source, len(sourceKeys)),
			Files:         []File{},
			Checks:        []CheckRun{},
			Statuses:      []CommitStatus{},
			ReviewThreads: []ReviewThread{},
			IssueComments: []Comment{},
			Reviews:       []Review{},
		},
	}
	for _, key := range sourceKeys {
		c.snapshot.Sources[key] = evidence.Source{}
	}

	start, startOK := c.readPullRequest(ref, false)
	if startOK {
		c.start = identityVersion(start)
		if c.retain(SourcePullRequest, start) {
			c.snapshot.PullRequest = start
			c.incrementItems(SourcePullRequest, 1)
		}
		c.collectPinned(ref, start)
	} else {
		for _, key := range []string{
			SourceFiles, SourceChecks, SourceStatuses, SourceReviewThreads,
			SourceIssueComments, SourceReviews,
		} {
			c.fail(key, "dependency_failed", "pull request metadata was unavailable")
		}
	}

	end, endOK := c.readPullRequest(ref, true)
	c.finishFreshness(startOK, endOK, end)
	c.finish()
	return c.snapshot
}

type collector struct {
	ctx                context.Context
	api                API
	opts               Options
	snapshot           Snapshot
	start              IdentityVersion
	requests           int
	retainedBytes      int
	totalComments      int
	retentionExhausted bool
	threadIDs          map[string]struct{}
	commentIDs         map[string]map[string]struct{}
}

func normalizeOptions(opts Options) Options {
	defaults := DefaultOptions()
	if opts.PerPage <= 0 {
		opts.PerPage = defaults.PerPage
	}
	if opts.PerPage > 100 {
		opts.PerPage = 100
	}
	if opts.MaxPages <= 0 {
		opts.MaxPages = defaults.MaxPages
	}
	if opts.MaxItems <= 0 {
		opts.MaxItems = defaults.MaxItems
	}
	if opts.MaxTotalComments <= 0 {
		opts.MaxTotalComments = defaults.MaxTotalComments
	}
	if opts.MaxRequests <= 0 {
		opts.MaxRequests = defaults.MaxRequests
	}
	if opts.MaxRetainedBytes <= 0 {
		opts.MaxRetainedBytes = defaults.MaxRetainedBytes
	}
	return opts
}

func (c *collector) collectPinned(ref github.PullRequest, metadata Metadata) {
	c.collectFiles(ref, metadata.ChangedFiles)
	targets, headCovered := repositoryScopes(ref, metadata)
	c.collectChecks(targets, metadata.Head.SHA, headCovered)
	c.collectStatuses(targets, metadata.Head.SHA, headCovered)
	c.collectReviewThreads(ref)
	c.collectIssueComments(ref)
	c.collectReviews(ref)
}

func (c *collector) readPullRequest(ref github.PullRequest, final bool) (Metadata, bool) {
	var raw restPullRequest
	endpoint := repoEndpoint(ref) + "/pulls/" + strconv.Itoa(ref.Number)
	if !c.rest(SourcePullRequest, endpoint, &raw, final) {
		return Metadata{}, false
	}
	metadata := raw.normalize()
	if metadata.Number != ref.Number || metadata.Head.SHA == "" || metadata.Base.SHA == "" ||
		metadata.Head.Ref == "" || metadata.Base.Ref == "" || metadata.Base.Repository.NameWithOwner == "" ||
		!strings.EqualFold(metadata.Base.Repository.NameWithOwner, ref.Owner+"/"+ref.Name) ||
		metadata.State == "" || metadata.UpdatedAt.IsZero() {
		c.fail(SourcePullRequest, "invalid_response", "pull request metadata omitted identity fields")
		return Metadata{}, false
	}
	return metadata, true
}

func (c *collector) finishFreshness(startOK, endOK bool, end Metadata) {
	source := c.snapshot.Sources[SourceFreshness]
	source.CapturedAt = c.snapshot.Sources[SourcePullRequest].CapturedAt
	if startOK {
		c.snapshot.Freshness.Start = c.start
		source.Items++
	}
	if endOK {
		c.snapshot.Freshness.End = identityVersion(end)
		source.Items++
		c.incrementItems(SourcePullRequest, 1)
	}
	if !startOK || !endOK {
		if source.Error == nil {
			source.Error = &evidence.Error{Code: "identity_unavailable", Message: "start and end identity reads did not both succeed"}
		}
		c.snapshot.Sources[SourceFreshness] = source
		return
	}
	pullSource := c.snapshot.Sources[SourcePullRequest]
	if pullSource.Error == nil && pullSource.Items == 2 {
		pullSource.Complete = true
		c.snapshot.Sources[SourcePullRequest] = pullSource
	}

	changed := changedIdentityFields(c.start, identityVersion(end))
	c.snapshot.Freshness.ChangedFields = changed
	if !c.retain(SourceFreshness, c.snapshot.Freshness) {
		source = c.snapshot.Sources[SourceFreshness]
		source.Items = 2
		c.snapshot.Sources[SourceFreshness] = source
		return
	}
	if len(changed) != 0 {
		source.Error = &evidence.Error{
			Code:    "snapshot_changed",
			Message: "pull request identity changed during collection",
		}
		c.snapshot.Sources[SourceFreshness] = source
		return
	}
	source.Complete = true
	c.snapshot.Sources[SourceFreshness] = source

}

func changedIdentityFields(start, end IdentityVersion) []string {
	var changed []string
	if start.HeadSHA != end.HeadSHA {
		changed = append(changed, "head_sha")
	}
	if start.HeadRef != end.HeadRef {
		changed = append(changed, "head_ref")
	}
	if start.HeadRepository != end.HeadRepository {
		changed = append(changed, "head_repository")
	}
	if start.BaseSHA != end.BaseSHA {
		changed = append(changed, "base_sha")
	}
	if start.BaseRef != end.BaseRef {
		changed = append(changed, "base_ref")
	}
	if start.BaseRepository != end.BaseRepository {
		changed = append(changed, "base_repository")
	}
	if !start.UpdatedAt.Equal(end.UpdatedAt) {
		changed = append(changed, "updated_at")
	}
	if start.State != end.State {
		changed = append(changed, "state")
	}
	if start.Draft != end.Draft {
		changed = append(changed, "draft")
	}
	if start.Merged != end.Merged {
		changed = append(changed, "merged")
	}
	return changed
}

func identityVersion(metadata Metadata) IdentityVersion {
	return IdentityVersion{
		HeadSHA:        metadata.Head.SHA,
		HeadRef:        metadata.Head.Ref,
		HeadRepository: metadata.Head.Repository,
		BaseSHA:        metadata.Base.SHA,
		BaseRef:        metadata.Base.Ref,
		BaseRepository: metadata.Base.Repository,
		UpdatedAt:      metadata.UpdatedAt,
		State:          metadata.State,
		Draft:          metadata.Draft,
		Merged:         metadata.Merged,
	}
}

func (c *collector) collectFiles(ref github.PullRequest, changedFiles int) {
	key := SourceFiles
	defer func() {
		sort.Slice(c.snapshot.Files, func(i, j int) bool {
			if c.snapshot.Files[i].Filename == c.snapshot.Files[j].Filename {
				return c.snapshot.Files[i].SHA < c.snapshot.Files[j].SHA
			}
			return c.snapshot.Files[i].Filename < c.snapshot.Files[j].Filename
		})
	}()
	if c.skipForRetention(key) {
		return
	}
	if changedFiles == 0 {
		source := c.snapshot.Sources[key]
		source.CapturedAt = c.snapshot.Sources[SourcePullRequest].CapturedAt
		c.snapshot.Sources[key] = source
		c.complete(key)
		return
	}
	base := repoEndpoint(ref) + "/pulls/" + strconv.Itoa(ref.Number) + "/files"
	seenPages := make(map[string]struct{})
	seenFiles := make(map[string]struct{})
	for page := 1; page <= c.opts.MaxPages; page++ {
		var raw []restFile
		if !c.rest(key, pagedEndpoint(base, c.opts.PerPage, page), &raw, false) {
			return
		}
		if c.repeatedRESTPage(key, rawIDs(raw, func(item restFile) string { return item.Filename }), seenPages) {
			return
		}
		for _, item := range raw {
			if item.Filename == "" {
				c.fail(key, "invalid_response", "file response omitted filename")
				return
			}
			if _, duplicate := seenFiles[item.Filename]; duplicate {
				c.fail(key, "duplicate_item", "file response repeated a filename")
				return
			}
			seenFiles[item.Filename] = struct{}{}
			if c.sourceItems(key) >= c.opts.MaxItems {
				c.fail(key, "item_limit", "file item limit reached")
				return
			}
			file := item.normalize()
			if !c.retain(key, file) {
				return
			}
			c.snapshot.Files = append(c.snapshot.Files, file)
			c.incrementItems(key, 1)
			if len(c.snapshot.Files) == filesAPIMax {
				if changedFiles > filesAPIMax {
					c.addLimitation(key, "GitHub returns at most 3,000 pull request files")
					c.fail(key, "api_limit", "pull request exceeds GitHub's 3,000-file response limit")
					return
				}
			}
		}
		if len(c.snapshot.Files) == changedFiles {
			c.complete(key)
			break
		}
		if len(raw) < c.opts.PerPage {
			c.fail(key, "incomplete_response", "file count did not match pull request metadata")
			return
		}
		if page == c.opts.MaxPages {
			c.fail(key, "page_limit", "file page limit reached")
			return
		}
	}
}

func (c *collector) collectChecks(targets []RepositoryScope, headSHA string, headCovered bool) {
	key := SourceChecks
	defer func() {
		sort.Slice(c.snapshot.Checks, func(i, j int) bool {
			if c.snapshot.Checks[i].ID == c.snapshot.Checks[j].ID {
				return c.snapshot.Checks[i].NodeID < c.snapshot.Checks[j].NodeID
			}
			return c.snapshot.Checks[i].ID < c.snapshot.Checks[j].ID
		})
	}()
	if c.skipForRetention(key) {
		return
	}
	c.addLimitation(key, "GitHub limits this endpoint to check runs from the 1,000 most recent check suites")
	index := make(map[string]int)
	allComplete := true
	for _, target := range targets {
		if !c.collectChecksTarget(target, headSHA, index) {
			allComplete = false
		}
	}
	if !headCovered {
		c.addLimitation(key, "head repository was absent or invalid; fork check coverage is unknown")
		c.fail(key, "head_repository_unavailable", "head repository could not be queried for check runs")
		return
	}
	if allComplete {
		c.complete(key)
	}
}

func (c *collector) collectChecksTarget(target RepositoryScope, headSHA string, index map[string]int) bool {
	key := SourceChecks
	base := scopeEndpoint(target) + "/commits/" + url.PathEscape(headSHA) + "/check-runs?filter=all"
	seenPages := make(map[string]struct{})
	knownTotal := -1
	observed := 0
	seenItems := make(map[string]struct{})
	for page := 1; page <= c.opts.MaxPages; page++ {
		var response restCheckRunsResponse
		if !c.rest(key, pagedEndpoint(base, c.opts.PerPage, page), &response, false) {
			return false
		}
		if response.TotalCount == nil {
			c.fail(key, "invalid_response", "check run response omitted total_count")
			return false
		}
		if knownTotal < 0 {
			knownTotal = *response.TotalCount
		} else if *response.TotalCount != knownTotal {
			c.fail(key, "inconsistent_response", "check run total changed between pages")
			return false
		}
		if c.repeatedRESTPage(key, rawIDs(response.CheckRuns, func(item restCheckRun) string { return strconv.FormatInt(item.ID, 10) }), seenPages) {
			return false
		}
		for _, item := range response.CheckRuns {
			itemKey, valid := localRecordKey(item.ID, item.NodeID)
			if !valid {
				c.fail(key, "invalid_response", "check run omitted stable identity")
				return false
			}
			if _, duplicate := seenItems[itemKey]; duplicate {
				c.fail(key, "duplicate_item", "check run response repeated an item within one repository")
				return false
			}
			seenItems[itemKey] = struct{}{}
			observed++
			if !c.addCheck(target, item, index) {
				return false
			}
		}
		if observed >= knownTotal {
			return observed == knownTotal
		}
		if len(response.CheckRuns) < c.opts.PerPage {
			c.fail(key, "incomplete_response", "check run count did not match the API total")
			return false
		}
		if page == c.opts.MaxPages {
			c.fail(key, "page_limit", "check run page limit reached")
			return false
		}
	}
	return false
}

func (c *collector) addCheck(target RepositoryScope, item restCheckRun, index map[string]int) bool {
	key := SourceChecks
	recordKey, valid := scopedRecordKey(target, item.ID, item.NodeID)
	if !valid {
		c.fail(key, "invalid_response", "check run omitted stable identity")
		return false
	}
	if existing, ok := index[recordKey]; ok {
		if !containsScope(c.snapshot.Checks[existing].ObservedIn, target) {
			if !c.retain(key, target) {
				return false
			}
			c.snapshot.Checks[existing].ObservedIn = append(c.snapshot.Checks[existing].ObservedIn, target)
			sortScopes(c.snapshot.Checks[existing].ObservedIn)
		}
		return true
	}
	if c.sourceItems(key) >= c.opts.MaxItems {
		c.fail(key, "item_limit", "check run item limit reached")
		return false
	}
	check := item.normalize()
	check.ObservedIn = []RepositoryScope{target}
	if !c.retain(key, check) {
		return false
	}
	index[recordKey] = len(c.snapshot.Checks)
	c.snapshot.Checks = append(c.snapshot.Checks, check)
	c.incrementItems(key, 1)
	return true
}

func (c *collector) collectStatuses(targets []RepositoryScope, headSHA string, headCovered bool) {
	key := SourceStatuses
	index := make(map[string]int)
	allComplete := true
	for _, target := range targets {
		if !c.collectStatusesTarget(target, headSHA, index) {
			allComplete = false
		}
	}
	if !headCovered {
		c.addLimitation(key, "head repository was absent or invalid; fork status coverage is unknown")
		c.fail(key, "head_repository_unavailable", "head repository could not be queried for commit statuses")
	} else if allComplete {
		c.complete(key)
	}
	sort.Slice(c.snapshot.Statuses, func(i, j int) bool {
		if c.snapshot.Statuses[i].Context == c.snapshot.Statuses[j].Context {
			if c.snapshot.Statuses[i].ID == c.snapshot.Statuses[j].ID {
				return c.snapshot.Statuses[i].NodeID < c.snapshot.Statuses[j].NodeID
			}
			return c.snapshot.Statuses[i].ID < c.snapshot.Statuses[j].ID
		}
		return c.snapshot.Statuses[i].Context < c.snapshot.Statuses[j].Context
	})
}

func (c *collector) collectStatusesTarget(target RepositoryScope, headSHA string, index map[string]int) bool {
	key := SourceStatuses
	if c.skipForRetention(key) {
		return false
	}
	base := scopeEndpoint(target) + "/commits/" + url.PathEscape(headSHA) + "/statuses"
	seenPages := make(map[string]struct{})
	seenItems := make(map[string]struct{})
	for page := 1; page <= c.opts.MaxPages; page++ {
		var items []restCommitStatus
		if !c.rest(key, pagedEndpoint(base, c.opts.PerPage, page), &items, false) {
			return false
		}
		if c.repeatedRESTPage(key, rawIDs(items, func(item restCommitStatus) string {
			return strconv.FormatInt(item.ID, 10)
		}), seenPages) {
			return false
		}
		for _, item := range items {
			itemKey, valid := localRecordKey(item.ID, item.NodeID)
			if !valid {
				c.fail(key, "invalid_response", "commit status omitted stable identity")
				return false
			}
			if _, duplicate := seenItems[itemKey]; duplicate {
				c.fail(key, "duplicate_item", "commit status response repeated an item within one repository")
				return false
			}
			seenItems[itemKey] = struct{}{}
			if !c.addStatus(target, item, index) {
				return false
			}
		}
		if len(items) < c.opts.PerPage {
			return true
		}
		if page == c.opts.MaxPages {
			c.fail(key, "page_limit", "commit status page limit reached")
			return false
		}
	}
	return false
}

func (c *collector) addStatus(target RepositoryScope, item restCommitStatus, index map[string]int) bool {
	key := SourceStatuses
	recordKey, valid := scopedRecordKey(target, item.ID, item.NodeID)
	if !valid {
		c.fail(key, "invalid_response", "commit status omitted stable identity")
		return false
	}
	if existing, ok := index[recordKey]; ok {
		if !containsScope(c.snapshot.Statuses[existing].ObservedIn, target) {
			if !c.retain(key, target) {
				return false
			}
			c.snapshot.Statuses[existing].ObservedIn = append(c.snapshot.Statuses[existing].ObservedIn, target)
			sortScopes(c.snapshot.Statuses[existing].ObservedIn)
		}
		return true
	}
	if c.sourceItems(key) >= c.opts.MaxItems {
		c.fail(key, "item_limit", "commit status item limit reached")
		return false
	}
	status := item.normalize()
	status.ObservedIn = []RepositoryScope{target}
	if !c.retain(key, status) {
		return false
	}
	index[recordKey] = len(c.snapshot.Statuses)
	c.snapshot.Statuses = append(c.snapshot.Statuses, status)
	c.incrementItems(key, 1)
	return true
}

func (c *collector) collectIssueComments(ref github.PullRequest) {
	key := SourceIssueComments
	base := repoEndpoint(ref) + "/issues/" + strconv.Itoa(ref.Number) + "/comments"
	collectRESTList(c, key, base, func(item restIssueComment) string {
		return strconv.FormatInt(item.ID, 10)
	}, func(item restIssueComment) bool {
		if !c.reserveComment(key) {
			return false
		}
		comment := item.normalize()
		if !c.retain(key, comment) {
			return false
		}
		c.snapshot.IssueComments = append(c.snapshot.IssueComments, comment)
		c.totalComments++
		return true
	})
	sortComments(c.snapshot.IssueComments)
}

func (c *collector) collectReviews(ref github.PullRequest) {
	key := SourceReviews
	base := repoEndpoint(ref) + "/pulls/" + strconv.Itoa(ref.Number) + "/reviews"
	collectRESTList(c, key, base, func(item restReview) string {
		return strconv.FormatInt(item.ID, 10)
	}, func(item restReview) bool {
		review := item.normalize()
		if !c.retain(key, review) {
			return false
		}
		c.snapshot.Reviews = append(c.snapshot.Reviews, review)
		return true
	})
	sort.Slice(c.snapshot.Reviews, func(i, j int) bool {
		left, right := c.snapshot.Reviews[i], c.snapshot.Reviews[j]
		if left.SubmittedAt != nil && right.SubmittedAt != nil && !left.SubmittedAt.Equal(*right.SubmittedAt) {
			return left.SubmittedAt.Before(*right.SubmittedAt)
		}
		return left.ID < right.ID
	})
}

func collectRESTList[T any](
	c *collector,
	key string,
	base string,
	id func(T) string,
	appendItem func(T) bool,
) {
	if c.skipForRetention(key) {
		return
	}
	seenPages := make(map[string]struct{})
	seenItems := make(map[string]struct{})
	for page := 1; page <= c.opts.MaxPages; page++ {
		var items []T
		if !c.rest(key, pagedEndpoint(base, c.opts.PerPage, page), &items, false) {
			return
		}
		if c.repeatedRESTPage(key, rawIDs(items, id), seenPages) {
			return
		}
		for _, item := range items {
			itemID := id(item)
			if itemID == "" {
				c.fail(key, "invalid_response", "REST list item omitted stable identity")
				return
			}
			if _, duplicate := seenItems[itemID]; duplicate {
				c.fail(key, "duplicate_item", "paginated REST response repeated an item")
				return
			}
			seenItems[itemID] = struct{}{}
			if c.sourceItems(key) >= c.opts.MaxItems {
				c.fail(key, "item_limit", "source item limit reached")
				return
			}
			if !appendItem(item) {
				return
			}
			c.incrementItems(key, 1)
		}
		if len(items) < c.opts.PerPage {
			c.complete(key)
			return
		}
		if page == c.opts.MaxPages {
			c.fail(key, "page_limit", "source page limit reached")
			return
		}
	}
}

func (c *collector) collectReviewThreads(ref github.PullRequest) {
	key := SourceReviewThreads
	defer func() {
		sort.Slice(c.snapshot.ReviewThreads, func(i, j int) bool {
			return c.snapshot.ReviewThreads[i].ID < c.snapshot.ReviewThreads[j].ID
		})
	}()
	if c.skipForRetention(key) {
		return
	}
	seenCursors := make(map[string]struct{})
	c.threadIDs = make(map[string]struct{})
	c.commentIDs = make(map[string]map[string]struct{})
	var cursor *string
	knownTotal := -1
	for page := 1; page <= c.opts.MaxPages; page++ {
		variables := map[string]any{
			"owner":    ref.Owner,
			"name":     ref.Name,
			"number":   ref.Number,
			"cursor":   cursor,
			"pageSize": c.opts.PerPage,
		}
		var data reviewThreadsData
		err := c.graphQL(key, reviewThreadsQuery, variables, &data)
		connection, ok := data.connection()
		if ok {
			if !c.processThreadNodes(key, connection.Nodes, err) {
				return
			}
			if err != nil {
				return
			}
			if connection.TotalCount == nil {
				c.fail(key, "invalid_response", "review thread response omitted totalCount")
				return
			}
			if knownTotal < 0 {
				knownTotal = *connection.TotalCount
			} else if *connection.TotalCount != knownTotal {
				c.fail(key, "inconsistent_response", "review thread total changed between pages")
				return
			}
		}
		if err != nil {
			return
		}
		if !ok {
			c.fail(key, "invalid_response", "review thread response omitted connection data")
			return
		}
		if connection.PageInfo.HasNextPage == nil {
			c.fail(key, "invalid_response", "review thread response omitted pageInfo.hasNextPage")
			return
		}
		if !*connection.PageInfo.HasNextPage {
			if c.sourceItems(key) != knownTotal {
				c.fail(key, "incomplete_response", "review thread count did not match the GraphQL total")
				return
			}
			if c.snapshot.Sources[key].Error == nil {
				c.complete(key)
			}
			break
		}
		if page == c.opts.MaxPages {
			c.fail(key, "page_limit", "review thread page limit reached")
			return
		}
		next, valid := nextCursor(key, connection.PageInfo.EndCursor, seenCursors, c)
		if !valid {
			return
		}
		cursor = &next
	}
}

func (c *collector) processThreadNodes(key string, nodes []*graphQLReviewThread, pageErr error) bool {
	for _, node := range nodes {
		if node == nil || node.ID == "" {
			c.fail(key, "invalid_response", "review thread response contained a null or unidentified node")
			return false
		}
		if _, duplicate := c.threadIDs[node.ID]; duplicate {
			c.fail(key, "duplicate_item", "review thread response repeated a node ID")
			return false
		}
		c.threadIDs[node.ID] = struct{}{}
		if c.sourceItems(key) >= c.opts.MaxItems {
			c.fail(key, "item_limit", "review thread item limit reached")
			return false
		}
		thread := ReviewThread{
			ID:           node.ID,
			DiffSide:     node.DiffSide,
			Path:         node.Path,
			Line:         node.Line,
			OriginalLine: node.OriginalLine,
			Resolved:     node.IsResolved,
			Outdated:     node.IsOutdated,
			Comments:     []Comment{},
		}
		thread.CommentsSource.Requests = 1
		thread.CommentsSource.CapturedAt = c.snapshot.Sources[key].CapturedAt
		if !c.retain(key, thread) {
			return false
		}
		c.snapshot.ReviewThreads = append(c.snapshot.ReviewThreads, thread)
		c.incrementItems(key, 1)
		index := len(c.snapshot.ReviewThreads) - 1
		if pageErr != nil {
			current := &c.snapshot.ReviewThreads[index]
			if node.Comments != nil {
				c.appendGraphQLComments(key, current, node.Comments.Nodes)
			}
			current.CommentsSource.Error = sourceError(pageErr)
		} else {
			c.collectThreadComments(key, index, node.Comments)
		}
		if c.retentionExhausted || threadLimitReached(c.snapshot.ReviewThreads[index].CommentsSource.Error) {
			return false
		}
		if len(c.snapshot.ReviewThreads[index].Comments) != 0 {
			c.snapshot.ReviewThreads[index].URL = c.snapshot.ReviewThreads[index].Comments[0].URL
		}
	}
	return true
}

func (c *collector) collectThreadComments(key string, index int, initial *graphQLCommentsConnection) {
	thread := &c.snapshot.ReviewThreads[index]
	defer func() { sortComments(thread.Comments) }()
	if initial == nil {
		thread.CommentsSource.Error = &evidence.Error{Code: "invalid_response", Message: "review thread omitted comments connection"}
		c.fail(key, "nested_incomplete", "one or more review thread comment lists were incomplete")
		return
	}
	if !c.appendGraphQLComments(key, thread, initial.Nodes) {
		return
	}
	if initial.TotalCount == nil {
		thread.CommentsSource.Error = &evidence.Error{Code: "invalid_response", Message: "review comments omitted totalCount"}
		c.fail(key, "nested_incomplete", "one or more review thread comment lists were incomplete")
		return
	}
	knownTotal := *initial.TotalCount
	if initial.PageInfo.HasNextPage == nil {
		thread.CommentsSource.Error = &evidence.Error{Code: "invalid_response", Message: "review comments omitted pageInfo.hasNextPage"}
		c.fail(key, "nested_incomplete", "one or more review thread comment lists were incomplete")
		return
	}
	if !*initial.PageInfo.HasNextPage {
		if thread.CommentsSource.Items != knownTotal {
			thread.CommentsSource.Error = &evidence.Error{Code: "incomplete_response", Message: "review comment count did not match the GraphQL total"}
			c.fail(key, "nested_incomplete", "one or more review thread comment lists were incomplete")
			return
		}
		thread.CommentsSource.Complete = true
		return
	}
	if c.opts.MaxPages < 2 {
		thread.CommentsSource.Error = &evidence.Error{Code: "page_limit", Message: "review comment page limit reached"}
		c.fail(key, "nested_incomplete", "one or more review thread comment lists were incomplete")
		return
	}
	seenCursors := make(map[string]struct{})
	next, valid := nextNestedCursor(initial.PageInfo.EndCursor, seenCursors, thread)
	if !valid {
		c.fail(key, "nested_incomplete", "one or more review thread comment lists were incomplete")
		return
	}
	cursor := &next
	for page := 2; page <= c.opts.MaxPages; page++ {
		variables := map[string]any{
			"threadID": thread.ID, "cursor": cursor, "pageSize": c.opts.PerPage,
		}
		var data reviewThreadCommentsData
		requestsBefore := c.snapshot.Sources[key].Requests
		err := c.graphQL(key, reviewThreadCommentsQuery, variables, &data)
		if c.snapshot.Sources[key].Requests > requestsBefore {
			thread.CommentsSource.Requests++
			thread.CommentsSource.CapturedAt = c.snapshot.Sources[key].CapturedAt
		}
		connection, ok := data.connection()
		if ok {
			if !c.appendGraphQLComments(key, thread, connection.Nodes) {
				return
			}
		}
		if err != nil {
			thread.CommentsSource.Error = sourceError(err)
			c.fail(key, "nested_incomplete", "one or more review thread comment lists were incomplete")
			return
		}
		if !ok || connection.PageInfo.HasNextPage == nil {
			thread.CommentsSource.Error = &evidence.Error{Code: "invalid_response", Message: "review comments response omitted connection metadata"}
			c.fail(key, "nested_incomplete", "one or more review thread comment lists were incomplete")
			return
		}
		if connection.TotalCount == nil || *connection.TotalCount != knownTotal {
			thread.CommentsSource.Error = &evidence.Error{Code: "inconsistent_response", Message: "review comment total was missing or changed between pages"}
			c.fail(key, "nested_incomplete", "one or more review thread comment lists were incomplete")
			return
		}
		if !*connection.PageInfo.HasNextPage {
			if thread.CommentsSource.Items != knownTotal {
				thread.CommentsSource.Error = &evidence.Error{Code: "incomplete_response", Message: "review comment count did not match the GraphQL total"}
				c.fail(key, "nested_incomplete", "one or more review thread comment lists were incomplete")
				return
			}
			thread.CommentsSource.Complete = true
			return
		}
		if page == c.opts.MaxPages {
			thread.CommentsSource.Error = &evidence.Error{Code: "page_limit", Message: "review comment page limit reached"}
			c.fail(key, "nested_incomplete", "one or more review thread comment lists were incomplete")
			return
		}
		next, valid := nextNestedCursor(connection.PageInfo.EndCursor, seenCursors, thread)
		if !valid {
			c.fail(key, "nested_incomplete", "one or more review thread comment lists were incomplete")
			return
		}
		cursor = &next
	}
}

func (c *collector) appendGraphQLComments(key string, thread *ReviewThread, nodes []*graphQLComment) bool {
	seen := c.commentIDs[thread.ID]
	if seen == nil {
		seen = make(map[string]struct{})
		c.commentIDs[thread.ID] = seen
	}
	for _, node := range nodes {
		if node == nil || node.NodeID == "" {
			thread.CommentsSource.Error = &evidence.Error{Code: "invalid_response", Message: "review comments contained a null or unidentified node"}
			c.fail(key, "nested_incomplete", "one or more review thread comment lists were incomplete")
			return false
		}
		if _, duplicate := seen[node.NodeID]; duplicate {
			thread.CommentsSource.Error = &evidence.Error{Code: "duplicate_item", Message: "review comments repeated a node ID"}
			c.fail(key, "nested_incomplete", "one or more review thread comment lists were incomplete")
			return false
		}
		seen[node.NodeID] = struct{}{}
		if !c.reserveComment(key) {
			thread.CommentsSource.Error = &evidence.Error{Code: "comment_limit", Message: "total comment limit reached"}
			c.fail(key, "comment_limit", "total comment limit reached")
			return false
		}
		comment := node.normalize(thread.Resolved, thread.Outdated)
		if !c.retain(key, comment) {
			thread.CommentsSource.Error = &evidence.Error{Code: "retention_limit", Message: "normalized retained payload limit reached"}
			return false
		}
		thread.Comments = append(thread.Comments, comment)
		thread.CommentsSource.Items++
		c.totalComments++
	}
	return true
}

func threadLimitReached(sourceErr *evidence.Error) bool {
	if sourceErr == nil {
		return false
	}
	switch sourceErr.Code {
	case "retention_limit", "comment_limit", "request_limit":
		return true
	default:
		return false
	}
}

func nextCursor(key string, cursor *string, seen map[string]struct{}, c *collector) (string, bool) {
	if cursor == nil || *cursor == "" {
		c.fail(key, "invalid_cursor", "paginated response omitted a required cursor")
		return "", false
	}
	if _, exists := seen[*cursor]; exists {
		c.fail(key, "repeated_cursor", "paginated response repeated a cursor")
		return "", false
	}
	seen[*cursor] = struct{}{}
	return *cursor, true
}

func nextNestedCursor(cursor *string, seen map[string]struct{}, thread *ReviewThread) (string, bool) {
	if cursor == nil || *cursor == "" {
		thread.CommentsSource.Error = &evidence.Error{Code: "invalid_cursor", Message: "review comments omitted a required cursor"}
		return "", false
	}
	if _, exists := seen[*cursor]; exists {
		thread.CommentsSource.Error = &evidence.Error{Code: "repeated_cursor", Message: "review comments repeated a cursor"}
		return "", false
	}
	seen[*cursor] = struct{}{}
	return *cursor, true
}

func (c *collector) finish() {
	now := time.Now().UTC()
	c.snapshot.FinishedAt = now
	complete := true
	for _, key := range sourceKeys {
		source := c.snapshot.Sources[key]
		c.snapshot.Sources[key] = source
		complete = complete && source.Complete
	}
	for i := range c.snapshot.ReviewThreads {
		thread := &c.snapshot.ReviewThreads[i]
		complete = complete && thread.CommentsSource.Complete
	}
	c.snapshot.Complete = complete
}

func (c *collector) rest(key, endpoint string, out any, final bool) bool {
	if err := c.ctx.Err(); err != nil {
		c.fail(key, "canceled", "collection context was canceled")
		return false
	}
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

func (c *collector) graphQL(key, query string, variables map[string]any, out any) error {
	if err := c.ctx.Err(); err != nil {
		c.fail(key, "canceled", "collection context was canceled")
		return err
	}
	if !c.takeRequest(key, false) {
		return errRequestLimit
	}
	err := c.api.GraphQL(c.ctx, query, variables, out)
	c.touch(key)
	if err != nil {
		c.failWithError(key, err)
	}
	return err
}

var errRequestLimit = errors.New("request limit reached")

func (c *collector) takeRequest(key string, final bool) bool {
	limit := c.opts.MaxRequests
	allowed := c.requests < limit
	if !final {
		allowed = c.requests < limit-1
	}
	if !allowed {
		c.fail(key, "request_limit", "API request limit reached")
		return false
	}
	c.requests++
	source := c.snapshot.Sources[key]
	source.Requests++
	c.snapshot.Sources[key] = source
	return true
}

func (c *collector) retain(key string, value any) bool {
	encoded, err := json.Marshal(value)
	if err != nil {
		c.fail(key, "normalization_failed", "normalized evidence could not be measured")
		return false
	}
	if len(encoded) > c.opts.MaxRetainedBytes-c.retainedBytes {
		c.retentionExhausted = true
		c.fail(key, "retention_limit", "normalized retained payload limit reached")
		return false
	}
	c.retainedBytes += len(encoded)
	return true
}

func (c *collector) skipForRetention(key string) bool {
	if !c.retentionExhausted {
		return false
	}
	c.fail(key, "retention_limit", "normalized retained payload limit reached")
	return true
}

func (c *collector) reserveComment(key string) bool {
	if c.totalComments < c.opts.MaxTotalComments {
		return true
	}
	c.fail(key, "comment_limit", "total comment limit reached")
	return false
}

func (c *collector) sourceItems(key string) int {
	return c.snapshot.Sources[key].Items
}

func (c *collector) incrementItems(key string, count int) {
	source := c.snapshot.Sources[key]
	source.Items += count
	c.snapshot.Sources[key] = source
}

func (c *collector) complete(key string) {
	source := c.snapshot.Sources[key]
	if source.CapturedAt == "" {
		source.CapturedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	if source.Error == nil {
		source.Complete = true
	}
	c.snapshot.Sources[key] = source
}

func (c *collector) fail(key, code, message string) {
	source := c.snapshot.Sources[key]
	if source.CapturedAt == "" {
		source.CapturedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	source.Complete = false
	if source.Error == nil {
		source.Error = &evidence.Error{Code: code, Message: message}
	} else if source.Error.Code != code {
		source.Limitations = appendUnique(source.Limitations, code+": "+message)
	}
	c.snapshot.Sources[key] = source
}

func (c *collector) touch(key string) {
	source := c.snapshot.Sources[key]
	source.CapturedAt = time.Now().UTC().Format(time.RFC3339Nano)
	c.snapshot.Sources[key] = source
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

func (c *collector) addLimitation(key, limitation string) {
	source := c.snapshot.Sources[key]
	source.Limitations = appendUnique(source.Limitations, limitation)
	c.snapshot.Sources[key] = source
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func (c *collector) repeatedRESTPage(key, signature string, seen map[string]struct{}) bool {
	if signature == "" {
		return false
	}
	if _, exists := seen[signature]; exists {
		c.fail(key, "repeated_page", "paginated REST response repeated a page")
		return true
	}
	seen[signature] = struct{}{}
	return false
}

func rawIDs[T any](items []T, id func(T) string) string {
	if len(items) == 0 {
		return ""
	}
	values := make([]string, 0, len(items))
	for _, item := range items {
		values = append(values, id(item))
	}
	return strings.Join(values, "\x00")
}

func repoEndpoint(ref github.PullRequest) string {
	return "repos/" + url.PathEscape(ref.Owner) + "/" + url.PathEscape(ref.Name)
}

func scopeEndpoint(scope RepositoryScope) string {
	return "repos/" + url.PathEscape(scope.Owner) + "/" + url.PathEscape(scope.Name)
}

func repositoryScopes(ref github.PullRequest, metadata Metadata) ([]RepositoryScope, bool) {
	base := RepositoryScope{Host: ref.Host, Owner: ref.Owner, Name: ref.Name}
	targets := []RepositoryScope{base}
	parts := strings.Split(metadata.Head.Repository.NameWithOwner, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return targets, false
	}
	head := RepositoryScope{Host: ref.Host, Owner: parts[0], Name: parts[1]}
	if !sameScope(base, head) {
		targets = append(targets, head)
	}
	return targets, true
}

func sameScope(left, right RepositoryScope) bool {
	return strings.EqualFold(left.Host, right.Host) &&
		strings.EqualFold(left.Owner, right.Owner) && strings.EqualFold(left.Name, right.Name)
}

func scopeKey(scope RepositoryScope) string {
	return strings.ToLower(scope.Host + "/" + scope.Owner + "/" + scope.Name)
}

func scopedRecordKey(scope RepositoryScope, id int64, nodeID string) (string, bool) {
	if nodeID != "" {
		return "node:" + nodeID, true
	}
	if id == 0 {
		return "", false
	}
	return "rest:" + scopeKey(scope) + ":" + strconv.FormatInt(id, 10), true
}

func localRecordKey(id int64, nodeID string) (string, bool) {
	if nodeID != "" {
		return "node:" + nodeID, true
	}
	if id == 0 {
		return "", false
	}
	return "rest:" + strconv.FormatInt(id, 10), true
}

func containsScope(scopes []RepositoryScope, target RepositoryScope) bool {
	for _, scope := range scopes {
		if sameScope(scope, target) {
			return true
		}
	}
	return false
}

func sortScopes(scopes []RepositoryScope) {
	sort.Slice(scopes, func(i, j int) bool { return scopeKey(scopes[i]) < scopeKey(scopes[j]) })
}

func pagedEndpoint(endpoint string, perPage, page int) string {
	separator := "?"
	if strings.Contains(endpoint, "?") {
		separator = "&"
	}
	return fmt.Sprintf("%s%sper_page=%d&page=%d", endpoint, separator, perPage, page)
}

func sortComments(comments []Comment) {
	sort.SliceStable(comments, func(i, j int) bool {
		if comments[i].CreatedAt.Equal(comments[j].CreatedAt) {
			// GraphQL node IDs are opaque. Preserve connection order on tied
			// timestamps so a reply cannot become the thread's root comment.
			if comments[i].ID == 0 || comments[j].ID == 0 {
				return false
			}
			return comments[i].ID < comments[j].ID
		}
		return comments[i].CreatedAt.Before(comments[j].CreatedAt)
	})
}

type restActor struct {
	Type    string `json:"type"`
	Login   string `json:"login"`
	ID      int64  `json:"id"`
	NodeID  string `json:"node_id"`
	HTMLURL string `json:"html_url"`
}

func (actor restActor) normalize() Actor {
	return Actor{Type: actor.Type, Login: actor.Login, ID: actor.ID, NodeID: actor.NodeID, URL: actor.HTMLURL}
}

type restRepository struct {
	ID       int64  `json:"id"`
	NodeID   string `json:"node_id"`
	FullName string `json:"full_name"`
	HTMLURL  string `json:"html_url"`
}

func (repository restRepository) normalize() RepositoryIdentity {
	return RepositoryIdentity{
		ID:            repository.ID,
		NodeID:        repository.NodeID,
		NameWithOwner: repository.FullName,
		URL:           repository.HTMLURL,
	}
}

type restBranch struct {
	Ref        string         `json:"ref"`
	SHA        string         `json:"sha"`
	Repository restRepository `json:"repo"`
}

func (branch restBranch) normalize() Branch {
	return Branch{Ref: branch.Ref, SHA: branch.SHA, Repository: branch.Repository.normalize()}
}

type restPullRequest struct {
	ID             int64      `json:"id"`
	NodeID         string     `json:"node_id"`
	Number         int        `json:"number"`
	HTMLURL        string     `json:"html_url"`
	Title          string     `json:"title"`
	Body           string     `json:"body"`
	State          string     `json:"state"`
	Draft          bool       `json:"draft"`
	Merged         bool       `json:"merged"`
	Mergeable      *bool      `json:"mergeable"`
	MergeableState string     `json:"mergeable_state"`
	User           restActor  `json:"user"`
	UpdatedAt      time.Time  `json:"updated_at"`
	ChangedFiles   int        `json:"changed_files"`
	Head           restBranch `json:"head"`
	Base           restBranch `json:"base"`
}

func (pull restPullRequest) normalize() Metadata {
	return Metadata{
		ID:             pull.ID,
		NodeID:         pull.NodeID,
		Number:         pull.Number,
		URL:            pull.HTMLURL,
		Title:          pull.Title,
		Body:           pull.Body,
		State:          pull.State,
		Draft:          pull.Draft,
		Merged:         pull.Merged,
		Mergeable:      pull.Mergeable,
		MergeableState: pull.MergeableState,
		Author:         pull.User.normalize(),
		UpdatedAt:      pull.UpdatedAt,
		ChangedFiles:   pull.ChangedFiles,
		Head:           pull.Head.normalize(),
		Base:           pull.Base.normalize(),
	}
}

type restFile struct {
	SHA         string `json:"sha"`
	Filename    string `json:"filename"`
	Status      string `json:"status"`
	Additions   int    `json:"additions"`
	Deletions   int    `json:"deletions"`
	Changes     int    `json:"changes"`
	BlobURL     string `json:"blob_url"`
	RawURL      string `json:"raw_url"`
	ContentsURL string `json:"contents_url"`
}

func (file restFile) normalize() File {
	return File{
		SHA: file.SHA, Filename: file.Filename, Status: file.Status,
		Additions: file.Additions, Deletions: file.Deletions, Changes: file.Changes,
		BlobURL: file.BlobURL, RawURL: file.RawURL, ContentsURL: file.ContentsURL,
	}
}

type restCheckRunsResponse struct {
	TotalCount *int           `json:"total_count"`
	CheckRuns  []restCheckRun `json:"check_runs"`
}

type restCheckRun struct {
	ID          int64           `json:"id"`
	NodeID      string          `json:"node_id"`
	Name        string          `json:"name"`
	Status      string          `json:"status"`
	Conclusion  string          `json:"conclusion"`
	HeadSHA     string          `json:"head_sha"`
	DetailsURL  string          `json:"details_url"`
	ExternalID  string          `json:"external_id"`
	StartedAt   *time.Time      `json:"started_at"`
	CompletedAt *time.Time      `json:"completed_at"`
	CheckSuite  *restCheckSuite `json:"check_suite"`
	App         *restApp        `json:"app"`
}

type restCheckSuite struct {
	ID      int64  `json:"id"`
	NodeID  string `json:"node_id"`
	HeadSHA string `json:"head_sha"`
}

type restApp struct {
	ID      int64     `json:"id"`
	NodeID  string    `json:"node_id"`
	Slug    string    `json:"slug"`
	Name    string    `json:"name"`
	Owner   restActor `json:"owner"`
	HTMLURL string    `json:"html_url"`
}

func (check restCheckRun) normalize() CheckRun {
	result := CheckRun{
		ID:          check.ID,
		NodeID:      check.NodeID,
		Name:        check.Name,
		Status:      check.Status,
		Conclusion:  check.Conclusion,
		HeadSHA:     check.HeadSHA,
		DetailsURL:  check.DetailsURL,
		ExternalID:  check.ExternalID,
		StartedAt:   check.StartedAt,
		CompletedAt: check.CompletedAt,
	}
	if check.CheckSuite != nil {
		result.CheckSuite = CheckSuite{
			ID: check.CheckSuite.ID, NodeID: check.CheckSuite.NodeID, HeadSHA: check.CheckSuite.HeadSHA,
		}
	}
	if check.App != nil {
		result.App = App{
			ID: check.App.ID, NodeID: check.App.NodeID, Slug: check.App.Slug,
			Name: check.App.Name, Owner: check.App.Owner.normalize(), HTMLURL: check.App.HTMLURL,
		}
	}
	return result
}

type restCommitStatus struct {
	ID          int64     `json:"id"`
	NodeID      string    `json:"node_id"`
	State       string    `json:"state"`
	Context     string    `json:"context"`
	Description string    `json:"description"`
	TargetURL   string    `json:"target_url"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	Creator     restActor `json:"creator"`
}

func (status restCommitStatus) normalize() CommitStatus {
	return CommitStatus{
		ID: status.ID, NodeID: status.NodeID, State: status.State, Context: status.Context,
		Description: status.Description, TargetURL: status.TargetURL, CreatedAt: status.CreatedAt,
		UpdatedAt: status.UpdatedAt, Creator: status.Creator.normalize(),
	}
}

type restIssueComment struct {
	ID                int64     `json:"id"`
	NodeID            string    `json:"node_id"`
	HTMLURL           string    `json:"html_url"`
	User              restActor `json:"user"`
	AuthorAssociation string    `json:"author_association"`
	Body              string    `json:"body"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

func (comment restIssueComment) normalize() Comment {
	return Comment{
		ID: comment.ID, NodeID: comment.NodeID, URL: comment.HTMLURL, Author: comment.User.normalize(),
		AuthorAssociation: comment.AuthorAssociation, Body: comment.Body,
		CreatedAt: comment.CreatedAt, UpdatedAt: comment.UpdatedAt,
	}
}

type restReview struct {
	ID          int64      `json:"id"`
	NodeID      string     `json:"node_id"`
	HTMLURL     string     `json:"html_url"`
	User        restActor  `json:"user"`
	Body        string     `json:"body"`
	State       string     `json:"state"`
	SubmittedAt *time.Time `json:"submitted_at"`
	CommitID    string     `json:"commit_id"`
}

func (review restReview) normalize() Review {
	return Review{
		ID: review.ID, NodeID: review.NodeID, URL: review.HTMLURL, Author: review.User.normalize(),
		Body: review.Body, State: review.State, SubmittedAt: review.SubmittedAt, CommitID: review.CommitID,
	}
}

type graphQLPageInfo struct {
	HasNextPage *bool   `json:"hasNextPage"`
	EndCursor   *string `json:"endCursor"`
}

type reviewThreadsData struct {
	Repository *struct {
		PullRequest *struct {
			ReviewThreads *graphQLReviewThreadsConnection `json:"reviewThreads"`
		} `json:"pullRequest"`
	} `json:"repository"`
}

func (data reviewThreadsData) connection() (*graphQLReviewThreadsConnection, bool) {
	if data.Repository == nil || data.Repository.PullRequest == nil || data.Repository.PullRequest.ReviewThreads == nil {
		return nil, false
	}
	return data.Repository.PullRequest.ReviewThreads, true
}

type graphQLReviewThreadsConnection struct {
	TotalCount *int                   `json:"totalCount"`
	Nodes      []*graphQLReviewThread `json:"nodes"`
	PageInfo   graphQLPageInfo        `json:"pageInfo"`
}

type graphQLReviewThread struct {
	DiffSide     string                     `json:"diffSide"`
	ID           string                     `json:"id"`
	IsResolved   bool                       `json:"isResolved"`
	IsOutdated   bool                       `json:"isOutdated"`
	Path         string                     `json:"path"`
	Line         *int                       `json:"line"`
	OriginalLine *int                       `json:"originalLine"`
	Comments     *graphQLCommentsConnection `json:"comments"`
}

type graphQLCommentsConnection struct {
	TotalCount *int              `json:"totalCount"`
	Nodes      []*graphQLComment `json:"nodes"`
	PageInfo   graphQLPageInfo   `json:"pageInfo"`
}

type reviewThreadCommentsData struct {
	Node *struct {
		Comments *graphQLCommentsConnection `json:"comments"`
	} `json:"node"`
}

func (data reviewThreadCommentsData) connection() (*graphQLCommentsConnection, bool) {
	if data.Node == nil || data.Node.Comments == nil {
		return nil, false
	}
	return data.Node.Comments, true
}

type graphQLComment struct {
	Review *struct {
		ID string `json:"id"`
	} `json:"pullRequestReview"`
	NodeID            string          `json:"id"`
	FullDatabaseID    json.RawMessage `json:"fullDatabaseId"`
	URL               string          `json:"url"`
	Author            *Actor          `json:"author"`
	AuthorAssociation string          `json:"authorAssociation"`
	Body              string          `json:"body"`
	CreatedAt         time.Time       `json:"createdAt"`
	UpdatedAt         time.Time       `json:"updatedAt"`
	Path              string          `json:"path"`
	Line              *int            `json:"line"`
	OriginalLine      *int            `json:"originalLine"`
	DiffHunk          string          `json:"diffHunk"`
	Outdated          bool            `json:"outdated"`
	Commit            *struct {
		OID string `json:"oid"`
	} `json:"commit"`
	OriginalCommit *struct {
		OID string `json:"oid"`
	} `json:"originalCommit"`
}

func (comment graphQLComment) normalize(resolved, threadOutdated bool) Comment {
	author := Actor{}
	if comment.Author != nil {
		author = *comment.Author
	}
	result := Comment{
		FullDatabaseID: normalizeBigInt(comment.FullDatabaseID), NodeID: comment.NodeID,
		URL: comment.URL, Author: author,
		AuthorAssociation: comment.AuthorAssociation, Body: comment.Body,
		CreatedAt: comment.CreatedAt, UpdatedAt: comment.UpdatedAt, Path: comment.Path,
		Line: comment.Line, OriginalLine: comment.OriginalLine, DiffHunk: comment.DiffHunk,
		Resolved: resolved, Outdated: comment.Outdated || threadOutdated,
	}
	if comment.Review != nil {
		result.ReviewNodeID = comment.Review.ID
	}
	if comment.Commit != nil {
		result.CommitID = comment.Commit.OID
	}
	if comment.OriginalCommit != nil {
		result.OriginalCommitID = comment.OriginalCommit.OID
	}
	return result
}

func normalizeBigInt(raw json.RawMessage) string {
	value := strings.TrimSpace(string(raw))
	if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
		var decoded string
		if json.Unmarshal(raw, &decoded) == nil {
			return decoded
		}
	}
	if value == "null" {
		return ""
	}
	return value
}
