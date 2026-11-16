package prdelta

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"aid/internal/evidence"
	"aid/internal/github"
)

type obj = map[string]any

func sha(char string) string { return strings.Repeat(char, 40) }

var (
	baselineSHA = sha("1")
	headSHA     = sha("2")
	movedSHA    = sha("9")
	mergeBase1  = sha("3")
	mergeBase2  = sha("4")
	recordedSHA = sha("e")
	mainTip     = sha("b")
)

type fakeAPI struct {
	t           *testing.T
	pulls       []obj
	pullReads   int
	rest        map[string]any
	threads     []obj
	threadReads int
	nested      map[string]obj
	stack       obj
	tips        map[string][]string
	tipReads    map[string]int
	endpoints   []string
	graphVars   []map[string]any
}

// newFake answers branch-tip lookups for main with mainTip and stack lookups
// with an existing branch and no pull requests unless a test overrides them.
func newFake(t *testing.T, pulls ...obj) *fakeAPI {
	return &fakeAPI{t: t, pulls: pulls, rest: map[string]any{}, nested: map[string]obj{},
		stack: obj{"ref": obj{"name": "x"}, "pullRequests": obj{"totalCount": 0, "nodes": []any{}}},
		tips:  map[string][]string{"refs/heads/main": {mainTip}}, tipReads: map[string]int{}}
}

func (fake *fakeAPI) REST(_ context.Context, endpoint string, out any) error {
	fake.endpoints = append(fake.endpoints, endpoint)
	if endpoint == "repos/acme/widget/pulls/7" {
		index := min(fake.pullReads, len(fake.pulls)-1)
		fake.pullReads++
		return copyJSON(out, fake.pulls[index])
	}
	if value, ok := fake.rest[endpoint]; ok {
		if err, ok := value.(error); ok {
			return err
		}
		return copyJSON(out, value)
	}
	switch {
	case strings.Contains(endpoint, "/check-runs?"):
		return copyJSON(out, obj{"total_count": 0, "check_runs": []any{}})
	case strings.Contains(endpoint, "/status?"):
		return copyJSON(out, obj{"total_count": 0, "statuses": []any{}})
	}
	fake.t.Errorf("unexpected REST endpoint %q", endpoint)
	return &github.Error{Kind: github.KindNotFound, Message: "not found"}
}

func (fake *fakeAPI) GraphQL(_ context.Context, query string, variables map[string]any, out any) error {
	fake.graphVars = append(fake.graphVars, variables)
	switch {
	case strings.Contains(query, "node(id:"):
		return copyJSON(out, obj{"node": obj{"comments": fake.nested[variables["id"].(string)]}})
	case strings.Contains(query, "pullRequests(headRefName:"):
		return copyJSON(out, obj{"repository": fake.stack})
	case strings.Contains(query, "target { oid }"):
		qualified := variables["qualified"].(string)
		tips, ok := fake.tips[qualified]
		if !ok {
			return copyJSON(out, obj{"repository": obj{"ref": nil}})
		}
		tip := tips[min(fake.tipReads[qualified], len(tips)-1)]
		fake.tipReads[qualified]++
		return copyJSON(out, obj{"repository": obj{"ref": obj{"target": obj{"oid": tip}}}})
	}
	page := fake.threadReads
	fake.threadReads++
	threads := obj{"pageInfo": obj{"hasNextPage": false}, "nodes": []any{}}
	if page < len(fake.threads) {
		threads = fake.threads[page]
	}
	return copyJSON(out, obj{"repository": obj{"pullRequest": obj{"reviewDecision": "APPROVED", "reviewThreads": threads}}})
}

func copyJSON(out, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, out)
}

func pullAt(head, baseRef, state string) obj {
	return obj{"number": 7, "state": state, "merged": state == "merged", "mergeable_state": "clean", "user": obj{"login": "author"},
		"head": obj{"ref": "topic", "sha": head, "repo": obj{"full_name": "acme/widget"}},
		"base": obj{"ref": baseRef, "sha": recordedSHA, "repo": obj{"full_name": "acme/widget", "default_branch": "main"}}}
}

func commit(id, parent, subject string) obj {
	return obj{"sha": id, "parents": []any{obj{"sha": parent}},
		"commit": obj{"message": subject + "\n\nbody", "author": obj{"email": "dev@example.com", "date": "2026-01-01T00:00:00Z"}}}
}

