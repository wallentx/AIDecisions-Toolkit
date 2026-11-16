package reviewqueue

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"aid/internal/prdelta"
)

// render encodes the result within maxBytes. Items are trimmed from the tail;
// a trim marks the result truncated and incomplete so a capped queue never
// looks complete. The summary keeps counting every collected item.
func render(result Result, maxBytes int) ([]byte, Result, error) {
	output := &OutputInfo{MaxBytes: maxBytes}
	result.Output = output
	encode := func() ([]byte, error) {
		data, err := json.Marshal(result)
		return append(data, '\n'), err
	}
	data, err := encode()
	if err != nil || len(data) <= maxBytes {
		return data, result, err
	}
	items := result.Items
	result.Complete = false
	output.Truncated = true
	output.RetryHint = "Rerun with a higher --max-output-bytes or a lower --max-prs; the queue is collected again and may have changed."
	result.Warnings = appendUnique(result.Warnings, "output_budget")
	keep := func(count int) ([]byte, error) {
		result.Items = items[:count]
		output.OmittedItems = len(items) - count
		return encode()
	}
	low, high := 0, len(items)
	for low < high {
		middle := low + (high-low+1)/2
		candidate, err := keep(middle)
		if err != nil {
			return nil, result, err
		}
		if len(candidate) <= maxBytes {
			low = middle
		} else {
			high = middle - 1
		}
	}
	if data, err = keep(low); err != nil {
		return nil, result, err
	}
	if len(data) > maxBytes {
		return nil, result, errors.New("output budget cannot hold the result metadata")
	}
	return data, result, nil
}

// writeText prints one line per pull request.
func writeText(w io.Writer, result Result) error {
	var b strings.Builder
	scope := "all visible repositories"
	if len(result.Scope.Owners) > 0 {
		scope = strings.Join(result.Scope.Owners, ", ")
	}
	fmt.Fprintf(&b, "AID.review-queue reviewer %s; scope %s (%s)\n", orDash(result.Reviewer), scope, result.Scope.Source)
	s := result.Summary
	if result.Sources.Search.Status != StatusOK {
		fmt.Fprintf(&b, "search failed: %s\n", result.Sources.Search.Status)
	} else if s.Total == 0 {
		fmt.Fprintln(&b, "no open pull requests are waiting on this reviewer")
	} else {
		fmt.Fprintf(&b, "%d waiting: %d new, %d re-review (%d unchanged)", s.Total, s.New, s.ReReview, s.ReReviewUnchanged)
		if s.Unknown > 0 {
			fmt.Fprintf(&b, ", %d unknown", s.Unknown)
		}
		fmt.Fprintln(&b)
	}
	for _, item := range result.Items {
		fmt.Fprintf(&b, "%s#%d %s\n", item.Repo, item.Number, itemText(item))
	}
	if result.Output != nil && result.Output.OmittedItems > 0 {
		fmt.Fprintf(&b, "... %d more omitted by the output budget\n", result.Output.OmittedItems)
	}
	if len(result.Warnings) > 0 {
		fmt.Fprintf(&b, "warnings: %s\n", strings.Join(result.Warnings, ", "))
	}
	fmt.Fprintf(&b, "complete: %v\n", result.Complete)
	_, err := io.WriteString(w, b.String())
	return err
}

func itemText(item Item) string {
	head := map[string]string{KindNew: "new", KindReReview: "re-review"}[item.Kind]
	if head == "" {
		head = "unknown"
	}
	if item.Draft {
		head += " (draft)"
	}
	var detail []string
	switch delta := item.Delta; {
	case item.Kind == KindReReview && delta == nil:
		detail = append(detail, "delta skipped")
	case delta != nil:
		detail = append(detail, delta.HeadRelation, "diff "+verdictText(delta.Verdict))
		if delta.ChangedFiles > 0 {
			detail = append(detail, fmt.Sprintf("%d files changed", delta.ChangedFiles))
		}
		threads := fmt.Sprintf("%d open threads", delta.ThreadsUnresolved)
		if delta.ThreadsUnresolvedWithReply > 0 {
			threads += fmt.Sprintf(" (%d with reply)", delta.ThreadsUnresolvedWithReply)
		}
		ci := delta.CI
		detail = append(detail, threads, fmt.Sprintf("CI %d/%d", ci.Success, ci.Success+ci.Failure+ci.Pending))
		if ci.Failure > 0 {
			detail = append(detail, fmt.Sprintf("%d failing", ci.Failure))
		}
		if !delta.Complete {
			detail = append(detail, "delta incomplete")
		}
	default:
		detail = append(detail, "by "+orDash(item.Author)+": "+item.Title)
	}
	return head + " " + strings.Join(detail, ", ")
}

func verdictText(verdict string) string {
	if verdict == prdelta.VerdictMovedOnly {
		return "moved only"
	}
	return verdict
}

func orDash(value string) string {
	if value == "" {
		return "-"
	}
	return value
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}
