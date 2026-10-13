package pr

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"aid/internal/github"
)

type fakeAPI struct {
	metadata  []restPullRequest
	pullReads int
	endpoints []string
	restHook  func(string, any) (bool, error)
	graphHook func(string, map[string]any, any) (bool, error)
}

func (fake *fakeAPI) REST(_ context.Context, endpoint string, out any) error {
	fake.endpoints = append(fake.endpoints, endpoint)
	if fake.restHook != nil {
		handled, err := fake.restHook(endpoint, out)
		if handled {
			return err
		}
	}
	switch {
	case endpoint == "repos/acme/widget/pulls/7":
		index := fake.pullReads
		if index >= len(fake.metadata) {
			index = len(fake.metadata) - 1
		}
		fake.pullReads++
		return copyJSON(out, fake.metadata[index])
	case strings.Contains(endpoint, "/check-runs?"):
		return copyJSON(out, restCheckRunsResponse{TotalCount: intPointer(0), CheckRuns: []restCheckRun{}})
	case strings.Contains(endpoint, "/statuses?"):
		return copyJSON(out, []restCommitStatus{})
	case strings.Contains(endpoint, "/issues/7/comments?"):
		return copyJSON(out, []restIssueComment{})
	case strings.Contains(endpoint, "/pulls/7/reviews?"):
		return copyJSON(out, []restReview{})
	case strings.Contains(endpoint, "/pulls/7/files?"):
		return copyJSON(out, []restFile{})
	default:
		return fmt.Errorf("unexpected REST endpoint %q", endpoint)
	}
}

func (fake *fakeAPI) GraphQL(_ context.Context, query string, variables map[string]any, out any) error {
	if fake.graphHook != nil {
		handled, err := fake.graphHook(query, variables, out)
		if handled {
			return err
		}
	}
	data := reviewThreadsData{Repository: &struct {
		PullRequest *struct {
			ReviewThreads *graphQLReviewThreadsConnection `json:"reviewThreads"`
		} `json:"pullRequest"`
	}{PullRequest: &struct {
		ReviewThreads *graphQLReviewThreadsConnection `json:"reviewThreads"`
	}{ReviewThreads: &graphQLReviewThreadsConnection{
		TotalCount: intPointer(0),
		Nodes:      []*graphQLReviewThread{},
		PageInfo:   graphQLPageInfo{HasNextPage: boolPointer(false)},
	}}}}
	return copyJSON(out, data)
}

func TestCollectPaginatesRESTAndOrdersEvidence(t *testing.T) {
	metadata := testMetadata()
	metadata.ChangedFiles = 3
	fake := &fakeAPI{metadata: []restPullRequest{metadata, metadata}}
	created := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	fake.restHook = func(endpoint string, out any) (bool, error) {
		page := endpointPage(endpoint)
		switch {
		case strings.Contains(endpoint, "/files?"):
			items := []restFile{{SHA: "2", Filename: "b.go"}, {SHA: "1", Filename: "a.go"}}
			if page == 2 {
				items = []restFile{{SHA: "3", Filename: "c.go"}}
			}
			return true, copyJSON(out, items)
		case strings.Contains(endpoint, "/statuses?"):
			items := []restCommitStatus{{ID: 2, NodeID: "S_2", Context: "z"}, {ID: 1, NodeID: "S_1", Context: "a"}}
			if page == 2 {
				items = []restCommitStatus{{ID: 3, NodeID: "S_3", Context: "a"}}
			}
			return true, copyJSON(out, items)
		case strings.Contains(endpoint, "/issues/7/comments?"):
			items := []restIssueComment{
				{ID: 2, Body: "later", CreatedAt: created.Add(time.Hour)},
				{ID: 1, Body: "first", CreatedAt: created},
			}
			if page == 2 {
				items = []restIssueComment{{ID: 3, Body: "last", CreatedAt: created.Add(2 * time.Hour)}}
			}
			return true, copyJSON(out, items)
		}
		return false, nil
	}

	got := Collect(context.Background(), fake, testReference(), Options{PerPage: 2})
	if !got.Complete {
		t.Fatalf("Collect() complete = false, sources = %+v", got.Sources)
	}
	if names := fileNames(got.Files); !reflect.DeepEqual(names, []string{"a.go", "b.go", "c.go"}) {
		t.Errorf("Collect() file names = %v, want sorted names", names)
	}
	if ids := statusIDs(got.Statuses); !reflect.DeepEqual(ids, []int64{1, 3, 2}) {
		t.Errorf("Collect() status IDs = %v, want [1 3 2]", ids)
	}
	if ids := commentIDs(got.IssueComments); !reflect.DeepEqual(ids, []int64{1, 2, 3}) {
		t.Errorf("Collect() comment IDs = %v, want [1 2 3]", ids)
	}
	for _, endpoint := range fake.endpoints {
		if (strings.Contains(endpoint, "/check-runs") || strings.Contains(endpoint, "/statuses")) && !strings.Contains(endpoint, "/old-head/") {
			t.Errorf("Collect() used an unpinned head endpoint %q", endpoint)
		}
	}
}