func file(name, status string, patch any) obj {
	return obj{"filename": name, "status": status, "patch": patch, "sha": "blob-" + name, "changes": 1}
}

func compareOf(status, mergeBase string, commits []any, files []any) obj {
	return obj{"status": status, "ahead_by": len(commits), "total_commits": len(commits),
		"merge_base_commit": obj{"sha": mergeBase}, "commits": commits, "files": files}
}

func compareEP(base, head string) string {
	return "repos/acme/widget/compare/" + base + "..." + head + "?per_page=100&page=1"
}

func reviewsEP() string { return "repos/acme/widget/pulls/7/reviews?per_page=100&page=1" }

func testRef() github.PullRequest {
	return github.PullRequest{Repository: github.Repository{Host: "github.com", Owner: "acme", Name: "widget"}, Number: 7}
}

const (
	patchTop    = "@@ -1,2 +1,3 @@\n a\n+b\n c"
	patchMoved  = "@@ -10,2 +10,3 @@ func x\n a\n+b\n c"
	patchEdited = "@@ -1,2 +1,3 @@\n a\n+bee\n c"
)

func TestFastForwardReportsNewCommitsAndChangedHunks(t *testing.T) {
	fake := newFake(t, pullAt(headSHA, "main", "open"))
	fake.rest[compareEP(baselineSHA, headSHA)] = compareOf("ahead", baselineSHA, []any{commit(headSHA, baselineSHA, "fix: edit b")}, nil)
	fake.rest[compareEP(mainTip, headSHA)] = compareOf("ahead", mergeBase1, []any{commit(baselineSHA, mergeBase1, "feat"), commit(headSHA, baselineSHA, "fix: edit b")},
		[]any{file("a.go", "modified", patchEdited), file("b.go", "modified", patchTop)})
	fake.rest[compareEP(mainTip, baselineSHA)] = compareOf("ahead", mergeBase1, []any{commit(baselineSHA, mergeBase1, "feat")},
		[]any{file("a.go", "modified", patchTop), file("b.go", "modified", patchTop)})

	got := Collect(context.Background(), fake, testRef(), Options{Reviewer: "octo", SinceCommit: baselineSHA})
	if !got.Complete || got.HeadRelation != RelationFastForward {
		t.Fatalf("Collect() complete=%v relation=%s sources=%+v", got.Complete, got.HeadRelation, got.Sources)
	}
	if want := []CommitRef{{SHA: headSHA, MessageFirstLine: "fix: edit b"}}; !reflect.DeepEqual(got.NewCommits, want) {
		t.Errorf("new_commits = %+v, want %+v", got.NewCommits, want)
	}
	diff := got.PRDiff
	if diff.Verdict != VerdictChanged || diff.Files[0].Path != "a.go" || diff.Files[0].Status != VerdictChanged || diff.Files[1].Status != VerdictUnchanged {
		t.Fatalf("pr_diff = %+v", diff)
	}
	if want := []HunkChange{{OldHeader: "@@ -1,2 +1,3 @@", NewHeader: "@@ -1,2 +1,3 @@"}}; !reflect.DeepEqual(diff.Files[0].ChangedHunks, want) {
		t.Errorf("changed hunks = %+v, want %+v", diff.Files[0].ChangedHunks, want)
	}
	if _, ok := got.Sources[SourceCommits]; ok {
		t.Error("fast-forward should not derive patch equivalence")
	}
}

func rebasedFake(t *testing.T, headFiles, baselineFiles []any, headPatch, baselinePatch any) *fakeAPI {
	fake := newFake(t, pullAt(headSHA, "main", "open"))
	fake.rest[compareEP(baselineSHA, headSHA)] = compareOf("diverged", mergeBase1, []any{commit(movedSHA, mergeBase2, "main work"), commit(headSHA, mergeBase2, "feat")}, nil)
	fake.rest[compareEP(mainTip, headSHA)] = compareOf("ahead", mergeBase2, []any{commit(headSHA, mergeBase2, "feat")}, headFiles)
	fake.rest[compareEP(mainTip, baselineSHA)] = compareOf("ahead", mergeBase1, []any{commit(baselineSHA, mergeBase1, "feat")}, baselineFiles)
	fake.rest["repos/acme/widget/commits?page=1&per_page=100&sha="+mainTip] = []any{commit(mergeBase2, mergeBase1, "main work"), commit(mergeBase1, sha("0"), "old")}
	fake.rest["repos/acme/widget/commits/"+headSHA] = obj{"sha": headSHA, "parents": []any{obj{"sha": mergeBase2}}, "files": []any{file("a.go", "modified", headPatch)}}
	fake.rest["repos/acme/widget/commits/"+baselineSHA] = obj{"sha": baselineSHA, "parents": []any{obj{"sha": mergeBase1}}, "files": []any{file("a.go", "modified", baselinePatch)}}
	return fake
}

