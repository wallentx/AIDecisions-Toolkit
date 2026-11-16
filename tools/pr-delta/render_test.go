package prdelta

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"aid/internal/evidence"
)

func renderFixture(files int) Result {
	diff := &PRDiff{Verdict: VerdictChanged, Counts: map[string]int{VerdictChanged: files}, Files: []FileDelta{}}
	for index := range files {
		diff.Files = append(diff.Files, FileDelta{Path: fmt.Sprintf("dir/file-%04d.go", index), Status: VerdictChanged,
			ChangedHunks: []HunkChange{{OldHeader: "@@ -1,2 +1,3 @@", NewHeader: "@@ -4,2 +4,3 @@"}}})
	}
	return Result{SchemaVersion: SchemaVersion, Complete: true, HeadRelation: RelationRebased, NewCommits: []CommitRef{},
		Baseline: &Baseline{Source: "flag", Commit: baselineSHA}, PRDiff: diff,
		Threads: Threads{Items: []Thread{}}, CI: CI{NonSuccess: []CIRecord{}}, Warnings: []string{},
		Sources: map[string]evidence.Source{SourcePRDiff: {Complete: true}}}
}

func TestRenderOutputCapTrimsFilesAndMarksIncomplete(t *testing.T) {
	result := renderFixture(200)
	data, rendered, err := render(result, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) > 4096 || rendered.Complete {
		t.Fatalf("render() = %d bytes, complete %v", len(data), rendered.Complete)
	}
	var decoded struct {
		Complete bool `json:"complete"`
		Output   struct {
			Truncated    bool           `json:"truncated"`
			OmittedItems map[string]int `json:"omitted_items"`
		} `json:"output"`
		PRDiff struct {
			Files []FileDelta `json:"files"`
		} `json:"pr_diff"`
		Sources map[string]evidence.Source `json:"sources"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	kept := len(decoded.PRDiff.Files)
	if decoded.Complete || !decoded.Output.Truncated || kept == 0 || kept+decoded.Output.OmittedItems["pr_diff.files"] != 200 {
		t.Errorf("capped output = complete %v truncated %v kept %d omitted %v", decoded.Complete, decoded.Output.Truncated, kept, decoded.Output.OmittedItems)
	}
	if source := decoded.Sources[SourcePRDiff]; source.Complete || len(source.Limitations) != 1 || source.Limitations[0] != "output_budget" {
		t.Errorf("pr_diff source = %+v", source)
	}
	if !result.Sources[SourcePRDiff].Complete || len(result.PRDiff.Files) != 200 {
		t.Error("render mutated the collected result")
	}
}

func TestRenderSmallResultIsUntouched(t *testing.T) {
	data, rendered, err := render(renderFixture(2), 128<<10)
	if err != nil || !rendered.Complete || rendered.Output.Truncated || !bytes.HasSuffix(data, []byte("\n")) {
		t.Fatalf("render() complete %v truncated %v err %v", rendered.Complete, rendered.Output.Truncated, err)
	}
}

func TestWriteTextSummarizesDelta(t *testing.T) {
	result := renderFixture(1)
	result.Baseline = &Baseline{Source: "review", ReviewState: "APPROVED", Commit: baselineSHA}
	result.PR = PRInfo{State: "open", HeadSHA: headSHA, BaseRef: "topic-base"}
	result.Stack = Stack{BasePR: &BasePR{Number: 474, State: "open"}}
	result.CI.Counts.Success = 8
	var out bytes.Buffer
	if err := writeText(&out, result); err != nil {
		t.Fatal(err)
	}
	want := "baseline APPROVED at 111111111; head 222222222 rebased; PR diff changed (1 files); threads 0 (0 unresolved, 0 with reply); CI 8 success; base PR #474 open"
	if !strings.Contains(out.String(), want) {
		t.Errorf("text = %q, want line %q", out.String(), want)
	}
}