func TestCollectPaginatesThreadsAndNestedComments(t *testing.T) {
	metadata := testMetadata()
	fake := &fakeAPI{metadata: []restPullRequest{metadata, metadata}}
	fake.graphHook = func(query string, variables map[string]any, out any) (bool, error) {
		if got := variables["pageSize"]; got != 100 {
			t.Errorf("GraphQL pageSize = %v, want 100", got)
		}
		if strings.Contains(query, "$threadID") {
			data := reviewThreadCommentsData{Node: &struct {
				Comments *graphQLCommentsConnection `json:"comments"`
			}{Comments: &graphQLCommentsConnection{
				TotalCount: intPointer(101),
				Nodes:      []*graphQLComment{testGraphQLComment(101)},
				PageInfo:   graphQLPageInfo{HasNextPage: boolPointer(false)},
			}}}
			return true, copyJSON(out, data)
		}
		cursor := variableString(variables["cursor"])
		var nodes []*graphQLReviewThread
		pageInfo := graphQLPageInfo{HasNextPage: boolPointer(false)}
		if cursor == "" {
			for index := 1; index <= 100; index++ {
				comments := &graphQLCommentsConnection{
					TotalCount: intPointer(0), PageInfo: graphQLPageInfo{HasNextPage: boolPointer(false)},
				}
				if index == 1 {
					for comment := 1; comment <= 100; comment++ {
						comments.Nodes = append(comments.Nodes, testGraphQLComment(comment))
					}
					comments.PageInfo = graphQLPageInfo{
						HasNextPage: boolPointer(true), EndCursor: stringPointer("comment-100"),
					}
					comments.TotalCount = intPointer(101)
				}
				nodes = append(nodes, &graphQLReviewThread{
					ID: fmt.Sprintf("thread-%03d", index), Path: "main.go", Comments: comments,
				})
			}
			pageInfo = graphQLPageInfo{HasNextPage: boolPointer(true), EndCursor: stringPointer("thread-100")}
		} else {
			nodes = []*graphQLReviewThread{{
				ID: "thread-101", Path: "later.go",
				Comments: &graphQLCommentsConnection{
					TotalCount: intPointer(0), PageInfo: graphQLPageInfo{HasNextPage: boolPointer(false)},
				},
			}}
		}
		return true, setThreadData(out, nodes, pageInfo, 101)
	}

	got := Collect(context.Background(), fake, testReference(), Options{})
	if !got.Complete {
		t.Fatalf("Collect() complete = false, sources = %+v", got.Sources)
	}
	if len(got.ReviewThreads) != 101 {
		t.Fatalf("Collect() threads = %d, want 101", len(got.ReviewThreads))
	}
	first := got.ReviewThreads[0]
	if len(first.Comments) != 101 || !first.CommentsSource.Complete || first.CommentsSource.Requests != 2 {
		t.Errorf("Collect() first thread comments = %d, source = %+v; want 101 complete across 2 requests", len(first.Comments), first.CommentsSource)
	}
	foundDatabaseID := false
	for _, comment := range first.Comments {
		foundDatabaseID = foundDatabaseID || comment.FullDatabaseID == "101"
	}
	if !foundDatabaseID {
		t.Error("Collect() omitted full database ID 101")
	}
}

