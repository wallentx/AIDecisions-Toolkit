package pr

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"sort"
	"strconv"
	"unicode/utf8"
)

// render applies presentation limits without making partial evidence look
// complete. API collection has its own aggregate retention and request limits.
func render(snapshot Snapshot, maxBytes, textLimit int) ([]byte, bool, error) {
	findings, findingsSource := deriveOpenFindings(snapshot, textLimit)
	snapshot.OpenFindings = &findings
	snapshot.Sources = maps.Clone(snapshot.Sources)
	if snapshot.Sources == nil {
		return nil, false, errors.New("missing source metadata")
	}
	snapshot.Sources[SourceOpenFindings] = findingsSource
	snapshot.Complete = snapshot.Complete && findings.Complete
	data, err := json.Marshal(snapshot)
	if err != nil {
		return nil, false, err
	}
	var document map[string]any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber() // GitHub identities must not pass through float64.
	if err := decoder.Decode(&document); err != nil {
		return nil, false, err
	}
	omitted := make(map[string]any)
	output := map[string]any{"max_bytes": maxBytes, "text_limit": textLimit, "truncated": false,
		"omitted_items": omitted, "truncated_bodies": 0}
	document["output"] = output
	sources, ok := document["sources"].(map[string]any)
	if !ok {
		return nil, false, errors.New("missing source metadata")
	}
	findingsDocument, _ := document[SourceOpenFindings].(map[string]any)
	mark := func(name string, status map[string]any, code string) {
		for _, dependency := range append([]string{SourceOpenFindings}, findingSources...) {
			if name == dependency {
				findingsDocument["complete"] = false
				derived, _ := sources[SourceOpenFindings].(map[string]any)
				derived["complete"] = false
				limitations, _ := derived["limitations"].([]any)
				found := false
				for _, limitation := range limitations {
					if limitation == "presentation_incomplete" {
						found = true
					}
				}
				if !found {
					derived["limitations"] = append(limitations, "presentation_incomplete")
				}
				break
			}
		}
		document["complete"] = false
		output["truncated"] = true
		output["retry_hint"] = "Rerun with higher --text-limit and/or --max-output-bytes; evidence is collected again and may have changed."
		if status == nil {
			status, _ = sources[name].(map[string]any)
		}
		if status == nil {
			status = make(map[string]any)
			sources[name] = status
		}
		status["complete"] = false
		limitations, _ := status["limitations"].([]any)
		found := false
		for _, existing := range limitations {
			if existing == code {
				found = true
			}
		}
		if !found {
			status["limitations"] = append(limitations, code)
		}
	}
	for _, name := range []string{"pull_request", "issue_comments", "reviews"} {
		if count := clipBodies(document[name], textLimit); count > 0 {
			output["truncated_bodies"] = output["truncated_bodies"].(int) + count
			mark(name, nil, "body_text_limit")
		}
	}
	threads, _ := document["review_threads"].([]any)
	for _, item := range threads {
		thread, _ := item.(map[string]any)
		if count := clipBodies(thread["comments"], textLimit); count > 0 {
			output["truncated_bodies"] = output["truncated_bodies"].(int) + count
			status, _ := thread["comments_source"].(map[string]any)
			if status == nil {
				status = make(map[string]any)
				thread["comments_source"] = status
			}
			mark("review_threads", status, "body_text_limit")
			mark("review_threads", nil, "body_text_limit")
		}
	}
	encode := func() ([]byte, error) {
		output["truncated_bodies"] = countTruncatedBodies(document)
		encoded, err := json.Marshal(document)
		if err != nil {
			return nil, err
		}
		return append(encoded, '\n'), nil
	}
	data, err = encode()
	if err != nil {
		return nil, false, err
	}
	// Trim deterministic tails, with a binary search for the largest prefix that
	// fits. This avoids repeated single-item removal and quadratic encoding cost.
	reduce := func(container map[string]any, field, source, counter string, status map[string]any) error {
		items, _ := container[field].([]any)
		if len(data) <= maxBytes || len(items) == 0 {
			return nil
		}
		mark(source, nil, "output_budget")
		if status != nil {
			mark(source, status, "output_budget")
		}
		previous, _ := omitted[counter].(int)
		set := func(keep int) {
			container[field] = items[:keep]
			omitted[counter] = previous + len(items) - keep
		}
		set(0)
		var err error
		data, err = encode()
		if err != nil || len(data) > maxBytes {
			return err
		}
		low, high := 0, len(items)-1
		for low < high {
			middle := low + (high-low+1)/2
			set(middle)
			candidate, err := encode()
			if err != nil {
				return err
			}
			if len(candidate) <= maxBytes {
				low = middle
			} else {
				high = middle - 1
			}
		}
		set(low)
		data, err = encode()
		return err
	}
	for _, name := range []string{"files", "statuses", "checks", "issue_comments", "reviews"} {
		if err := reduce(document, name, name, name, nil); err != nil {
			return nil, false, err
		}
	}
	for index := len(threads) - 1; index >= 0 && len(data) > maxBytes; index-- {
		thread, _ := threads[index].(map[string]any)
		status, _ := thread["comments_source"].(map[string]any)
		if err := reduce(thread, "comments", "review_threads", "review_comments", status); err != nil {
			return nil, false, err
		}
	}
	if err := reduce(document, "review_threads", "review_threads", "review_threads", nil); err != nil {
		return nil, false, err
	}
	for _, field := range []string{"unthreaded_review_bodies", "unresolved_threads"} {
		if err := reduce(findingsDocument, field, SourceOpenFindings, "open_findings."+field, nil); err != nil {
			return nil, false, err
		}
	}
	if len(data) > maxBytes {
		if count := clipBodies(document["pull_request"], 0); count > 0 {
			output["truncated_bodies"] = output["truncated_bodies"].(int) + count
			mark("pull_request", nil, "output_budget")
			data, err = encode()
		}
	}
	if err != nil || len(data) > maxBytes {
		return nil, false, errors.New("output budget cannot hold source metadata")
	}
	complete, _ := document["complete"].(bool)
	return data, complete, nil
}

