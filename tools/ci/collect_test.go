package ci

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"aid/internal/github"
)

type fakeResponse struct {
	value any
	err   error
}

type fakeAPI struct {
	rest      map[string][]fakeResponse
	raw       map[string][]fakeResponse
	restCalls []string
	rawCalls  []string
}

func newFakeAPI() *fakeAPI {
	return &fakeAPI{rest: map[string][]fakeResponse{}, raw: map[string][]fakeResponse{}}
}

func (api *fakeAPI) REST(_ context.Context, endpoint string, out any) error {
	api.restCalls = append(api.restCalls, endpoint)
	responses := api.rest[endpoint]
	if len(responses) == 0 {
		return fmt.Errorf("unexpected REST endpoint %s", endpoint)
	}
	response := responses[0]
	api.rest[endpoint] = responses[1:]
	if response.err != nil {
		return response.err
	}
	encoded, err := json.Marshal(response.value)
	if err != nil {
		return err
	}
	return json.Unmarshal(encoded, out)
}

func (api *fakeAPI) RawREST(_ context.Context, endpoint string) ([]byte, error) {
	api.rawCalls = append(api.rawCalls, endpoint)
	responses := api.raw[endpoint]
	if len(responses) == 0 {
		return nil, fmt.Errorf("unexpected raw endpoint %s", endpoint)
	}
	response := responses[0]
	api.raw[endpoint] = responses[1:]
	data, _ := response.value.([]byte)
	return append([]byte(nil), data...), response.err
}

func (api *fakeAPI) queueREST(endpoint string, values ...any) {
	for _, value := range values {
		response, ok := value.(fakeResponse)
		if !ok {
			response = fakeResponse{value: value}
		}
		api.rest[endpoint] = append(api.rest[endpoint], response)
	}
}

func (api *fakeAPI) queueRaw(endpoint string, data []byte, err error) {
	api.raw[endpoint] = append(api.raw[endpoint], fakeResponse{value: data, err: err})
}

func testRef(attempt int) github.WorkflowRun {
	return github.WorkflowRun{Repository: github.Repository{Host: "github.com", Owner: "octo", Name: "example"}, RunID: 9_007_199_254_740_993, Attempt: attempt}
}

func runJSON(ref github.WorkflowRun, attempt int, conclusion string) map[string]any {
	return map[string]any{
		"id": ref.RunID, "run_attempt": attempt, "workflow_id": int64(8_007_199_254_740_993),
		"name": "CI", "display_title": "CI", "event": "pull_request", "status": "completed",
		"conclusion": conclusion, "head_sha": "abc123", "head_branch": "main",
		"created_at": "2026-10-13T01:00:00Z", "updated_at": "2026-10-13T01:05:00Z",
		"run_started_at": "2026-10-13T01:00:01Z", "repository": map[string]any{"full_name": "octo/example"},
	}
}

func jobJSON(ref github.WorkflowRun, id int64, name, conclusion string, steps []map[string]any) map[string]any {
	return map[string]any{
		"id": id, "run_id": ref.RunID, "name": name, "head_sha": "abc123", "status": "completed",
		"conclusion": conclusion, "started_at": "2026-10-13T01:01:00Z", "completed_at": "2026-10-13T01:02:00Z",
		"check_run_url": fmt.Sprintf("https://api.github.com/repos/octo/example/check-runs/%d", id+1000),
		"steps":         steps, "labels": []string{"ubuntu-latest"},
	}
}

func checkJSON(job map[string]any, count int) map[string]any {
	id := job["id"].(int64)
	return map[string]any{"id": id + 1000, "head_sha": "abc123", "status": job["status"],
		"conclusion": job["conclusion"], "output": map[string]any{"annotations_count": count}}
}

func queueStableRun(api *fakeAPI, ref github.WorkflowRun, attempt int, conclusion string) {
	endpoint := fmt.Sprintf("repos/octo/example/actions/runs/%d/attempts/%d", ref.RunID, attempt)
	run := runJSON(ref, attempt, conclusion)
	api.queueREST(endpoint, run, run)
}