func TestCollectDetectsIdentityMutation(t *testing.T) {
	start := testMetadata()
	end := start
	end.Head.SHA = "new-head"
	end.Head.Ref = "rewritten"
	end.Head.Repository.FullName = "other/widget"
	end.Base.SHA = "new-base"
	end.UpdatedAt = end.UpdatedAt.Add(time.Minute)
	end.State = "closed"
	end.Draft = true
	end.Merged = true
	fake := &fakeAPI{metadata: []restPullRequest{start, end}}

	got := Collect(context.Background(), fake, testReference(), Options{})
	if got.Complete {
		t.Fatal("Collect() complete = true after identity mutation, want false")
	}
	want := []string{"head_sha", "head_ref", "head_repository", "base_sha", "updated_at", "state", "draft", "merged"}
	if !reflect.DeepEqual(got.Freshness.ChangedFields, want) {
		t.Errorf("Collect() changed fields = %v, want %v", got.Freshness.ChangedFields, want)
	}
	if got.Sources[SourceFreshness].Error.Code != "snapshot_changed" {
		t.Errorf("Collect() freshness error = %+v, want snapshot_changed", got.Sources[SourceFreshness].Error)
	}
}

func TestCollectPreservesSourcePermissionFailure(t *testing.T) {
	metadata := testMetadata()
	fake := &fakeAPI{metadata: []restPullRequest{metadata, metadata}}
	fake.restHook = func(endpoint string, _ any) (bool, error) {
		if strings.Contains(endpoint, "/statuses?") {
			return true, &github.Error{Kind: github.KindPermissionDenied, Message: "GitHub denied access"}
		}
		return false, nil
	}

	got := Collect(context.Background(), fake, testReference(), Options{})
	status := got.Sources[SourceStatuses]
	if got.Complete || status.Complete || status.Error == nil || status.Error.Code != "permission_denied" {
		t.Errorf("Collect() status source = %+v, complete = %t; want permission failure", status, got.Complete)
	}
}

func TestCollectGraphQLIncompleteResponses(t *testing.T) {
	for _, test := range []struct {
		name    string
		partial bool
		want    string
	}{
		{name: "partial_error", partial: true, want: "graphql_error"},
		{name: "missing_page_info", want: "invalid_response"},
	} {
		t.Run(test.name, func(t *testing.T) {
			metadata := testMetadata()
			fake := &fakeAPI{metadata: []restPullRequest{metadata, metadata}}
			fake.graphHook = func(_ string, _ map[string]any, out any) (bool, error) {
				thread := &graphQLReviewThread{
					ID: "thread-1", Comments: &graphQLCommentsConnection{
						TotalCount: intPointer(1),
						Nodes:      []*graphQLComment{testGraphQLComment(1)},
						PageInfo:   graphQLPageInfo{HasNextPage: boolPointer(false)},
					},
				}
				pageInfo := graphQLPageInfo{}
				if test.partial {
					pageInfo.HasNextPage = boolPointer(false)
				}
				if err := setThreadData(out, []*graphQLReviewThread{thread}, pageInfo); err != nil {
					return true, err
				}
				if test.partial {
					return true, &github.Error{Kind: github.KindGraphQLError, Message: "GitHub returned a partial GraphQL response"}
				}
				return true, nil
			}

			got := Collect(context.Background(), fake, testReference(), Options{})
			source := got.Sources[SourceReviewThreads]
			if got.Complete || source.Error == nil || source.Error.Code != test.want {
				t.Errorf("Collect() review source = %+v, complete = %t; want %s", source, got.Complete, test.want)
			}
			if len(got.ReviewThreads) != 1 {
				t.Fatalf("Collect() retained threads = %d, want 1", len(got.ReviewThreads))
			}
			if test.partial && got.ReviewThreads[0].CommentsSource.Complete {
				t.Errorf("Collect() partial thread comments source = %+v, want incomplete", got.ReviewThreads[0].CommentsSource)
			}
		})
	}
}

