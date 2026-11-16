package reviewqueue

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"aid/internal/github"
)

type obj = map[string]any

var headSHA = strings.Repeat("2", 40)

// fakeAPI serves the search query and the pr-delta endpoints. Pull requests
// listed in deltaOK return a complete, unchanged comparison; any other pull
// request metadata read fails.
type fakeAPI struct {
	t          *testing.T
	mu         sync.Mutex
	user       any
	pages      []any
	searchErr  error
	searchVars []map[string]any
	deltaOK    map[string]bool
	rest       []string
}

func (fake *fakeAPI) REST(_ context.Context, endpoint string, out any) error {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.rest = append(fake.rest, endpoint)
	if endpoint == "user" {
		if err, ok := fake.user.(error); ok {
			return err
		}
		return copyJSON(out, fake.user)
	}
	switch {
	case strings.HasSuffix(endpoint, "/check-runs?filter=latest&per_page=100&page=1"):
		return copyJSON(out, obj{"total_count": 2, "check_runs": []any{
			obj{"name": "test", "status": "completed", "conclusion": "success"},
			obj{"name": "lint", "status": "completed", "conclusion": "success"}}})
	case strings.HasSuffix(endpoint, "/status?per_page=100&page=1"):
		return copyJSON(out, obj{"total_count": 0, "statuses": []any{}})
	case strings.Contains(endpoint, "/compare/"+baseTip+"..."):
		return copyJSON(out, obj{"status": "ahead", "ahead_by": 1, "total_commits": 1,
			"merge_base_commit": obj{"sha": strings.Repeat("3", 40)}, "commits": []any{}, "files": []any{}})
	}
	owner, name, number, suffix := splitPullEndpoint(endpoint)
	if number == "" || !fake.deltaOK[owner+"/"+name+"#"+number] {
		return &github.Error{Kind: github.KindPermissionDenied, Message: "GitHub denied the request"}
	}
	switch suffix {
	case "":
		return copyJSON(out, obj{"number": atoi(number), "state": "open", "mergeable_state": "clean",
			"head": obj{"ref": "topic", "sha": headSHA, "repo": obj{"full_name": owner + "/" + name}},
			"base": obj{"ref": "main", "sha": strings.Repeat("e", 40), "repo": obj{"full_name": owner + "/" + name, "default_branch": "main"}}})
	case "/reviews?per_page=100&page=1":
		return copyJSON(out, []any{obj{"id": 11, "user": obj{"login": "octo"}, "state": "APPROVED", "commit_id": headSHA,
			"submitted_at": "2026-11-01T00:00:00Z"}})
	}
	fake.t.Errorf("unexpected REST endpoint %q", endpoint)
	return &github.Error{Kind: github.KindNotFound, Message: "not found"}
}

func splitPullEndpoint(endpoint string) (owner, name, number, suffix string) {
	parts := strings.SplitN(strings.TrimPrefix(endpoint, "repos/"), "/", 4)
	if len(parts) < 4 || parts[2] != "pulls" {
		return "", "", "", ""
	}
	number, after, _ := strings.Cut(parts[3], "/")
	if after != "" {
		after = "/" + after
	}
	return parts[0], parts[1], number, after
}

func atoi(value string) int {
	number, _ := strconv.Atoi(value)
	return number
}

func (fake *fakeAPI) GraphQL(_ context.Context, query string, variables map[string]any, out any) error {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if strings.Contains(query, "search(") {
		fake.searchVars = append(fake.searchVars, variables)
		if fake.searchErr != nil {
			return fake.searchErr
		}
		page := len(fake.searchVars) - 1
		if page >= len(fake.pages) {
			fake.t.Errorf("unexpected search page %d", page)
			return &github.Error{Kind: github.KindInvalidResponse, Message: "invalid"}
		}
		return copyJSON(out, obj{"search": fake.pages[page]})
	}
	switch {
	case strings.Contains(query, "target { oid }"):
		return copyJSON(out, obj{"repository": obj{"ref": obj{"target": obj{"oid": baseTip}}}})
	case strings.Contains(query, "pullRequests(headRefName"):
		return copyJSON(out, obj{"repository": obj{"ref": obj{"name": "main"}, "pullRequests": obj{"totalCount": 0, "nodes": []any{}}}})
	}
	threads := obj{"pageInfo": obj{"hasNextPage": false}, "nodes": []any{}}
	return copyJSON(out, obj{"repository": obj{"pullRequest": obj{"reviewDecision": "APPROVED", "reviewThreads": threads}}})
}