func queueArtifacts(api *fakeAPI, ref github.WorkflowRun) {
	endpoint := fmt.Sprintf("repos/octo/example/actions/runs/%d/artifacts?per_page=100&page=1", ref.RunID)
	api.queueREST(endpoint, map[string]any{"total_count": 0, "artifacts": []any{}})
}

func TestCollectResolvesLatestOnceThenPins(t *testing.T) {
	api := newFakeAPI()
	ref := testRef(0)
	latest := fmt.Sprintf("repos/octo/example/actions/runs/%d", ref.RunID)
	api.queueREST(latest, runJSON(ref, 2, "success"))
	queueStableRun(api, ref, 2, "success")
	jobsEndpoint := fmt.Sprintf("repos/octo/example/actions/runs/%d/attempts/2/jobs?per_page=100&page=1", ref.RunID)
	api.queueREST(jobsEndpoint, map[string]any{"total_count": 0, "jobs": []any{}})
	queueArtifacts(api, ref)

	snapshot, err := Collect(context.Background(), api, ref, Options{})
	if err != nil {
		t.Fatalf("Collect() error: %v", err)
	}
	if !snapshot.Complete || snapshot.Reference.Attempt != 2 {
		t.Errorf("Collect() complete/attempt = %v/%d, want true/2", snapshot.Complete, snapshot.Reference.Attempt)
	}
	if got := countString(api.restCalls, latest); got != 1 {
		t.Errorf("latest endpoint calls = %d, want 1", got)
	}
	for _, call := range api.restCalls[1:] {
		if strings.Contains(call, fmt.Sprintf("actions/runs/%d/jobs", ref.RunID)) {
			t.Errorf("Collect() used mutable latest jobs endpoint %q", call)
		}
	}
}

func TestCollectExplicitAttemptFailureMatrixAndNonblockingAnnotation(t *testing.T) {
	api := newFakeAPI()
	ref := testRef(1)
	queueStableRun(api, ref, 1, "failure")
	lint := jobJSON(ref, 101, "lint (py3.13)", "success", []map[string]any{{"number": 1, "name": "lint", "status": "completed", "conclusion": "success"}})
	tests := jobJSON(ref, 102, "tests (windows)", "failure", []map[string]any{
		{"number": 1, "name": "tests", "status": "completed", "conclusion": "success"},
		{"number": 2, "name": "Post cleanup", "status": "completed", "conclusion": "failure"},
	})
	skipped := jobJSON(ref, 103, "optional", "skipped", nil)
	delete(skipped, "started_at")
	delete(skipped, "completed_at")
	jobsEndpoint := fmt.Sprintf("repos/octo/example/actions/runs/%d/attempts/1/jobs?per_page=100&page=1", ref.RunID)
	api.queueREST(jobsEndpoint, map[string]any{"total_count": 3, "jobs": []any{lint, tests, skipped}})
	for _, item := range []struct {
		job   map[string]any
		count int
	}{{lint, 1}, {tests, 0}, {skipped, 0}} {
		id := item.job["id"].(int64) + 1000
		api.queueREST(fmt.Sprintf("repos/octo/example/check-runs/%d", id), checkJSON(item.job, item.count))
		if item.count == 1 {
			api.queueREST(fmt.Sprintf("repos/octo/example/check-runs/%d/annotations?per_page=100&page=1", id), []any{
				map[string]any{"path": "lint.py", "start_line": 7, "start_column": 2, "end_column": 9,
					"annotation_level": "warning", "title": "Expected lint", "message": "token=secret-value", "raw_details": "Authorization: token opaque-secret"},
			})
		}
	}
	api.queueRaw("repos/octo/example/actions/jobs/102/logs", []byte("setup ok\nPost cleanup\ncommand failed\n"), nil)
	queueArtifacts(api, ref)

	snapshot, err := Collect(context.Background(), api, ref, Options{})
	if err != nil {
		t.Fatalf("Collect() error: %v", err)
	}
	if !snapshot.Complete {
		t.Errorf("Collect() complete = false; sources=%+v", snapshot.Sources)
	}
	if len(snapshot.Annotations) != 1 || strings.Contains(snapshot.Annotations[0].Message+snapshot.Annotations[0].RawDetails, "secret") {
		t.Errorf("annotations = %+v, want one fully redacted nonblocking lint annotation", snapshot.Annotations)
	}
	if got := snapshot.Jobs[1].FailedStepNames; len(got) != 1 || got[0] != "Post cleanup" {
		t.Errorf("failed step names = %v, want Post cleanup", got)
	}
	if snapshot.Jobs[1].Log.State != "available" || len(snapshot.Jobs[1].Log.Excerpts) == 0 {
		t.Errorf("failure log = %+v, want available selected excerpt", snapshot.Jobs[1].Log)
	}
	if len(api.rawCalls) != 1 || !strings.HasSuffix(api.rawCalls[0], "/actions/jobs/102/logs") {
		t.Errorf("raw calls = %v, want only failed matrix job", api.rawCalls)
	}
	if snapshot.Jobs[0].Log.State != "skipped" || snapshot.Jobs[2].Log.State != "skipped" {
		t.Errorf("nonproblem job log states = %q/%q, want skipped/skipped", snapshot.Jobs[0].Log.State, snapshot.Jobs[2].Log.State)
	}
	if snapshot.Reference.RunID != 9_007_199_254_740_993 || snapshot.Run.WorkflowID != 8_007_199_254_740_993 {
		t.Errorf("64-bit identities were not preserved: %+v", snapshot.Run)
	}
}