func TestCollectCanceledContextMakesNoRequests(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	fake := &fakeAPI{metadata: []restPullRequest{testMetadata()}}
	got := Collect(ctx, fake, testReference(), Options{})
	if got.Complete || len(fake.endpoints) != 0 {
		t.Errorf("Collect(canceled) complete = %t, REST requests = %d; want false, 0", got.Complete, len(fake.endpoints))
	}
	if got.Sources[SourcePullRequest].Error.Code != "canceled" {
		t.Errorf("Collect(canceled) pull request source = %+v, want canceled", got.Sources[SourcePullRequest])
	}
}

func TestCollectMarksGitHubFileLimit(t *testing.T) {
	metadata := testMetadata()
	metadata.ChangedFiles = filesAPIMax + 1
	fake := &fakeAPI{metadata: []restPullRequest{metadata, metadata}}
	fake.restHook = func(endpoint string, out any) (bool, error) {
		if !strings.Contains(endpoint, "/files?") {
			return false, nil
		}
		page := endpointPage(endpoint)
		items := make([]restFile, 100)
		for index := range items {
			number := (page-1)*100 + index
			items[index] = restFile{SHA: strconv.Itoa(number), Filename: fmt.Sprintf("file-%04d", number)}
		}
		return true, copyJSON(out, items)
	}

	got := Collect(context.Background(), fake, testReference(), Options{})
	source := got.Sources[SourceFiles]
	if len(got.Files) != filesAPIMax || source.Error == nil || source.Error.Code != "api_limit" {
		t.Errorf("Collect() files = %d, source = %+v; want 3000 and api_limit", len(got.Files), source)
	}
	if source.Requests != 30 {
		t.Errorf("Collect() file requests = %d, want 30", source.Requests)
	}
}

func TestCollectEnforcesRequestAndRetentionLimits(t *testing.T) {
	metadata := testMetadata()

	t.Run("request_reserves_final_identity", func(t *testing.T) {
		fake := &fakeAPI{metadata: []restPullRequest{metadata, metadata}}
		got := Collect(context.Background(), fake, testReference(), Options{MaxRequests: 2})
		if fake.pullReads != 2 {
			t.Errorf("Collect() pull reads = %d, want 2", fake.pullReads)
		}
		if got.Sources[SourceChecks].Error.Code != "request_limit" {
			t.Errorf("Collect() checks source = %+v, want request_limit", got.Sources[SourceChecks])
		}
	})

	t.Run("retained_payload", func(t *testing.T) {
		large := metadata
		large.Body = strings.Repeat("x", 1024)
		fake := &fakeAPI{metadata: []restPullRequest{large, large}}
		got := Collect(context.Background(), fake, testReference(), Options{MaxRetainedBytes: 256})
		if got.Sources[SourcePullRequest].Error.Code != "retention_limit" {
			t.Errorf("Collect() pull source = %+v, want retention_limit", got.Sources[SourcePullRequest])
		}
		if got.Sources[SourceChecks].Error.Code != "retention_limit" {
			t.Errorf("Collect() checks source = %+v, want retention_limit", got.Sources[SourceChecks])
		}
	})
}

func TestCollectRejectsRepeatedGraphQLCursor(t *testing.T) {
	metadata := testMetadata()
	fake := &fakeAPI{metadata: []restPullRequest{metadata, metadata}}
	fake.graphHook = func(_ string, _ map[string]any, out any) (bool, error) {
		return true, setThreadData(out, nil, graphQLPageInfo{
			HasNextPage: boolPointer(true), EndCursor: stringPointer("same"),
		})
	}

	got := Collect(context.Background(), fake, testReference(), Options{})
	source := got.Sources[SourceReviewThreads]
	if source.Error == nil || source.Error.Code != "repeated_cursor" || source.Requests != 2 {
		t.Errorf("Collect() review source = %+v, want repeated_cursor after 2 requests", source)
	}
}