func TestRebaseWithShiftedHunksIsMovedOnly(t *testing.T) {
	fake := rebasedFake(t, []any{file("a.go", "modified", patchMoved)}, []any{file("a.go", "modified", patchTop)}, patchMoved, patchTop)
	got := Collect(context.Background(), fake, testRef(), Options{Reviewer: "octo", SinceCommit: baselineSHA})
	if !got.Complete || got.HeadRelation != RelationRebased || got.PRDiff.Verdict != VerdictMovedOnly {
		t.Fatalf("Collect() complete=%v relation=%s diff=%+v sources=%+v", got.Complete, got.HeadRelation, got.PRDiff, got.Sources)
	}
	if got.NewCommits == nil || len(got.NewCommits) != 0 {
		t.Errorf("new_commits = %#v, want empty list (all patches equivalent)", got.NewCommits)
	}
	if got.PRDiff.Files[0].Status != VerdictMovedOnly || len(got.PRDiff.Files[0].ChangedHunks) != 0 {
		t.Errorf("file = %+v, want moved_only without changed hunks", got.PRDiff.Files[0])
	}
}

func TestRebaseDroppingFileIsChangedWithRemoved(t *testing.T) {
	fake := rebasedFake(t, []any{file("a.go", "modified", patchTop)},
		[]any{file("a.go", "modified", patchTop), file("gone.go", "added", "@@ -0,0 +1 @@\n+x")}, patchTop, patchEdited)
	got := Collect(context.Background(), fake, testRef(), Options{Reviewer: "octo", SinceCommit: baselineSHA})
	if got.PRDiff.Verdict != VerdictChanged || got.PRDiff.Counts[FileRemoved] != 1 || got.PRDiff.Files[0].Path != "gone.go" {
		t.Fatalf("pr_diff = %+v", got.PRDiff)
	}
	if want := []HunkChange{{OldHeader: "@@ -0,0 +1 @@"}}; !reflect.DeepEqual(got.PRDiff.Files[0].ChangedHunks, want) {
		t.Errorf("removed hunks = %+v, want %+v", got.PRDiff.Files[0].ChangedHunks, want)
	}
	if len(got.NewCommits) != 1 || got.NewCommits[0].SHA != headSHA {
		t.Errorf("new_commits = %+v, want the non-equivalent head commit", got.NewCommits)
	}
}

func TestRewrittenBaseRealignsBaselineSide(t *testing.T) {
	oldBase := sha("5")
	fake := newFake(t, pullAt(headSHA, "stack-base", "merged"))
	fake.rest[compareEP(baselineSHA, headSHA)] = compareOf("diverged", mergeBase1, []any{commit(headSHA, recordedSHA, "feat")}, nil)
	fake.rest[compareEP(recordedSHA, headSHA)] = compareOf("ahead", recordedSHA, []any{commit(headSHA, recordedSHA, "feat")},
		[]any{file("a.go", "modified", patchMoved)})
	fake.rest[compareEP(recordedSHA, baselineSHA)] = compareOf("diverged", mergeBase1,
		[]any{commit(oldBase, mergeBase1, "base work"), commit(baselineSHA, oldBase, "feat")},
		[]any{file("a.go", "modified", patchTop), file("base.go", "modified", patchTop)})
	fake.rest["repos/acme/widget/commits?page=1&per_page=100&sha="+recordedSHA] = []any{commit(recordedSHA, sha("6"), "base work"), commit(mergeBase1, sha("0"), "old")}
	fake.rest[compareEP(oldBase, baselineSHA)] = compareOf("ahead", oldBase, []any{commit(baselineSHA, oldBase, "feat")}, []any{file("a.go", "modified", patchTop)})
	fake.rest["repos/acme/widget/commits/"+headSHA] = obj{"sha": headSHA, "parents": []any{obj{"sha": recordedSHA}}, "files": []any{file("a.go", "modified", patchMoved)}}
	fake.rest["repos/acme/widget/commits/"+baselineSHA] = obj{"sha": baselineSHA, "parents": []any{obj{"sha": oldBase}}, "files": []any{file("a.go", "modified", patchTop)}}

	got := Collect(context.Background(), fake, testRef(), Options{Reviewer: "octo", SinceCommit: baselineSHA})
	if got.PRDiff.Verdict != VerdictMovedOnly || got.PRDiff.BaselineSide.Base != oldBase || got.PRDiff.Base.Source != "recorded_base_sha" {
		t.Fatalf("pr_diff = %+v", got.PRDiff)
	}
	if !reflect.DeepEqual(got.Warnings, []string{"baseline_base_realigned"}) {
		t.Errorf("warnings = %v", got.Warnings)
	}
}