func TestCollectRejectsJobOutsideSelectedAttempt(t *testing.T) {
	api := newFakeAPI()
	ref := testRef(1)
	ref.JobID = 999
	queueStableRun(api, ref, 1, "success")
	job := jobJSON(ref, 101, "build", "success", nil)
	endpoint := fmt.Sprintf("repos/octo/example/actions/runs/%d/attempts/1/jobs?per_page=100&page=1", ref.RunID)
	api.queueREST(endpoint, map[string]any{"total_count": 1, "jobs": []any{job}})

	_, err := Collect(context.Background(), api, ref, Options{})
	if !errors.Is(err, ErrJobNotInAttempt) {
		t.Fatalf("Collect() error = %v, want ErrJobNotInAttempt", err)
	}
	if len(api.rawCalls) != 0 {
		t.Errorf("raw calls = %v, want no job log request", api.rawCalls)
	}
}

func TestCollectFailedRunWithNoJobsIsExplicitlyIncomplete(t *testing.T) {
	api := newFakeAPI()
	ref := testRef(1)
	queueStableRun(api, ref, 1, "failure")
	endpoint := fmt.Sprintf("repos/octo/example/actions/runs/%d/attempts/1/jobs?per_page=100&page=1", ref.RunID)
	api.queueREST(endpoint, map[string]any{"total_count": 0, "jobs": []any{}})
	queueArtifacts(api, ref)

	snapshot, err := Collect(context.Background(), api, ref, Options{})
	if err != nil {
		t.Fatalf("Collect() error: %v", err)
	}
	if snapshot.Complete || snapshot.Sources[SourceJobs].Error == nil || snapshot.Sources[SourceLogs].Error == nil {
		t.Errorf("failed run without jobs = complete:%v jobs:%+v logs:%+v", snapshot.Complete, snapshot.Sources[SourceJobs], snapshot.Sources[SourceLogs])
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("json.Marshal(snapshot) error: %v", err)
	}
	for _, field := range []string{`"jobs":[]`, `"annotations":[]`, `"items":[]`} {
		if !strings.Contains(string(encoded), field) {
			t.Errorf("json.Marshal(snapshot) missing non-null array %s: %s", field, encoded)
		}
	}
}