func TestCollectQueriesBaseAndHeadRepositories(t *testing.T) {
	metadata := testMetadata()
	fake := &fakeAPI{metadata: []restPullRequest{metadata, metadata}}
	fake.restHook = func(endpoint string, out any) (bool, error) {
		switch {
		case strings.Contains(endpoint, "/check-runs?"):
			return true, copyJSON(out, restCheckRunsResponse{
				TotalCount: intPointer(1),
				CheckRuns:  []restCheckRun{{ID: 1, NodeID: "C_1", Name: "test", HeadSHA: "old-head"}},
			})
		case strings.Contains(endpoint, "/statuses?"):
			return true, copyJSON(out, []restCommitStatus{{ID: 2, NodeID: "S_2", Context: "ci"}})
		}
		return false, nil
	}

	got := Collect(context.Background(), fake, testReference(), Options{})
	if !got.Complete {
		t.Fatalf("Collect() complete = false, sources = %+v", got.Sources)
	}
	if len(got.Checks) != 1 || len(got.Checks[0].ObservedIn) != 2 {
		t.Errorf("Collect() checks = %+v, want one check observed in base and head", got.Checks)
	}
	if len(got.Statuses) != 1 || len(got.Statuses[0].ObservedIn) != 2 {
		t.Errorf("Collect() statuses = %+v, want one status observed in base and head", got.Statuses)
	}
	joined := strings.Join(fake.endpoints, "\n")
	for _, repository := range []string{"repos/acme/widget/commits/old-head", "repos/fork/widget/commits/old-head"} {
		if !strings.Contains(joined, repository) {
			t.Errorf("Collect() endpoints omitted %q:\n%s", repository, joined)
		}
	}
}

func TestCollectMarksMissingHeadRepositoryCoverage(t *testing.T) {
	metadata := testMetadata()
	metadata.Head.Repository = restRepository{}
	fake := &fakeAPI{metadata: []restPullRequest{metadata, metadata}}

	got := Collect(context.Background(), fake, testReference(), Options{})
	for _, key := range []string{SourceChecks, SourceStatuses} {
		source := got.Sources[key]
		if source.Error == nil || source.Error.Code != "head_repository_unavailable" {
			t.Errorf("Collect() %s source = %+v, want head_repository_unavailable", key, source)
		}
	}
}

func TestCollectUsesConfiguredGraphQLPageSize(t *testing.T) {
	metadata := testMetadata()
	fake := &fakeAPI{metadata: []restPullRequest{metadata, metadata}}
	fake.graphHook = func(_ string, variables map[string]any, out any) (bool, error) {
		if got := variables["pageSize"]; got != 17 {
			t.Errorf("GraphQL pageSize = %v, want 17", got)
		}
		return true, setThreadData(out, nil, graphQLPageInfo{HasNextPage: boolPointer(false)})
	}

	got := Collect(context.Background(), fake, testReference(), Options{PerPage: 17})
	if !got.Sources[SourceReviewThreads].Complete {
		t.Errorf("Collect() review source = %+v, want complete", got.Sources[SourceReviewThreads])
	}
}

func TestCollectRejectsDuplicateGraphQLNodeIDs(t *testing.T) {
	metadata := testMetadata()
	fake := &fakeAPI{metadata: []restPullRequest{metadata, metadata}}
	fake.graphHook = func(_ string, variables map[string]any, out any) (bool, error) {
		thread := &graphQLReviewThread{
			ID: "duplicate", Comments: &graphQLCommentsConnection{
				TotalCount: intPointer(0), PageInfo: graphQLPageInfo{HasNextPage: boolPointer(false)},
			},
		}
		if variableString(variables["cursor"]) == "" {
			return true, setThreadData(out, []*graphQLReviewThread{thread}, graphQLPageInfo{
				HasNextPage: boolPointer(true), EndCursor: stringPointer("next"),
			}, 2)
		}
		return true, setThreadData(out, []*graphQLReviewThread{thread}, graphQLPageInfo{
			HasNextPage: boolPointer(false),
		}, 2)
	}

	got := Collect(context.Background(), fake, testReference(), Options{})
	source := got.Sources[SourceReviewThreads]
	if source.Error == nil || source.Error.Code != "duplicate_item" {
		t.Errorf("Collect() review source = %+v, want duplicate_item", source)
	}
}