func TestMissingBaselineHasNoDelta(t *testing.T) {
	fake := newFake(t, pullAt(headSHA, "main", "open"))
	fake.rest[reviewsEP()] = []any{
		obj{"id": 1, "user": obj{"login": "someone"}, "state": "APPROVED", "commit_id": baselineSHA, "submitted_at": "2026-01-01T00:00:00Z"},
		obj{"id": 2, "user": obj{"login": "octo"}, "state": "PENDING", "commit_id": baselineSHA},
	}
	got := Collect(context.Background(), fake, testRef(), Options{Reviewer: "octo"})
	if got.Complete || got.Baseline != nil || got.PRDiff != nil || got.NewCommits != nil || got.HeadRelation != RelationUnknown {
		t.Fatalf("Collect() = complete %v baseline %+v diff %+v commits %v", got.Complete, got.Baseline, got.PRDiff, got.NewCommits)
	}
	if !got.Sources[SourceBaseline].Complete || !reflect.DeepEqual(got.Warnings, []string{"baseline_missing"}) {
		t.Errorf("baseline source = %+v warnings = %v", got.Sources[SourceBaseline], got.Warnings)
	}
	for _, endpoint := range fake.endpoints {
		if strings.Contains(endpoint, "/compare/") {
			t.Errorf("missing baseline should not compare: %s", endpoint)
		}
	}
}

func TestReviewerDefaultsToAuthenticatedUser(t *testing.T) {
	fake := newFake(t, pullAt(headSHA, "main", "open"))
	fake.rest["user"] = obj{"login": "Octo"}
	fake.rest[reviewsEP()] = []any{
		obj{"id": 10, "user": obj{"login": "octo"}, "state": "CHANGES_REQUESTED", "commit_id": baselineSHA, "submitted_at": "2026-01-01T00:00:00Z"},
		obj{"id": 11, "user": obj{"login": "octo"}, "state": "APPROVED", "commit_id": headSHA, "submitted_at": "2026-01-02T00:00:00Z"},
		obj{"id": 12, "user": obj{"login": "other"}, "state": "APPROVED", "commit_id": baselineSHA, "submitted_at": "2026-01-03T00:00:00Z"},
		obj{"id": 13, "user": obj{"login": "octo"}, "state": "PENDING", "commit_id": baselineSHA},
	}
	fake.rest[compareEP(mainTip, headSHA)] = compareOf("ahead", mergeBase1, []any{commit(headSHA, mergeBase1, "feat")}, []any{file("a.go", "modified", patchTop)})
	got := Collect(context.Background(), fake, testRef(), Options{})
	if got.Reviewer != "Octo" || got.ReviewerSource != "authenticated_user" {
		t.Fatalf("reviewer = %q (%s)", got.Reviewer, got.ReviewerSource)
	}
	if got.Baseline == nil || *got.Baseline.ReviewID != 11 || got.Baseline.ReviewState != "APPROVED" || got.Baseline.Source != "review" {
		t.Fatalf("baseline = %+v", got.Baseline)
	}
	if !got.Complete || got.HeadRelation != RelationIdentical || got.PRDiff.Verdict != VerdictUnchanged || got.PR.ReviewDecision != "APPROVED" {
		t.Errorf("identical head: complete=%v relation=%s diff=%+v", got.Complete, got.HeadRelation, got.PRDiff)
	}
}

