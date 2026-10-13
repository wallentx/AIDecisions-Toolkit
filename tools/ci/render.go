package ci

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
)

func render(snapshot Snapshot, maxBytes int) ([]byte, bool, error) {
	document, err := cloneSnapshot(snapshot)
	if err != nil {
		return nil, false, err
	}
	document.Output = OutputState{MaxBytes: maxBytes, OmittedItems: map[string]int{}}
	if encoded, fits := encodeWithin(document, maxBytes); fits {
		return encoded, document.Complete, nil
	}

	for jobIndex := len(document.Jobs) - 1; jobIndex >= 0; jobIndex-- {
		for len(document.Jobs[jobIndex].Log.Excerpts) > 0 {
			last := len(document.Jobs[jobIndex].Log.Excerpts) - 1
			document.Jobs[jobIndex].Log.Excerpts = document.Jobs[jobIndex].Log.Excerpts[:last]
			document.Jobs[jobIndex].Log.ExcerptSelectionComplete = false
			document.Jobs[jobIndex].Log.OmittedExcerpts++
			document.markOutputOmission(SourceLogs, "log_excerpts", 1)
			if encoded, fits := encodeWithin(document, maxBytes); fits {
				return encoded, false, nil
			}
		}
	}

	if len(document.Annotations) > 0 {
		original := document.Annotations
		document.markOutputOmission(SourceAnnotations, "annotations", len(original))
		document.Annotations = []Annotation{}
		if encoded, fits := encodeWithin(document, maxBytes); fits {
			return encoded, false, nil
		}
	}
	if len(document.Artifacts.Items) > 0 {
		count := len(document.Artifacts.Items)
		document.Artifacts.Items = []Artifact{}
		document.markOutputOmission(SourceArtifacts, "artifacts", count)
		if encoded, fits := encodeWithin(document, maxBytes); fits {
			return encoded, false, nil
		}
	}

	stepCount := 0
	labelCount := 0
	for index := range document.Jobs {
		stepCount += len(document.Jobs[index].Steps)
		labelCount += len(document.Jobs[index].Labels)
		document.Jobs[index].Steps = []Step{}
		document.Jobs[index].Labels = []string{}
	}
	if stepCount > 0 {
		document.markOutputOmission(SourceJobs, "job_steps", stepCount)
	}
	if labelCount > 0 {
		document.markOutputOmission(SourceJobs, "job_labels", labelCount)
	}
	if encoded, fits := encodeWithin(document, maxBytes); fits {
		return encoded, false, nil
	}

	if document.Selection.Scope == "run_attempt" && len(document.Jobs) > 0 {
		for len(document.Jobs) > 0 {
			document.Jobs = document.Jobs[:len(document.Jobs)-1]
			document.markOutputOmission(SourceJobs, "jobs", 1)
			document.markOutputOmission(SourceLogs, "job_log_records", 1)
			if encoded, fits := encodeWithin(document, maxBytes); fits {
				return encoded, false, nil
			}
		}
	}

	jobOptionalFields := 0
	for index := range document.Jobs {
		if document.Jobs[index].HTMLURL != "" || document.Jobs[index].CheckRunURL != "" || document.Jobs[index].RunnerName != "" {
			jobOptionalFields++
		}
		document.Jobs[index].HTMLURL = ""
		document.Jobs[index].CheckRunURL = ""
		document.Jobs[index].RunnerName = ""
	}
	document.Run.URL = ""
	document.Run.HTMLURL = ""
	document.Run.DisplayTitle = ""
	document.markOutputOmission(SourceJobs, "optional_job_fields", jobOptionalFields)
	document.markOutputOmission(SourceRun, "optional_run_fields", 1)
	if encoded, fits := encodeWithin(document, maxBytes); fits {
		return encoded, false, nil
	}
	return nil, false, errors.New("required CI evidence exceeds output limit")
}

func cloneSnapshot(snapshot Snapshot) (Snapshot, error) {
	data, err := json.Marshal(snapshot)
	if err != nil {
		return Snapshot{}, err
	}
	var cloned Snapshot
	if err := json.Unmarshal(data, &cloned); err != nil {
		return Snapshot{}, err
	}
	return cloned, nil
}

func encodeWithin(snapshot Snapshot, maxBytes int) ([]byte, bool) {
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return nil, false
	}
	encoded = append(encoded, '\n')
	return encoded, len(encoded) <= maxBytes
}

func (snapshot *Snapshot) markOutputOmission(sourceKey, item string, count int) {
	if count <= 0 {
		return
	}
	snapshot.Complete = false
	snapshot.Output.Truncated = true
	snapshot.Output.OmittedItems[item] += count
	snapshot.Output.RetryHint = "Reduce the selected scope or rerun with a higher --max-output-bytes within the documented cap; evidence is collected again and may have changed."
	source := snapshot.Sources[sourceKey]
	source.Complete = false
	limitation := "output_budget"
	found := false
	for _, existing := range source.Limitations {
		if existing == limitation {
			found = true
			break
		}
	}
	if !found {
		source.Limitations = append(source.Limitations, limitation)
	}
	snapshot.Sources[sourceKey] = source
}

func writeText(writer io.Writer, encoded []byte) error {
	var snapshot Snapshot
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	if err := decoder.Decode(&snapshot); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(writer, "AID.ci: %s\nComplete: %v (capture is not atomic)\nSelection: %s\n", stripControls(formatReference(snapshot.Reference)), snapshot.Complete, stripControls(snapshot.Selection.Description)); err != nil {
		return err
	}
	keys := make([]string, 0, len(snapshot.Sources))
	for key := range snapshot.Sources {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if _, err := fmt.Fprintln(writer, "SOURCE       COMPLETE  ITEMS  ERROR"); err != nil {
		return err
	}
	for _, key := range keys {
		source := snapshot.Sources[key]
		code := "-"
		if source.Error != nil {
			code = strconv.QuoteToASCII(stripControls(source.Error.Code))
		}
		if _, err := fmt.Fprintf(writer, "%-12s %-9v %-6d %s\n", key, source.Complete, source.Items, code); err != nil {
			return err
		}
	}
	for _, job := range snapshot.Jobs {
		if _, err := fmt.Fprintf(writer, "\nJob %d: %s [%s/%s] log=%s\n", job.ID, stripControls(job.Name), stripControls(job.Status), stripControls(job.Conclusion), stripControls(job.Log.State)); err != nil {
			return err
		}
		for _, excerpt := range job.Log.Excerpts {
			if _, err := fmt.Fprintf(writer, "  lines %d-%d (%s)\n", excerpt.StartLine, excerpt.EndLine, stripControls(excerpt.SelectionReason)); err != nil {
				return err
			}
			for offset, line := range excerpt.Lines {
				if _, err := fmt.Fprintf(writer, "  %d | %s\n", excerpt.StartLine+offset, stripControls(line)); err != nil {
					return err
				}
			}
		}
	}
	_, err := fmt.Fprintln(writer, "Use --json for complete structured evidence. Exit 3 means partial, capped, changed, or ongoing evidence.")
	return err
}