func TestCollectRejectsDuplicateCommentNodeIDsAcrossPages(t *testing.T) {
	metadata := testMetadata()
	fake := &fakeAPI{metadata: []restPullRequest{metadata, metadata}}
	fake.graphHook = func(query string, _ map[string]any, out any) (bool, error) {
		comment := testGraphQLComment(1)
		if strings.Contains(query, "$threadID") {
			data := reviewThreadCommentsData{Node: &struct {
				Comments *graphQLCommentsConnection `json:"comments"`
			}{Comments: &graphQLCommentsConnection{
				TotalCount: intPointer(2), Nodes: []*graphQLComment{comment},
				PageInfo: graphQLPageInfo{HasNextPage: boolPointer(false)},
			}}}
			return true, copyJSON(out, data)
		}
		thread := &graphQLReviewThread{
			ID: "thread", Comments: &graphQLCommentsConnection{
				TotalCount: intPointer(2), Nodes: []*graphQLComment{comment},
				PageInfo: graphQLPageInfo{HasNextPage: boolPointer(true), EndCursor: stringPointer("next")},
			},
		}
		return true, setThreadData(out, []*graphQLReviewThread{thread}, graphQLPageInfo{HasNextPage: boolPointer(false)})
	}

	got := Collect(context.Background(), fake, testReference(), Options{})
	comments := got.ReviewThreads[0].CommentsSource
	if comments.Error == nil || comments.Error.Code != "duplicate_item" {
		t.Errorf("Collect() comments source = %+v, want duplicate_item", comments)
	}
}

func TestCollectEnforcesNestedAndRESTCaps(t *testing.T) {
	metadata := testMetadata()

	t.Run("nested_page_limit", func(t *testing.T) {
		fake := &fakeAPI{metadata: []restPullRequest{metadata, metadata}}
		fake.graphHook = func(_ string, _ map[string]any, out any) (bool, error) {
			thread := &graphQLReviewThread{
				ID: "thread", Comments: &graphQLCommentsConnection{
					TotalCount: intPointer(2), Nodes: []*graphQLComment{testGraphQLComment(1)},
					PageInfo: graphQLPageInfo{HasNextPage: boolPointer(true), EndCursor: stringPointer("more")},
				},
			}
			return true, setThreadData(out, []*graphQLReviewThread{thread}, graphQLPageInfo{HasNextPage: boolPointer(false)})
		}
		got := Collect(context.Background(), fake, testReference(), Options{MaxPages: 1})
		comments := got.ReviewThreads[0].CommentsSource
		if comments.Error == nil || comments.Error.Code != "page_limit" {
			t.Errorf("Collect() nested comments source = %+v, want page_limit", comments)
		}
	})

	t.Run("item_limit", func(t *testing.T) {
		fake := &fakeAPI{metadata: []restPullRequest{metadata, metadata}}
		fake.restHook = func(endpoint string, out any) (bool, error) {
			if strings.Contains(endpoint, "/issues/7/comments?") {
				return true, copyJSON(out, []restIssueComment{{ID: 1}, {ID: 2}})
			}
			return false, nil
		}
		got := Collect(context.Background(), fake, testReference(), Options{PerPage: 2, MaxItems: 1})
		source := got.Sources[SourceIssueComments]
		if source.Error == nil || source.Error.Code != "item_limit" || len(got.IssueComments) != 1 {
			t.Errorf("Collect() issue comments = %d, source = %+v; want one item and item_limit", len(got.IssueComments), source)
		}
	})
}

