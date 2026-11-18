package pr

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"aid/internal/evidence"
)

func renderFixture() Snapshot {
	sources := make(map[string]evidence.Source)
	for _, name := range []string{SourcePullRequest, SourceFiles, SourceChecks, SourceStatuses, SourceReviewThreads, SourceIssueComments, SourceReviews, SourceFreshness} {
		sources[name] = evidence.Source{Complete: true, CapturedAt: "2026-10-13T00:00:00Z"}
	}
	return Snapshot{SchemaVersion: 1, Complete: true, Sources: sources, PullRequest: Metadata{Title: "Example", Body: "A short description", Head: Branch{SHA: "head"}, Author: Actor{Login: "author", Type: "User"}}}
}

func readRendered(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var result map[string]any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&result); err != nil {
		t.Fatalf("decode rendered JSON: %v", err)
	}
	return result
}

func TestRenderCompleteEvidencePreservesLargeIdentity(t *testing.T) {
	snapshot := renderFixture()
	snapshot.IssueComments = []Comment{{ID: 9007199254740993, Body: "comment"}}
	data, complete, err := render(snapshot, 128<<10, 4000)
	if err != nil || !complete {
		t.Fatalf("render(complete) = complete %v, error %v", complete, err)
	}
	result := readRendered(t, data)
	comment := result["issue_comments"].([]any)[0].(map[string]any)
	if got := comment["id"].(json.Number).String(); got != "9007199254740993" {
		t.Errorf("rendered identity = %s, want 9007199254740993", got)
	}
	if result["output"].(map[string]any)["truncated"] != false {
		t.Error("complete small snapshot unexpectedly truncated")
	}
}

func TestRenderTextLimitMarksNestedSourceIncomplete(t *testing.T) {
	snapshot := renderFixture()
	snapshot.ReviewThreads = []ReviewThread{{ID: "T1", CommentsSource: evidence.Source{Complete: true}, Comments: []Comment{{NodeID: "C1", Body: "abc世界def"}}}}
	data, complete, err := render(snapshot, 128<<10, 4)
	if err != nil || complete {
		t.Fatalf("render(text limit) = complete %v, error %v", complete, err)
	}
	result := readRendered(t, data)
	thread := result["review_threads"].([]any)[0].(map[string]any)
	comment := thread["comments"].([]any)[0].(map[string]any)
	if comment["body"] != "abc世" || comment["body_truncated"] != true || comment["body_chars"].(json.Number).String() != "8" {
		t.Errorf("truncated comment = %#v, want four-character prefix and original length 8", comment)
	}
	if thread["comments_source"].(map[string]any)["complete"] != false || result["sources"].(map[string]any)[SourceReviewThreads].(map[string]any)["complete"] != false {
		t.Error("text truncation did not mark both thread and aggregate source incomplete")
	}
}

func TestRenderBudgetReturnsValidPartialEvidence(t *testing.T) {
	snapshot := renderFixture()
	for index := 0; index < 120; index++ {
		snapshot.Files = append(snapshot.Files, File{Filename: strings.Repeat("a", 80), Status: "modified"})
	}
	data, complete, err := render(snapshot, 8192, 4000)
	if err != nil || complete || len(data) > 8192 {
		t.Fatalf("render(budget) = %d bytes, complete %v, error %v", len(data), complete, err)
	}
	result := readRendered(t, data)
	files := result["files"].([]any)
	omitted := result["output"].(map[string]any)["omitted_items"].(map[string]any)[SourceFiles].(json.Number)
	count, _ := omitted.Int64()
	if len(files) == 0 || len(files)+int(count) != 120 {
		t.Errorf("retained %d, omitted %d; want nonempty prefix totaling 120", len(files), count)
	}
	if result["sources"].(map[string]any)[SourceFiles].(map[string]any)["complete"] != false {
		t.Error("file omission remained complete")
	}
	again, _, _ := render(snapshot, 8192, 4000)
	if !bytes.Equal(data, again) {
		t.Error("output differs for identical input and limits")
	}
}

func TestRenderTrimsCommentsBeforeDroppingThread(t *testing.T) {
	snapshot := renderFixture()
	thread := ReviewThread{ID: "T1", CommentsSource: evidence.Source{Complete: true}}
	for index := 0; index < 40; index++ {
		thread.Comments = append(thread.Comments, Comment{ID: int64(index + 1), Body: strings.Repeat("body", 100)})
	}
	snapshot.ReviewThreads = []ReviewThread{thread}
	data, complete, err := render(snapshot, 8192, 4000)
	if err != nil || complete || len(data) > 8192 {
		t.Fatalf("render(nested budget) = bytes %d, complete %v, error %v", len(data), complete, err)
	}
	result := readRendered(t, data)
	threads := result["review_threads"].([]any)
	if len(threads) != 1 {
		t.Fatalf("retained threads = %d, want 1", len(threads))
	}
	got := threads[0].(map[string]any)
	if len(got["comments"].([]any)) >= 40 || got["comments_source"].(map[string]any)["complete"] != false {
		t.Error("nested comments were not bounded and marked incomplete")
	}
}