func TestDeletedBaseBranchFallsBackToDefaultBranch(t *testing.T) {
	fake := newFake(t, pullAt(headSHA, "feature", "open"))
	fake.stack = obj{"ref": nil, "pullRequests": obj{"totalCount": 1, "nodes": []any{
		obj{"number": 6, "state": "MERGED", "headRepository": obj{"nameWithOwner": "acme/widget"}},
	}}}
	fake.rest[compareEP(baselineSHA, headSHA)] = compareOf("ahead", baselineSHA, []any{commit(headSHA, baselineSHA, "more")}, nil)
	fake.rest[compareEP(mainTip, headSHA)] = compareOf("ahead", mergeBase1, []any{commit(baselineSHA, mergeBase1, "feat"), commit(headSHA, baselineSHA, "more")}, []any{file("a.go", "modified", patchTop)})
	fake.rest[compareEP(mainTip, baselineSHA)] = compareOf("ahead", mergeBase1, []any{commit(baselineSHA, mergeBase1, "feat")}, []any{file("a.go", "modified", patchTop)})
	got := Collect(context.Background(), fake, testRef(), Options{Reviewer: "octo", SinceCommit: baselineSHA})
	if got.PRDiff.Base != (DiffBase{Ref: "main", SHA: mainTip, Source: "default_branch"}) || !reflect.DeepEqual(got.Warnings, []string{"base_branch_missing"}) {
		t.Fatalf("base = %+v warnings = %v", got.PRDiff.Base, got.Warnings)
	}
	stack := got.Stack
	if stack.BaseIsDefaultBranch || stack.BaseBranchExists == nil || *stack.BaseBranchExists || stack.BasePR == nil || *stack.BasePR != (BasePR{Number: 6, State: "merged", Merged: true}) {
		t.Errorf("stack = %+v base_pr = %+v", stack, stack.BasePR)
	}
	if !got.Sources[SourceStack].Complete || !got.Complete {
		t.Errorf("a missing base branch is evidence, not a failure: sources = %+v", got.Sources)
	}
}

func threadNode(id string, resolved bool, comments []any, hasNext bool) obj {
	return obj{"id": id, "isResolved": resolved, "isOutdated": false, "path": "a.go", "line": 3,
		"comments": obj{"pageInfo": obj{"hasNextPage": hasNext, "endCursor": "c-" + id}, "nodes": comments}}
}

func comment(id int64, login, body string) obj {
	return obj{"fullDatabaseId": fmt.Sprint(id), "author": obj{"login": login}, "body": body}
}

func TestThreadReplyDetection(t *testing.T) {
	fake := newFake(t, pullAt(headSHA, "main", "open"))
	fake.rest[compareEP(mainTip, headSHA)] = compareOf("ahead", mergeBase1, nil, nil)
	fake.threads = []obj{{"pageInfo": obj{"hasNextPage": true, "endCursor": "p1"}, "nodes": []any{
		threadNode("T1", false, []any{comment(4000000001, "octo", "please fix"), comment(2, "author", "first")}, true),
		threadNode("T2", false, []any{comment(3, "octo", "nit"), comment(4, "author", "done"), comment(5, "octo", "thanks")}, false),
	}}, {"pageInfo": obj{"hasNextPage": false}, "nodes": []any{
		threadNode("T3", false, []any{comment(6, "someone", "mine"), comment(7, "author", "reply")}, false),
		threadNode("T4", true, []any{comment(8, "octo", "q"), comment(9, "author", "a")}, false),
		threadNode("T5", false, []any{comment(11, "octo", "q"), comment(12, "author", "fixed"), comment(13, "helper", "lgtm")}, false),
		threadNode("T6", false, []any{comment(14, "octo", "q"), comment(15, "copilot", "bot says")}, false),
	}}}
	fake.nested["T1"] = obj{"pageInfo": obj{"hasNextPage": false}, "nodes": []any{comment(10, "author", "Fixed   in\nabc1234 with a long explanation")}}
	got := Collect(context.Background(), fake, testRef(), Options{Reviewer: "octo", SinceCommit: headSHA, TextLimit: 16})
	threads := got.Threads
	if threads.Total != 5 || threads.Unresolved != 4 || threads.UnresolvedWithAuthorReply != 2 {
		t.Fatalf("thread counts = %+v", threads)
	}
	first := threads.Items[0]
	if first.ThreadID != "T1" || first.FirstCommentID != 4000000001 || !first.AuthorRepliedAfter ||
		first.LastReplyBy != "author" || first.LastReplyExcerpt != "Fixed in abc1234" || !first.LastReplyClipped {
		t.Errorf("T1 = %+v", first)
	}
	if threads.Items[1].AuthorRepliedAfter || threads.Items[1].LastReplyExcerpt != "" {
		t.Errorf("T2 reviewer replied last; got %+v", threads.Items[1])
	}
	if fifth := threads.Items[3]; !fifth.AuthorRepliedAfter || fifth.LastReplyBy != "helper" || fifth.LastReplyExcerpt != "lgtm" {
		t.Errorf("T5 author replied before a third party; got %+v", fifth)
	}
	if sixth := threads.Items[4]; sixth.AuthorRepliedAfter || sixth.LastReplyBy != "copilot" || sixth.LastReplyExcerpt != "bot says" {
		t.Errorf("T6 only a bot replied; got %+v", sixth)
	}
	if !got.Sources[SourceThreads].Complete || got.Sources[SourceThreads].Requests != 3 {
		t.Errorf("threads source = %+v", got.Sources[SourceThreads])
	}
}

