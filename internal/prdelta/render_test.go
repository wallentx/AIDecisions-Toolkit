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
	data, rendered, err := Render(result, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) > 4096 || rendered.Complete {
		t.Fatalf("Render() = %d bytes, complete %v", len(data), rendered.Complete)
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
	data, rendered, err := Render(renderFixture(2), 128<<10)
	if err != nil || !rendered.Complete || rendered.Output.Truncated || !bytes.HasSuffix(data, []byte("\n")) {
		t.Fatalf("Render() complete %v truncated %v err %v", rendered.Complete, rendered.Output.Truncated, err)
	}
}

func TestWriteTextSummarizesDelta(t *testing.T) {
	result := renderFixture(1)
	result.Baseline = &Baseline{Source: "review", ReviewState: "APPROVED", Commit: baselineSHA}
	result.PR = PRInfo{State: "open", HeadSHA: headSHA, BaseRef: "topic-base"}
	result.Stack = Stack{BasePR: &BasePR{Number: 474, State: "open"}}
	result.CI.Counts.Success = 8
	var out bytes.Buffer
	if err := WriteText(&out, result); err != nil {
		t.Fatal(err)
	}
	want := "baseline APPROVED at 111111111; head 222222222 rebased; PR diff changed (1 files); threads 0 (0 unresolved, 0 with author reply); CI 8 success; base PR #474 open"
	if !strings.Contains(out.String(), want) {
		t.Errorf("text = %q, want line %q", out.String(), want)
	}
}

func TestRenderTrimMarksOwningSources(t *testing.T) {
	sources := func() map[string]evidence.Source {
		return map[string]evidence.Source{SourceChecks: {Complete: true}, SourceStatuses: {Complete: true},
			SourceHeadRelation: {Complete: true}, SourceCommits: {Complete: true}, SourcePRDiff: {Complete: true}}
	}
	long := strings.Repeat("x", 60)
	ci := renderFixture(0)
	ci.Sources = sources()
	ci.CI.NonSuccess = []CIRecord{{Name: "check", Kind: "check_run", Conclusion: "failure"}}
	for index := range 200 {
		ci.CI.NonSuccess = append(ci.CI.NonSuccess, CIRecord{Name: fmt.Sprintf("status-%d-%s", index, long), Kind: "status", Conclusion: "failure"})
	}
	commits := func(relation string) Result {
		result := renderFixture(0)
		result.Sources = sources()
		result.HeadRelation = relation
		for index := range 200 {
			result.NewCommits = append(result.NewCommits, CommitRef{SHA: headSHA, MessageFirstLine: fmt.Sprintf("commit %d %s", index, long)})
		}
		return result
	}
	for _, test := range []struct {
		name             string
		result           Result
		marked, unmarked string
	}{
		{"statuses omitted", ci, SourceStatuses, SourceChecks},
		{"rebased commits", commits(RelationRebased), SourceCommits, SourceHeadRelation},
		{"fast-forward commits", commits(RelationFastForward), SourceHeadRelation, SourceCommits},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, rendered, err := Render(test.result, 4096)
			if err != nil {
				t.Fatal(err)
			}
			if marked := rendered.Sources[test.marked]; marked.Complete || len(marked.Limitations) != 1 || marked.Limitations[0] != "output_budget" {
				t.Errorf("%s = %+v, want output_budget", test.marked, marked)
			}
			if unmarked := rendered.Sources[test.unmarked]; !unmarked.Complete || len(unmarked.Limitations) != 0 {
				t.Errorf("%s = %+v, want untouched", test.unmarked, unmarked)
			}
			if rendered.Complete || !rendered.Output.Truncated {
				t.Errorf("complete = %v truncated = %v", rendered.Complete, rendered.Output.Truncated)
			}
		})
	}
}