func TestBodyReclippingKeepsOriginalLength(t *testing.T) {
	value := map[string]any{"body": "abcdefgh"}
	clipBodies(value, 4)
	clipBodies(value, 0)
	if value["body_chars"] != 8 || value["body"] != "" || countTruncatedBodies(value) != 1 {
		t.Fatalf("reclipped body = %#v", value)
	}
}

func TestTextSummaryEscapesTerminalControlCharacters(t *testing.T) {
	snapshot := renderFixture()
	snapshot.PullRequest.Title = "title\x1b[2J\nforged row"
	data, _, err := render(snapshot, 128<<10, 4000)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := writeText(&output, data); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), "\x1b") || strings.Contains(output.String(), "\nforged row") {
		t.Fatalf("unsafe terminal control output: %q", output.String())
	}
}

func TestRenderBudgetsFindingsBeforeCollectedEvidence(t *testing.T) {
	snapshot := findingFixture()
	template := snapshot.ReviewThreads[0]
	snapshot.ReviewThreads = nil
	for i := range 32 {
		thread := template
		thread.ID = fmt.Sprintf("PRRT_unique_thread_%03d", i)
		thread.Path = fmt.Sprintf("path-%02d.go", i)
		snapshot.ReviewThreads = append(snapshot.ReviewThreads, thread)
	}
	// Measure the space for all findings without peer IDs, and the space
	// for just the collected evidence, independent of field-name lengths.
	withoutPeers, _, err := render(snapshot, 1<<20, 4000)
	if err != nil {
		t.Fatal(err)
	}
	for i := range snapshot.ReviewThreads {
		snapshot.ReviewThreads[i].Path = "path-00.go"
		snapshot.ReviewThreads[i].Resolved = true
	}
	withoutOpenThreads, _, err := render(snapshot, 1<<20, 4000)
	if err != nil {
		t.Fatal(err)
	}
	for i := range snapshot.ReviewThreads {
		snapshot.ReviewThreads[i].Resolved = false
	}
	for _, tc := range []struct {
		name            string
		budget          int
		keepAllFindings bool
	}{
		{"drop just peers", len(withoutPeers) + 1024 + 48*len(snapshot.ReviewThreads), true},
		{"drop derived rows", len(withoutOpenThreads) + 1024, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, complete, err := render(snapshot, tc.budget, 4000)
			if err != nil || complete || len(data) > tc.budget {
				t.Fatalf("render returned %d bytes, complete=%v, err=%v", len(data), complete, err)
			}
			var got struct {
				OpenFindings OpenFindings               `json:"open_findings"`
				Threads      []ReviewThread             `json:"review_threads"`
				Reviews      []Review                   `json:"reviews"`
				Sources      map[string]evidence.Source `json:"sources"`
				Output       struct {
					Omitted map[string]int `json:"omitted_items"`
				} `json:"output"`
			}
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatal(err)
			}
			if len(got.Threads) != len(snapshot.ReviewThreads) || len(got.Reviews) != len(snapshot.Reviews) || !got.Sources[SourceReviewThreads].Complete || !got.Sources[SourceReviews].Complete {
				t.Fatalf("lost original evidence: %d threads, %d reviews, omitted %v", len(got.Threads), len(got.Reviews), got.Output.Omitted)
			}
			for _, thread := range got.Threads {
				if len(thread.Comments) != len(template.Comments) || !thread.CommentsSource.Complete {
					t.Fatalf("lost original comments in %s", thread.ID)
				}
			}
			if got.OpenFindings.Complete || got.Sources[SourceOpenFindings].Complete || got.Output.Omitted["open_findings.same_location_as"] == 0 {
				t.Fatalf("peer omission is unreported: %+v", got.Output.Omitted)
			}
			if tc.keepAllFindings {
				if len(got.OpenFindings.UnresolvedThreads) != len(snapshot.ReviewThreads) {
					t.Fatalf("lost findings with sufficient space: %v", got.Output.Omitted)
				}
				for _, f := range got.OpenFindings.UnresolvedThreads {
					if len(f.SameLocationAs) > 0 || !f.SameLocationTruncated {
						t.Fatalf("inaccurate peer status: %+v", f)
					}
				}
			} else if got.Output.Omitted["open_findings.unresolved_threads"] == 0 {
				t.Fatalf("expected omitted findings: %v", got.Output.Omitted)
			}
		})
	}
}