func TestPatchOmittedIsUnknownAndIncomplete(t *testing.T) {
	fake := newFake(t, pullAt(headSHA, "main", "open"))
	fake.rest[compareEP(baselineSHA, headSHA)] = compareOf("ahead", baselineSHA, []any{commit(headSHA, baselineSHA, "bin")}, nil)
	fake.rest[compareEP(mainTip, headSHA)] = compareOf("ahead", mergeBase1, nil, []any{file("logo.png", "modified", nil), file("a.go", "modified", patchTop)})
	fake.rest[compareEP(mainTip, baselineSHA)] = compareOf("ahead", mergeBase1, nil, []any{file("logo.png", "modified", nil), file("a.go", "modified", patchTop)})
	got := Collect(context.Background(), fake, testRef(), Options{Reviewer: "octo", SinceCommit: baselineSHA})
	if got.Complete || got.PRDiff.Verdict != VerdictUnknown || got.Sources[SourcePRDiff].Error == nil {
		t.Fatalf("Collect() complete=%v diff=%+v source=%+v", got.Complete, got.PRDiff, got.Sources[SourcePRDiff])
	}
	if got.PRDiff.Files[0].Path != "logo.png" || got.PRDiff.Files[0].Status != VerdictUnknown {
		t.Errorf("files = %+v", got.PRDiff.Files)
	}
}

func TestHeadMovedDuringCollectionIsIncomplete(t *testing.T) {
	fake := newFake(t, pullAt(headSHA, "main", "open"), pullAt(sha("7"), "main", "open"))
	fake.rest[compareEP(mainTip, headSHA)] = compareOf("ahead", mergeBase1, nil, nil)
	got := Collect(context.Background(), fake, testRef(), Options{Reviewer: "octo", SinceCommit: headSHA})
	if got.Complete || got.HeadStable.Stable || got.HeadStable.End != sha("7") || !reflect.DeepEqual(got.Warnings, []string{"head_moved"}) {
		t.Fatalf("head_stable = %+v warnings = %v complete = %v", got.HeadStable, got.Warnings, got.Complete)
	}
}

func TestShortSinceCommitIsResolved(t *testing.T) {
	fake := newFake(t, pullAt(headSHA, "main", "open"))
	fake.rest["repos/acme/widget/commits/2222222"] = obj{"sha": headSHA}
	fake.rest[compareEP(mainTip, headSHA)] = compareOf("ahead", mergeBase1, nil, nil)
	got := Collect(context.Background(), fake, testRef(), Options{Reviewer: "octo", SinceCommit: "2222222"})
	if got.Baseline == nil || got.Baseline.Commit != headSHA || got.Baseline.Source != "flag" || !got.Complete {
		t.Fatalf("baseline = %+v complete = %v", got.Baseline, got.Complete)
	}
}