func clipBodies(value any, limit int) int {
	count := 0
	switch value := value.(type) {
	case []any:
		for _, item := range value {
			count += clipBodies(item, limit)
		}
	case map[string]any:
		if body, ok := value["body"].(string); ok {
			length := utf8.RuneCountInString(body)
			if length > limit {
				value["body"] = string([]rune(body)[:limit])
				if _, exists := value["body_chars"]; !exists {
					value["body_chars"] = length
				}
				value["body_truncated"] = true
				count++
			}
		}
	}
	return count
}

func countTruncatedBodies(value any) int {
	count := 0
	switch value := value.(type) {
	case []any:
		for _, item := range value {
			count += countTruncatedBodies(item)
		}
	case map[string]any:
		if value["body_truncated"] == true {
			count++
		}
		for _, item := range value {
			count += countTruncatedBodies(item)
		}
	}
	return count
}

func writeText(w io.Writer, encoded []byte) error {
	var document map[string]any
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	if err := decoder.Decode(&document); err != nil {
		return err
	}
	metadata, _ := document["pull_request"].(map[string]any)
	if _, err := fmt.Fprintf(w, "AID.pr: %q\nComplete: %v (capture is not atomic)\n", metadata["title"], document["complete"]); err != nil {
		return err
	}
	if findings, ok := document[SourceOpenFindings].(map[string]any); ok {
		counts, _ := findings["counts"].(map[string]any)
		if _, err := fmt.Fprintf(w, "Open findings: %v bot, %v human, %v unknown; %v outdated; %v review bodies (complete: %v).\n", counts["bot"], counts["human"], counts["unknown"], counts["outdated"], counts["unthreaded_bodies"], findings["complete"]); err != nil {
			return err
		}
	}
	sources, _ := document["sources"].(map[string]any)
	keys := make([]string, 0, len(sources))
	for key := range sources {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if _, err := fmt.Fprintln(w, "SOURCE           COMPLETE  ITEMS  ERROR"); err != nil {
		return err
	}
	for _, key := range keys {
		source, _ := sources[key].(map[string]any)
		code := "-"
		if failure, ok := source["error"].(map[string]any); ok {
			if text, ok := failure["code"].(string); ok {
				code = strconv.QuoteToASCII(text)
			}
		}
		if _, err := fmt.Fprintf(w, "%-16s %-9v %-6v %s\n", key, source["complete"], source["items"], code); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintln(w, "Use --json for evidence. Exit 3 means incomplete, capped, or changed evidence.")
	return err
}