func TestCollectAnnotationPagesUseCountNotContentIdentity(t *testing.T) {
	api := newFakeAPI()
	ref := testRef(1)
	queueStableRun(api, ref, 1, "failure")
	job := jobJSON(ref, 101, "test", "failure", []map[string]any{{"number": 1, "name": "test", "status": "completed", "conclusion": "failure"}})
	jobsEndpoint := fmt.Sprintf("repos/octo/example/actions/runs/%d/attempts/1/jobs?per_page=1&page=1", ref.RunID)
	api.queueREST(jobsEndpoint, map[string]any{"total_count": 1, "jobs": []any{job}})
	api.queueREST("repos/octo/example/check-runs/1101", checkJSON(job, 2))
	annotation := map[string]any{"path": "same.go", "start_line": 1, "annotation_level": "failure", "message": "same content"}
	api.queueREST("repos/octo/example/check-runs/1101/annotations?per_page=1&page=1", []any{annotation})
	api.queueREST("repos/octo/example/check-runs/1101/annotations?per_page=1&page=2", []any{annotation})
	api.queueRaw("repos/octo/example/actions/jobs/101/logs", []byte{}, nil)
	api.queueREST(fmt.Sprintf("repos/octo/example/actions/runs/%d/artifacts?per_page=1&page=1", ref.RunID), map[string]any{"total_count": 0, "artifacts": []any{}})

	snapshot, err := Collect(context.Background(), api, ref, Options{PerPage: 1})
	if err != nil {
		t.Fatalf("Collect() error: %v", err)
	}
	if len(snapshot.Annotations) != 2 || !snapshot.Sources[SourceAnnotations].Complete {
		t.Errorf("annotations/count source = %d/%+v, want 2/complete", len(snapshot.Annotations), snapshot.Sources[SourceAnnotations])
	}
	if snapshot.Annotations[0].CaptureOrdinal == snapshot.Annotations[1].CaptureOrdinal || snapshot.Annotations[1].SourcePage != 2 {
		t.Errorf("annotation local provenance = %+v", snapshot.Annotations)
	}
}

func TestCollectFocusedSuccessRetrievesEmptyLog(t *testing.T) {
	api := newFakeAPI()
	ref := testRef(1)
	ref.JobID = 101
	queueStableRun(api, ref, 1, "success")
	job := jobJSON(ref, 101, "build", "success", nil)
	jobsEndpoint := fmt.Sprintf("repos/octo/example/actions/runs/%d/attempts/1/jobs?per_page=100&page=1", ref.RunID)
	api.queueREST(jobsEndpoint, map[string]any{"total_count": 1, "jobs": []any{job}})
	api.queueREST("repos/octo/example/check-runs/1101", checkJSON(job, 0))
	api.queueRaw("repos/octo/example/actions/jobs/101/logs", []byte{}, nil)
	queueArtifacts(api, ref)

	snapshot, err := Collect(context.Background(), api, ref, Options{})
	if err != nil {
		t.Fatalf("Collect() error: %v", err)
	}
	if !snapshot.Complete || len(snapshot.Jobs) != 1 || snapshot.Jobs[0].Log.State != "empty" {
		t.Errorf("focused success snapshot = complete:%v jobs:%+v", snapshot.Complete, snapshot.Jobs)
	}
	if len(api.rawCalls) != 1 {
		t.Errorf("focused success raw calls = %v, want one", api.rawCalls)
	}
}