func TestCICountsAndNonSuccessNames(t *testing.T) {
	fake := newFake(t, pullAt(headSHA, "main", "open"))
	fake.rest[compareEP(mainTip, headSHA)] = compareOf("ahead", mergeBase1, nil, nil)
	fake.rest["repos/acme/widget/commits/"+headSHA+"/check-runs?filter=latest&per_page=100&page=1"] = obj{"total_count": 4, "check_runs": []any{
		obj{"name": "lint", "status": "completed", "conclusion": "success"},
		obj{"name": "test", "status": "completed", "conclusion": "failure"},
		obj{"name": "e2e", "status": "in_progress"},
		obj{"name": "slow", "status": "completed", "conclusion": "timed_out"},
	}}
	fake.rest["repos/acme/widget/commits/"+headSHA+"/status?per_page=100&page=1"] = obj{"total_count": 1, "statuses": []any{obj{"context": "legacy", "state": "error"}}}
	got := Collect(context.Background(), fake, testRef(), Options{Reviewer: "octo", SinceCommit: headSHA})
	if want := (CICounts{Success: 1, Failure: 2, Pending: 1, Other: 1}); got.CI.Counts != want {
		t.Errorf("counts = %+v, want %+v", got.CI.Counts, want)
	}
	var names []string
	for _, record := range got.CI.NonSuccess {
		names = append(names, record.Name)
	}
	if !reflect.DeepEqual(names, []string{"legacy", "slow", "test", "e2e"}) {
		t.Errorf("non-success names = %v", names)
	}
}

func TestParseHunksIgnoresHeaderOffsets(t *testing.T) {
	first, ok1 := parseHunks(patchTop + "\n@@ -20 +21 @@\n-x\n+y")
	second, ok2 := parseHunks(patchMoved)
	if !ok1 || !ok2 || len(first) != 2 || first[0].body != second[0].body || first[1].body != "-x\n+y" {
		t.Fatalf("parseHunks = %+v / %+v", first, second)
	}
	if _, ok := parseHunks("not a patch"); ok {
		t.Error("patch without a hunk header should not parse")
	}
}

func TestBaseMovedDuringCollectionIsIncomplete(t *testing.T) {
	fake := newFake(t, pullAt(headSHA, "main", "open"))
	fake.tips["refs/heads/main"] = []string{mainTip, sha("f")}
	fake.rest[compareEP(mainTip, headSHA)] = compareOf("ahead", mergeBase1, nil, nil)
	got := Collect(context.Background(), fake, testRef(), Options{Reviewer: "octo", SinceCommit: headSHA})
	if got.Complete || !got.HeadStable.Stable || !reflect.DeepEqual(got.Warnings, []string{"base_moved"}) {
		t.Fatalf("complete = %v head_stable = %+v warnings = %v", got.Complete, got.HeadStable, got.Warnings)
	}
	if source := got.Sources[SourcePRDiff]; source.Error == nil || source.Error.Code != "snapshot_changed" {
		t.Errorf("pr_diff source = %+v", source)
	}
	if got.PRDiff.Base != (DiffBase{Ref: "main", SHA: mainTip, Source: "base_ref"}) || fake.tipReads["refs/heads/main"] != 2 {
		t.Errorf("base = %+v tip reads = %d", got.PRDiff.Base, fake.tipReads["refs/heads/main"])
	}
}

func TestBaseNeedingPathEscapesUsesGraphQL(t *testing.T) {
	branch, tip := "stack+fix#1", sha("c")
	fake := newFake(t, pullAt(headSHA, branch, "open"))
	fake.tips["refs/heads/"+branch] = []string{tip}
	fake.stack = obj{"ref": obj{"name": branch}, "pullRequests": obj{"totalCount": 3, "nodes": []any{
		obj{"number": 9, "state": "OPEN", "headRepository": obj{"nameWithOwner": "fork/widget"}},
		obj{"number": 5, "state": "MERGED", "headRepository": obj{"nameWithOwner": "acme/widget"}},
		obj{"number": 4, "state": "OPEN", "headRepository": obj{"nameWithOwner": "acme/widget"}},
	}}}
	fake.rest[compareEP(tip, headSHA)] = compareOf("ahead", mergeBase1, nil, nil)
	got := Collect(context.Background(), fake, testRef(), Options{Reviewer: "octo", SinceCommit: headSHA})
	if !got.Complete || got.PRDiff.Base != (DiffBase{Ref: branch, SHA: tip, Source: "base_ref"}) {
		t.Fatalf("complete = %v base = %+v sources = %+v", got.Complete, got.PRDiff.Base, got.Sources)
	}
	if stack := got.Stack; stack.BaseBranchExists == nil || !*stack.BaseBranchExists || stack.BasePR == nil ||
		*stack.BasePR != (BasePR{Number: 4, State: "open"}) {
		t.Errorf("stack = %+v base_pr = %+v", stack, stack.BasePR)
	}
	for _, endpoint := range fake.endpoints {
		if strings.Contains(endpoint, "stack") || strings.Contains(endpoint, "/branches/") || strings.Contains(endpoint, "/pulls?") {
			t.Errorf("branch name reached a REST endpoint: %s", endpoint)
		}
	}
	var branchVars int
	for _, vars := range fake.graphVars {
		if vars["branch"] == branch || vars["qualified"] == "refs/heads/"+branch {
			branchVars++
		}
	}
	if branchVars != 3 {
		t.Errorf("branch passed as a GraphQL variable %d times, want 3 (stack, base, final base check)", branchVars)
	}
}

