package ci

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"aid/internal/evidence"
)

func TestRenderBudgetKeepsValidJSONAndMarksSources(t *testing.T) {
	snapshot := Snapshot{
		SchemaVersion: 1,
		Selection:     Selection{Scope: "run_attempt"},
		StartedAt:     time.Unix(1, 0).UTC(),
		FinishedAt:    time.Unix(2, 0).UTC(),
		Complete:      true,
		Sources: map[string]evidence.Source{
			SourceRun: {Complete: true}, SourceJobs: {Complete: true}, SourceLogs: {Complete: true},
			SourceAnnotations: {Complete: true}, SourceArtifacts: {Complete: true}, SourceFreshness: {Complete: true},
		},
		Jobs: []Job{{ID: 9_007_199_254_740_993, Name: "failed", Steps: []Step{{Name: strings.Repeat("s", 1000)}},
			Log: LogEvidence{State: "available", Excerpts: []LogExcerpt{{StartLine: 1, EndLine: 1, Lines: []string{strings.Repeat("l", 3000)}}}}}},
		Annotations: []Annotation{{CaptureOrdinal: 1, Message: strings.Repeat("a", 3000)}},
		Artifacts:   ArtifactCollection{Items: []Artifact{{ID: 8_007_199_254_740_993, Name: strings.Repeat("z", 1000)}}},
	}
	encoded, complete, err := render(snapshot, 4096)
	if err != nil {
		t.Fatalf("render() error: %v", err)
	}
	if complete || len(encoded) > 4096 || !json.Valid(encoded) {
		t.Errorf("render() complete/bytes/valid = %v/%d/%v, want false/<=4096/true", complete, len(encoded), json.Valid(encoded))
	}
	var got Snapshot
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatalf("json.Unmarshal(render()) error: %v", err)
	}
	if !got.Output.Truncated || got.Sources[SourceLogs].Complete || got.Sources[SourceAnnotations].Complete {
		t.Errorf("rendered omission state = output:%+v logs:%+v annotations:%+v", got.Output, got.Sources[SourceLogs], got.Sources[SourceAnnotations])
	}
	if got.Jobs[0].ID != 9_007_199_254_740_993 {
		t.Errorf("rendered job ID = %d, want exact 64-bit identity", got.Jobs[0].ID)
	}
}

func TestWriteTextStripsTerminalAndFormatControls(t *testing.T) {
	snapshot := Snapshot{SchemaVersion: 1, Selection: Selection{Description: "x\x1b[31m\u202Ey"}, Sources: map[string]evidence.Source{}, Jobs: []Job{}}
	encoded, _, err := render(snapshot, 4096)
	if err != nil {
		t.Fatalf("render() error: %v", err)
	}
	var output strings.Builder
	if err := writeText(&output, encoded); err != nil {
		t.Fatalf("writeText() error: %v", err)
	}
	if strings.Contains(output.String(), "\x1b") || strings.Contains(output.String(), "\u202e") {
		t.Errorf("writeText() retained terminal/format controls: %q", output.String())
	}
}