func TestCollectAnnotationPermissionAndTruncatedLogStates(t *testing.T) {
	t.Run("annotation_permission", func(t *testing.T) {
		api := newFakeAPI()
		ref := testRef(1)
		queueStableRun(api, ref, 1, "failure")
		job := jobJSON(ref, 101, "test", "failure", []map[string]any{{"number": 1, "name": "test", "status": "completed", "conclusion": "failure"}})
		endpoint := fmt.Sprintf("repos/octo/example/actions/runs/%d/attempts/1/jobs?per_page=100&page=1", ref.RunID)
		api.queueREST(endpoint, map[string]any{"total_count": 1, "jobs": []any{job}})
		api.queueREST("repos/octo/example/check-runs/1101", fakeResponse{err: &github.Error{Kind: github.KindPermissionDenied, Message: "safe"}})
		api.queueRaw("repos/octo/example/actions/jobs/101/logs", []byte{}, nil)
		queueArtifacts(api, ref)
		snapshot, err := Collect(context.Background(), api, ref, Options{})
		if err != nil {
			t.Fatalf("Collect() error: %v", err)
		}
		failure := snapshot.Sources[SourceAnnotations].Error
		if snapshot.Complete || failure == nil || failure.Code != "permission_denied" {
			t.Errorf("permission snapshot = complete:%v source:%+v", snapshot.Complete, snapshot.Sources[SourceAnnotations])
		}
	})

	t.Run("bounded_prefix", func(t *testing.T) {
		api := newFakeAPI()
		ref := testRef(1)
		queueStableRun(api, ref, 1, "failure")
		job := jobJSON(ref, 101, "test", "failure", []map[string]any{{"number": 1, "name": "test", "status": "completed", "conclusion": "failure"}})
		endpoint := fmt.Sprintf("repos/octo/example/actions/runs/%d/attempts/1/jobs?per_page=100&page=1", ref.RunID)
		api.queueREST(endpoint, map[string]any{"total_count": 1, "jobs": []any{job}})
		api.queueREST("repos/octo/example/check-runs/1101", checkJSON(job, 0))
		prefix := []byte("error complete line\nAuthorization: Bearer incomplete-secret")
		api.queueRaw("repos/octo/example/actions/jobs/101/logs", prefix, &github.Error{Kind: github.KindResponseLimit, Message: "safe"})
		queueArtifacts(api, ref)
		snapshot, err := Collect(context.Background(), api, ref, Options{})
		if err != nil {
			t.Fatalf("Collect() error: %v", err)
		}
		log := snapshot.Jobs[0].Log
		if log.State != "truncated" || log.RetrievalComplete || log.ExcerptSelectionComplete || snapshot.Sources[SourceLogs].Complete {
			t.Errorf("bounded log state = %+v source=%+v", log, snapshot.Sources[SourceLogs])
		}
		if strings.Contains(excerptText(log.Excerpts), "incomplete-secret") {
			t.Errorf("bounded log returned incomplete final line: %+v", log.Excerpts)
		}
	})
}

func TestCollectOngoingRunKeepsIdentitySourceComplete(t *testing.T) {
	api := newFakeAPI()
	ref := testRef(1)
	run := runJSON(ref, 1, "")
	run["status"] = "in_progress"
	endpoint := fmt.Sprintf("repos/octo/example/actions/runs/%d/attempts/1", ref.RunID)
	api.queueREST(endpoint, run, run)
	api.queueREST(endpoint+"/jobs?per_page=100&page=1", map[string]any{"total_count": 0, "jobs": []any{}})
	queueArtifacts(api, ref)
	snapshot, err := Collect(context.Background(), api, ref, Options{})
	if err != nil {
		t.Fatalf("Collect() error: %v", err)
	}
	if snapshot.Complete || !snapshot.Sources[SourceRun].Complete || snapshot.Sources[SourceFreshness].Error == nil || snapshot.Sources[SourceFreshness].Error.Code != "run_in_progress" {
		t.Errorf("ongoing state = complete:%v run:%+v freshness:%+v", snapshot.Complete, snapshot.Sources[SourceRun], snapshot.Sources[SourceFreshness])
	}
}

func countString(values []string, target string) int {
	count := 0
	for _, value := range values {
		if value == target {
			count++
		}
	}
	return count
}