func TestCollectRejectsSameRepositoryCheckAndStatusOverlap(t *testing.T) {
	metadata := testMetadata()
	metadata.Head.Repository = metadata.Base.Repository

	t.Run("checks", func(t *testing.T) {
		fake := &fakeAPI{metadata: []restPullRequest{metadata, metadata}}
		fake.restHook = func(endpoint string, out any) (bool, error) {
			if !strings.Contains(endpoint, "/check-runs?") {
				return false, nil
			}
			items := []restCheckRun{{ID: 1, NodeID: "C_1"}, {ID: 2, NodeID: "C_2"}}
			if endpointPage(endpoint) == 2 {
				items = []restCheckRun{{ID: 2, NodeID: "C_2"}}
			}
			return true, copyJSON(out, restCheckRunsResponse{TotalCount: intPointer(3), CheckRuns: items})
		}
		got := Collect(context.Background(), fake, testReference(), Options{PerPage: 2})
		source := got.Sources[SourceChecks]
		if source.Error == nil || source.Error.Code != "duplicate_item" || len(got.Checks) != 2 {
			t.Errorf("Collect() checks = %d, source = %+v; want duplicate_item after 2 unique records", len(got.Checks), source)
		}
	})

	t.Run("statuses", func(t *testing.T) {
		fake := &fakeAPI{metadata: []restPullRequest{metadata, metadata}}
		fake.restHook = func(endpoint string, out any) (bool, error) {
			if !strings.Contains(endpoint, "/statuses?") {
				return false, nil
			}
			items := []restCommitStatus{{ID: 1, NodeID: "S_1"}, {ID: 2, NodeID: "S_2"}}
			if endpointPage(endpoint) == 2 {
				items = []restCommitStatus{{ID: 2, NodeID: "S_2"}}
			}
			return true, copyJSON(out, items)
		}
		got := Collect(context.Background(), fake, testReference(), Options{PerPage: 2})
		source := got.Sources[SourceStatuses]
		if source.Error == nil || source.Error.Code != "duplicate_item" || len(got.Statuses) != 2 {
			t.Errorf("Collect() statuses = %d, source = %+v; want duplicate_item after 2 unique records", len(got.Statuses), source)
		}
	})
}

func TestCollectRejectsOverlappingRESTPages(t *testing.T) {
	t.Run("files", func(t *testing.T) {
		metadata := testMetadata()
		metadata.ChangedFiles = 3
		fake := &fakeAPI{metadata: []restPullRequest{metadata, metadata}}
		fake.restHook = func(endpoint string, out any) (bool, error) {
			if !strings.Contains(endpoint, "/files?") {
				return false, nil
			}
			items := []restFile{{Filename: "a.go"}, {Filename: "b.go"}}
			if endpointPage(endpoint) == 2 {
				items = []restFile{{Filename: "b.go"}}
			}
			return true, copyJSON(out, items)
		}
		got := Collect(context.Background(), fake, testReference(), Options{PerPage: 2})
		if source := got.Sources[SourceFiles]; source.Error == nil || source.Error.Code != "duplicate_item" {
			t.Errorf("Collect() file source = %+v, want duplicate_item", source)
		}
	})

	t.Run("issue_comments", func(t *testing.T) {
		metadata := testMetadata()
		fake := &fakeAPI{metadata: []restPullRequest{metadata, metadata}}
		fake.restHook = func(endpoint string, out any) (bool, error) {
			if !strings.Contains(endpoint, "/issues/7/comments?") {
				return false, nil
			}
			items := []restIssueComment{{ID: 1}, {ID: 2}}
			if endpointPage(endpoint) == 2 {
				items = []restIssueComment{{ID: 2}}
			}
			return true, copyJSON(out, items)
		}
		got := Collect(context.Background(), fake, testReference(), Options{PerPage: 2})
		if source := got.Sources[SourceIssueComments]; source.Error == nil || source.Error.Code != "duplicate_item" {
			t.Errorf("Collect() issue comment source = %+v, want duplicate_item", source)
		}
	})

	t.Run("reviews", func(t *testing.T) {
		metadata := testMetadata()
		fake := &fakeAPI{metadata: []restPullRequest{metadata, metadata}}
		fake.restHook = func(endpoint string, out any) (bool, error) {
			if !strings.Contains(endpoint, "/pulls/7/reviews?") {
				return false, nil
			}
			items := []restReview{{ID: 1}, {ID: 2}}
			if endpointPage(endpoint) == 2 {
				items = []restReview{{ID: 2}}
			}
			return true, copyJSON(out, items)
		}
		got := Collect(context.Background(), fake, testReference(), Options{PerPage: 2})
		if source := got.Sources[SourceReviews]; source.Error == nil || source.Error.Code != "duplicate_item" {
			t.Errorf("Collect() review source = %+v, want duplicate_item", source)
		}
	})
}