// baseTip is the resolved tip of the PRs' base branch.
var baseTip = strings.Repeat("b", 40)

func copyJSON(out, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, out)
}

func pullNode(repo string, number int, reviews int, requested ...obj) obj {
	nodes := []any{}
	for _, reviewer := range requested {
		nodes = append(nodes, obj{"requestedReviewer": reviewer})
	}
	return obj{"__typename": "PullRequest", "number": number, "title": "Change  " + repo, "isDraft": false,
		"url": "https://github.com/" + repo + "/pull/" + strconv.Itoa(number), "updatedAt": "2026-11-16T00:00:00Z",
		"author": obj{"login": "author"}, "repository": obj{"nameWithOwner": repo},
		"reviewRequests": obj{"totalCount": len(nodes), "nodes": nodes},
		"reviews":        obj{"totalCount": reviews}}
}

func userRequest(login string) obj { return obj{"__typename": "User", "login": login} }
func teamRequest(slug string) obj  { return obj{"__typename": "Team", "slug": slug} }

func page(issueCount int, hasNext bool, nodes ...any) obj {
	return obj{"issueCount": issueCount, "pageInfo": obj{"hasNextPage": hasNext, "endCursor": "cursor"}, "nodes": nodes}
}

func configFile(t *testing.T, extra string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yml")
	content := "version: 1\nglobal:\n  timeout_seconds: 10\n  output: json\ntools:\n  review-queue:\n    hostname: github.com\n" + extra
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func runCLI(t *testing.T, fake *fakeAPI, args ...string) (int, map[string]any, string) {
	t.Helper()
	var out, errs bytes.Buffer
	args = append([]string{"--config", configFile(t, ""), "--workspace", t.TempDir()}, args...)
	code := run(args, &out, &errs, func(github.Client) API {
		if fake == nil {
			t.Error("unexpected API construction")
		}
		return fake
	})
	var result map[string]any
	if out.Len() > 0 && json.Unmarshal(out.Bytes(), &result) != nil {
		return code, nil, out.String()
	}
	return code, result, errs.String()
}

func TestClassifiesNewAndReReviewWithDelta(t *testing.T) {
	fake := &fakeAPI{t: t, deltaOK: map[string]bool{"acme/widget#7": true}, pages: []any{page(3, false,
		pullNode("acme/widget", 7, 1, userRequest("Octo")),
		pullNode("acme/widget", 8, 0, teamRequest("platform")),
		pullNode("acme/gadget", 9, 0, teamRequest("a"), teamRequest("b")),
	)}}
	result := Collect(context.Background(), fake, Options{Reviewer: "octo", Owners: []string{"acme"}, ScopeSource: ScopeFlag})
	if !result.Complete || result.Summary != (Summary{Total: 3, New: 2, ReReview: 1, ReReviewUnchanged: 1}) {
		t.Fatalf("complete %v summary %+v sources %+v", result.Complete, result.Summary, result.Sources)
	}
	reReview, team, multi := result.Items[0], result.Items[1], result.Items[2]
	if reReview.Kind != KindReReview || reReview.RequestedVia != "user" || reReview.Delta == nil {
		t.Fatalf("re-review item = %+v", reReview)
	}
	if delta := reReview.Delta; delta.Verdict != "unchanged" || delta.HeadRelation != "identical" || !delta.Complete ||
		delta.CI.Success != 2 || delta.BaselineState != "APPROVED" {
		t.Errorf("delta = %+v", delta)
	}
	if team.Kind != KindNew || team.RequestedVia != "team:platform" || team.Delta != nil || team.Title != "Change acme/widget" {
		t.Errorf("team item = %+v", team)
	}
	if multi.RequestedVia != "unknown" {
		t.Errorf("multi-team requested_via = %q", multi.RequestedVia)
	}
	if query := fake.searchVars[0]["q"]; query != "is:pr is:open archived:false review-requested:octo user:acme sort:updated-desc" ||
		fake.searchVars[0]["reviewer"] != "octo" {
		t.Errorf("search vars = %v", fake.searchVars[0])
	}
}

func TestPerItemDeltaFailureMarksIncomplete(t *testing.T) {
	fake := &fakeAPI{t: t, deltaOK: map[string]bool{"acme/widget#7": true}, pages: []any{page(2, false,
		pullNode("acme/widget", 7, 2, userRequest("octo")),
		pullNode("acme/gadget", 9, 1, userRequest("octo")),
	)}}
	code, result, stderr := runCLI(t, fake, "--reviewer", "octo", "--owner", "acme", "--json")
	if code != 3 || result["complete"] != false {
		t.Fatalf("exit %d complete %v stderr %s", code, result["complete"], stderr)
	}
	items := result["items"].([]any)
	good := items[0].(map[string]any)["delta"].(map[string]any)
	bad := items[1].(map[string]any)["delta"].(map[string]any)
	if good["complete"] != true || bad["complete"] != false || bad["verdict"] != "unknown" {
		t.Errorf("deltas good %v bad %v", good, bad)
	}
	sources := result["sources"].(map[string]any)
	if delta := sources["delta"].(map[string]any); delta["status"] != "incomplete" || delta["incomplete"] != 1.0 || delta["computed"] != 2.0 {
		t.Errorf("delta source = %v", delta)
	}
}

func TestEmptyQueueExitsZero(t *testing.T) {
	fake := &fakeAPI{t: t, user: obj{"login": "octo"}, pages: []any{page(0, false)}}
	code, result, stderr := runCLI(t, fake, "--owner", "other-org", "--json")
	if code != 0 || result["complete"] != true || result["reviewer"] != "octo" || result["reviewer_source"] != "authenticated_user" {
		t.Fatalf("exit %d result %v stderr %s", code, result, stderr)
	}
	if summary := result["summary"].(map[string]any); summary["total"] != 0.0 {
		t.Errorf("summary = %v", summary)
	}
	if items := result["items"].([]any); len(items) != 0 {
		t.Errorf("items = %v", items)
	}
}

func TestSearchFailureExitsThreeWithEmptyItems(t *testing.T) {
	fake := &fakeAPI{t: t, searchErr: &github.Error{Kind: github.KindPermissionDenied, Message: "GitHub denied the request"}}
	code, result, _ := runCLI(t, fake, "--reviewer", "octo", "--owner", "acme", "--json")
	if code != 3 || result["complete"] != false {
		t.Fatalf("exit %d result %v", code, result)
	}
	search := result["sources"].(map[string]any)["search"].(map[string]any)
	if search["status"] != "permission_denied" || len(result["items"].([]any)) != 0 {
		t.Errorf("search = %v items = %v", search, result["items"])
	}
}

func TestReviewerLookupFailureSkipsSearch(t *testing.T) {
	fake := &fakeAPI{t: t, user: &github.Error{Kind: github.KindPermissionDenied, Message: "denied"}}
	code, result, _ := runCLI(t, fake, "--owner", "acme", "--json")
	sources := result["sources"].(map[string]any)
	if code != 3 || sources["search"].(map[string]any)["status"] != "dependency_failed" || len(fake.searchVars) != 0 {
		t.Errorf("exit %d sources %v", code, sources)
	}
}

func TestNoDeltaMakesOneSearchCall(t *testing.T) {
	fake := &fakeAPI{t: t, pages: []any{page(1, false, pullNode("acme/widget", 7, 1, userRequest("octo")))}}
	code, result, _ := runCLI(t, fake, "--reviewer", "octo", "--owner", "acme", "--no-delta", "--json")
	if code != 0 || len(fake.rest) != 0 || len(fake.searchVars) != 1 {
		t.Fatalf("exit %d rest %v searches %d", code, fake.rest, len(fake.searchVars))
	}
	item := result["items"].([]any)[0].(map[string]any)
	delta := result["sources"].(map[string]any)["delta"].(map[string]any)
	if item["kind"] != "re_review" || item["delta"] != nil || delta["status"] != "skipped" || delta["skipped"] != 1.0 {
		t.Errorf("item %v delta source %v", item, delta)
	}
}

func TestMaxPRsTruncates(t *testing.T) {
	fake := &fakeAPI{t: t, pages: []any{page(5, true,
		pullNode("acme/a", 1, 0, userRequest("octo")), pullNode("acme/b", 2, 0, userRequest("octo")))}}
	code, result, _ := runCLI(t, fake, "--reviewer", "octo", "--owner", "acme", "--max-prs", "2", "--json")
	if code != 3 || result["truncated"] != true || len(result["items"].([]any)) != 2 {
		t.Fatalf("exit %d truncated %v items %d", code, result["truncated"], len(result["items"].([]any)))
	}
	if first := fake.searchVars[0]["first"]; first != 2.0 && first != 2 {
		t.Errorf("first = %v", first)
	}
}

func TestSearchPaginatesUntilBound(t *testing.T) {
	first := make([]any, 0, 100)
	for number := 1; number <= 100; number++ {
		first = append(first, pullNode("acme/a", number, 0, userRequest("octo")))
	}
	fake := &fakeAPI{t: t, pages: []any{page(101, true, first...), page(101, false, pullNode("acme/a", 101, 0, userRequest("octo")))}}
	result := Collect(context.Background(), fake, Options{Reviewer: "octo", MaxPRs: 150, NoDelta: true})
	if !result.Complete || len(result.Items) != 101 || len(fake.searchVars) != 2 || fake.searchVars[1]["cursor"] != "cursor" {
		t.Errorf("complete %v items %d searches %d", result.Complete, len(result.Items), len(fake.searchVars))
	}
	if result.Scope.Source != ScopeAll || !strings.Contains(strings.Join(result.Warnings, ","), "scope_all_repositories") {
		t.Errorf("scope = %+v warnings %v", result.Scope, result.Warnings)
	}
}

func TestUnknownReviewHistoryIsIncomplete(t *testing.T) {
	node := pullNode("acme/a", 1, 0, userRequest("octo"))
	node["reviews"] = nil
	fake := &fakeAPI{t: t, pages: []any{page(1, false, node)}}
	result := Collect(context.Background(), fake, Options{Reviewer: "octo", Owners: []string{"acme"}})
	if result.Complete || result.Summary.Unknown != 1 || result.Items[0].Kind != KindUnknown {
		t.Errorf("complete %v summary %+v", result.Complete, result.Summary)
	}
}

func TestCLIUsageAndDryRun(t *testing.T) {
	for _, args := range [][]string{
		{"--owner", "bad owner"}, {"--reviewer", "bad login"}, {"--max-prs", "0"}, {"--json", "--text"}, {"positional"},
		{"--workspace", filepath.Join(t.TempDir(), "missing")},
	} {
		if code, _, _ := runCLI(t, nil, args...); code != 2 {
			t.Errorf("%v exit = %d", args, code)
		}
	}
	workspace := t.TempDir()
	makeRepo(t, filepath.Join(workspace, "one"), "git@github.com:acme/one.git")
	var out, errs bytes.Buffer
	code := run([]string{"--config", configFile(t, ""), "--workspace", workspace, "--dry-run"}, &out, &errs, func(github.Client) API {
		t.Error("dry-run constructed an API")
		return nil
	})
	var preview struct {
		Scope Scope `json:"scope"`
	}
	if code != 0 || json.Unmarshal(out.Bytes(), &preview) != nil || preview.Scope.Source != ScopeWorkspaceChildren ||
		preview.Scope.Query != "is:pr is:open archived:false review-requested:@me user:acme sort:updated-desc" {
		t.Errorf("dry-run exit %d out %s", code, &out)
	}
	out.Reset()
	code = run([]string{"--config", configFile(t, "    owners: [configured]\n"), "--workspace", workspace, "--dry-run"}, &out, &errs, nil)
	if code != 0 || json.Unmarshal(out.Bytes(), &preview) != nil || preview.Scope.Source != ScopeConfig {
		t.Errorf("config owners dry-run exit %d out %s", code, &out)
	}
}

func TestCLIVersion(t *testing.T) {
	var out, errs bytes.Buffer
	if code := run([]string{"--version", "--json"}, &out, &errs, nil); code != 0 {
		t.Fatalf("exit %d", code)
	}
	var info map[string]string
	if json.Unmarshal(out.Bytes(), &info) != nil || info["command"] != "AID.review-queue" || info["tool_version"] != "0.1.0" {
		t.Errorf("version = %s", &out)
	}
}

func TestTextOutputOneLinePerPR(t *testing.T) {
	result := finish(Result{Reviewer: "octo", Scope: Scope{Owners: []string{"FarmGPU"}, Source: ScopeWorkspaceChildren},
		Sources: Sources{Search: SourceStatus{Status: StatusOK}, Delta: DeltaSource{Status: StatusOK}},
		Items: []Item{
			{Repo: "FarmGPU/fgpu_ansible", Number: 498, Kind: KindReReview, Delta: &Delta{HeadRelation: "rebased", Verdict: "unchanged",
				CI: DeltaCI{Success: 8}, Complete: true}},
			{Repo: "FarmGPU/x", Number: 2, Kind: KindNew, Author: "dev", Title: "Add thing"},
		}})
	var out bytes.Buffer
	if err := writeText(&out, result); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"FarmGPU/fgpu_ansible#498 re-review rebased, diff unchanged, 0 open threads, CI 8/8\n",
		"FarmGPU/x#2 new by dev: Add thing\n",
		"2 waiting: 1 new, 1 re-review (1 unchanged)",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("text missing %q:\n%s", want, &out)
		}
	}
}

func TestRenderBudgetTrimsItems(t *testing.T) {
	result := Result{SchemaVersion: 1, Complete: true, Items: []Item{}, Warnings: []string{}}
	for number := range 200 {
		result.Items = append(result.Items, Item{Repo: "acme/widget", Number: number + 1, Title: strings.Repeat("t", 100), Kind: KindNew})
	}
	data, rendered, err := render(result, 4096)
	if err != nil || len(data) > 4096 || rendered.Complete || !rendered.Output.Truncated || rendered.Output.OmittedItems == 0 {
		t.Errorf("err %v bytes %d complete %v output %+v", err, len(data), rendered.Complete, rendered.Output)
	}
}

func TestSearchPageBound(t *testing.T) {
	var pages []any
	for range 5 {
		pages = append(pages, page(10, true, obj{"__typename": "Issue"}))
	}
	fake := &fakeAPI{t: t, pages: pages}
	result := Collect(context.Background(), fake, Options{Reviewer: "octo", MaxPRs: 5, NoDelta: true})
	if result.Complete || result.Sources.Search.Status != "page_limit" || len(fake.searchVars) != 3 {
		t.Errorf("complete %v search %+v pages %d", result.Complete, result.Sources.Search, len(fake.searchVars))
	}
}