func TestPureRenameIdentityIncludesSourceAndBlob(t *testing.T) {
	fake := newFake(t)
	rename := func(id, from, blob string) {
		fake.rest["repos/acme/widget/commits/"+id] = obj{"sha": id, "parents": []any{obj{"sha": mergeBase1}},
			"files": []any{obj{"filename": "new.go", "status": "renamed", "previous_filename": from, "sha": blob, "changes": 0}}}
	}
	rename(sha("a"), "old.go", "blob1")
	rename(sha("b"), "other.go", "blob1")
	rename(sha("c"), "old.go", "blob2")
	rename(sha("d"), "old.go", "blob1")
	c := &collector{ctx: context.Background(), api: fake, opts: normalizeOptions(Options{}), ref: testRef(), reserved: 1,
		result: Result{Sources: map[string]evidence.Source{}}}
	ids := map[string]string{}
	for _, id := range []string{sha("a"), sha("b"), sha("c"), sha("d")} {
		identity, ok := c.patchIdentity(id)
		if !ok {
			t.Fatalf("patchIdentity(%s) failed", id)
		}
		ids[id] = identity
	}
	if ids[sha("a")] != ids[sha("d")] {
		t.Error("identical renames should share an identity")
	}
	if ids[sha("a")] == ids[sha("b")] || ids[sha("a")] == ids[sha("c")] {
		t.Error("renames from a different source or blob should not share an identity")
	}
}

func TestSideOnlyFileWithoutUsablePatchIsUnknown(t *testing.T) {
	fake := newFake(t, pullAt(headSHA, "main", "open"))
	fake.rest[compareEP(baselineSHA, headSHA)] = compareOf("ahead", baselineSHA, []any{commit(headSHA, baselineSHA, "bin")}, nil)
	fake.rest[compareEP(mainTip, headSHA)] = compareOf("ahead", mergeBase1, nil,
		[]any{file("a.go", "modified", patchTop), file("logo.png", "added", nil)})
	fake.rest[compareEP(mainTip, baselineSHA)] = compareOf("ahead", mergeBase1, nil,
		[]any{file("a.go", "modified", patchTop), file("old.txt", "added", "not a patch")})
	got := Collect(context.Background(), fake, testRef(), Options{Reviewer: "octo", SinceCommit: baselineSHA})
	if got.Complete || got.PRDiff.Verdict != VerdictUnknown || got.Sources[SourcePRDiff].Error == nil {
		t.Fatalf("complete = %v diff = %+v source = %+v", got.Complete, got.PRDiff, got.Sources[SourcePRDiff])
	}
	want := map[string]string{"logo.png": "GitHub omitted the patch (binary or too large)", "old.txt": "patch could not be parsed into hunks"}
	for _, file := range got.PRDiff.Files {
		if reason, ok := want[file.Path]; ok && (file.Status != VerdictUnknown || file.Reason != reason) {
			t.Errorf("%s = %+v, want unknown with %q", file.Path, file, reason)
		}
	}
	if got.PRDiff.Counts[VerdictUnknown] != 2 || got.PRDiff.Counts[FileAdded] != 0 || got.PRDiff.Counts[FileRemoved] != 0 {
		t.Errorf("counts = %v", got.PRDiff.Counts)
	}
}