func testReference() github.PullRequest {
	return github.PullRequest{
		Repository: github.Repository{Host: "github.com", Owner: "acme", Name: "widget"},
		Number:     7,
	}
}

func testMetadata() restPullRequest {
	mergeable := true
	return restPullRequest{
		ID: 1, NodeID: "PR_1", Number: 7, HTMLURL: "https://github.com/acme/widget/pull/7",
		Title: "Improve collector", Body: "body", State: "open", Mergeable: &mergeable,
		MergeableState: "clean", UpdatedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		User: restActor{Login: "octocat", ID: 9, NodeID: "U_9"},
		Head: restBranch{Ref: "feature", SHA: "old-head", Repository: restRepository{
			ID: 10, NodeID: "R_10", FullName: "fork/widget", HTMLURL: "https://github.com/fork/widget",
		}},
		Base: restBranch{Ref: "main", SHA: "base-sha", Repository: restRepository{
			ID: 11, NodeID: "R_11", FullName: "acme/widget", HTMLURL: "https://github.com/acme/widget",
		}},
	}
}

func testGraphQLComment(id int) *graphQLComment {
	created := time.Date(2026, 1, 1, 0, 0, id%60, 0, time.UTC)
	return &graphQLComment{
		NodeID: fmt.Sprintf("comment-%03d", id), FullDatabaseID: json.RawMessage(strconv.Itoa(id)),
		URL:    fmt.Sprintf("https://github.com/acme/widget/pull/7#discussion_r%d", id),
		Author: &Actor{Login: "reviewer"}, Body: strconv.Itoa(id), CreatedAt: created, UpdatedAt: created,
	}
}

func setThreadData(out any, nodes []*graphQLReviewThread, pageInfo graphQLPageInfo, totals ...int) error {
	total := len(nodes)
	if len(totals) != 0 {
		total = totals[0]
	}
	data := reviewThreadsData{Repository: &struct {
		PullRequest *struct {
			ReviewThreads *graphQLReviewThreadsConnection `json:"reviewThreads"`
		} `json:"pullRequest"`
	}{PullRequest: &struct {
		ReviewThreads *graphQLReviewThreadsConnection `json:"reviewThreads"`
	}{ReviewThreads: &graphQLReviewThreadsConnection{
		TotalCount: intPointer(total), Nodes: nodes, PageInfo: pageInfo,
	}}}}
	return copyJSON(out, data)
}

func copyJSON(out, value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return json.Unmarshal(encoded, out)
}

func endpointPage(endpoint string) int {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return 0
	}
	page, _ := strconv.Atoi(parsed.Query().Get("page"))
	return page
}

func variableString(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case *string:
		if typed != nil {
			return *typed
		}
	}
	return ""
}

func boolPointer(value bool) *bool       { return &value }
func intPointer(value int) *int          { return &value }
func stringPointer(value string) *string { return &value }

func fileNames(files []File) []string {
	result := make([]string, len(files))
	for index := range files {
		result[index] = files[index].Filename
	}
	return result
}

func statusIDs(statuses []CommitStatus) []int64 {
	result := make([]int64, len(statuses))
	for index := range statuses {
		result[index] = statuses[index].ID
	}
	return result
}

func commentIDs(comments []Comment) []int64 {
	result := make([]int64, len(comments))
	for index := range comments {
		result[index] = comments[index].ID
	}
	return result
}
