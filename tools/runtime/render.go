package runtime

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"unicode"
)

func Render(report Report) ([]byte, bool, error) {
	report.Output.MaxBytes = MaximumOutputBytes
	encoded, err := encodeReport(report)
	if err != nil {
		return nil, false, err
	}
	if len(encoded) <= MaximumOutputBytes {
		return encoded, report.Complete, nil
	}
	if report.ELF == nil {
		return nil, false, errors.New("required runtime evidence exceeds the fixed 4096-byte output envelope")
	}

	original := *report.ELF
	total := len(original.Needed) + len(original.RPath) + len(original.RunPath)
	low, high := 0, total
	var best []byte
	for low <= high {
		retained := low + (high-low)/2
		candidate := report
		facts := original
		facts.Needed, facts.RPath, facts.RunPath = retainDynamic(original, retained)
		facts.OmittedNeeded = original.OmittedNeeded + len(original.Needed) - len(facts.Needed)
		facts.OmittedRPath = original.OmittedRPath + len(original.RPath) - len(facts.RPath)
		facts.OmittedRunPath = original.OmittedRunPath + len(original.RunPath) - len(facts.RunPath)
		candidate.ELF = &facts
		candidate.Complete = false
		candidate.Source.Complete = false
		if candidate.Status == "complete" {
			candidate.Status = "incomplete"
		}
		candidate.Output.Truncated = true
		candidate.Output.OmittedItems = report.Output.OmittedItems + total - retained
		data, marshalErr := encodeReport(candidate)
		if marshalErr != nil {
			return nil, false, marshalErr
		}
		if len(data) <= MaximumOutputBytes {
			best = data
			low = retained + 1
		} else {
			high = retained - 1
		}
	}
	if best == nil {
		return nil, false, errors.New("required runtime evidence exceeds the fixed 4096-byte output envelope")
	}
	return best, false, nil
}

func retainDynamic(facts ELFFacts, count int) (needed, rpath, runpath []string) {
	take := func(values []string) []string {
		amount := len(values)
		if amount > count {
			amount = count
		}
		count -= amount
		return append([]string{}, values[:amount]...)
	}
	return take(facts.Needed), take(facts.RPath), take(facts.RunPath)
}

func encodeReport(report Report) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(report); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func WriteText(writer io.Writer, encoded []byte) error {
	var report Report
	if err := json.Unmarshal(encoded, &report); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(writer, "AID.runtime: %s (complete=%v)\nTarget: %s\n", safeText(report.Status), report.Complete, safeText(report.Target.Path)); err != nil {
		return err
	}
	if report.ELF != nil {
		omitted := strconv.Itoa(report.ELF.OmittedNeeded)
		if !report.ELF.OmissionCountsKnown {
			omitted = "unknown"
		}
		if _, err := fmt.Fprintf(writer, "ELF: %s %d-bit %s %s\nInterpreter: %s [%s]\nNeeded: %d declared (%s omitted)\n",
			safeText(report.ELF.Machine), report.ELF.Bitness, safeText(report.ELF.Endianness), safeText(report.ELF.Type),
			safeText(emptyAs(report.ELF.Interpreter, "<not declared>")), safeText(report.ELF.InterpreterObservation),
			len(report.ELF.Needed), omitted); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(writer, "Host: %s", safeText(report.Host.Observation)); err != nil {
		return err
	}
	if report.Host.Observation == "observed" {
		if _, err := fmt.Fprintf(writer, " (%s/%s)", safeText(report.Host.OS), safeText(report.Host.Arch)); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintln(writer); err != nil {
		return err
	}
	for _, path := range report.Host.Paths {
		if _, err := fmt.Fprintf(writer, "  %s: %s%s\n", safeText(path.Path), safeText(path.State), kindSuffix(path.Kind)); err != nil {
			return err
		}
	}
	if report.Issue != nil {
		if _, err := fmt.Fprintf(writer, "Issue: %s: %s\n", safeText(report.Issue.Code), safeText(report.Issue.Message)); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintln(writer, "Static facts only; dependency resolution and launch success remain unknown. Use --json for the evidence packet.")
	return err
}

func emptyAs(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func kindSuffix(kind string) string {
	if kind == "" {
		return ""
	}
	return " (" + safeText(kind) + ")"
}

func safeText(value string) string {
	result := make([]rune, 0, len(value))
	for _, character := range value {
		if unicode.IsControl(character) || isBidiControl(character) {
			continue
		}
		result = append(result, character)
	}
	return string(result)
}

func isBidiControl(value rune) bool {
	return value == '\u061c' || value == '\u200e' || value == '\u200f' ||
		(value >= '\u202a' && value <= '\u202e') || (value >= '\u2066' && value <= '\u2069')
}
