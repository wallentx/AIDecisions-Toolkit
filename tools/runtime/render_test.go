package runtime

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestRenderDeterministicAndBoundedWithExplicitOmissions(t *testing.T) {
	report := baseReport("/target")
	report.Status, report.Complete, report.Source.Complete = "complete", true, true
	report.ELF = &ELFFacts{Format: "ELF", Machine: "AArch64", Bitness: 64, Endianness: "little", Type: "shared_object", InterpreterObservation: "not_declared", Needed: []string{}, RPath: []string{}, RunPath: []string{}, OmissionCountsKnown: true}
	for index := 0; index < 300; index++ {
		report.ELF.Needed = append(report.ELF.Needed, fmt.Sprintf("lib%04d-%s.so", index, strings.Repeat("x", 20)))
	}
	first, complete, err := Render(report)
	if err != nil || complete || len(first) > MaximumOutputBytes || !json.Valid(first) {
		t.Fatalf("render complete=%v bytes=%d err=%v", complete, len(first), err)
	}
	second, _, _ := Render(report)
	if !bytes.Equal(first, second) {
		t.Fatal("render is not deterministic")
	}
	var got Report
	if err := json.Unmarshal(first, &got); err != nil {
		t.Fatal(err)
	}
	if !got.Output.Truncated || got.Output.OmittedItems == 0 || got.Source.Complete || got.ELF.OmittedNeeded == 0 {
		t.Fatalf("omissions not explicit: %+v", got)
	}
	if got.ELF.RPath == nil || got.ELF.RunPath == nil {
		t.Fatal("empty declaration arrays became null during output trimming")
	}
}

func TestRenderPreservesUnknownOmissionCountState(t *testing.T) {
	report := baseReport("/target")
	report.Status = "incomplete"
	report.Output.OmissionCountKnown = false
	report.ELF = &ELFFacts{Format: "ELF", Machine: "AArch64", Bitness: 64, Endianness: "little", Type: "shared_object", InterpreterObservation: "not_declared", Needed: []string{"known-prefix.so"}, RPath: []string{}, RunPath: []string{}, OmissionCountsKnown: false}
	encoded, _, err := Render(report)
	if err != nil {
		t.Fatal(err)
	}
	var got Report
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatal(err)
	}
	if got.Output.OmissionCountKnown || got.ELF.OmissionCountsKnown {
		t.Fatalf("unknown omission counts became exact: %+v", got)
	}
	var text bytes.Buffer
	if err := WriteText(&text, encoded); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text.String(), "unknown omitted") || strings.Contains(text.String(), "(0 omitted)") {
		t.Fatalf("text output presented an unknown count as exact: %s", &text)
	}
}

func TestTextEscapesControlsAndBidi(t *testing.T) {
	report := baseReport("/tmp/\x1b[31mred\u202Etxt")
	data, _ := encodeReport(report)
	var output bytes.Buffer
	if err := WriteText(&output, data); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), "\x1b") || strings.Contains(output.String(), "\u202E") {
		t.Fatalf("unsafe text: %q", output.String())
	}
}
