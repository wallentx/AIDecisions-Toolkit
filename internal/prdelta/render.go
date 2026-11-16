package prdelta

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"aid/internal/evidence"
)

// Render encodes the result within maxBytes. Lists are trimmed from their
// least important tail; any trim marks the owning source and the result
// incomplete so a capped document never looks complete.
func Render(result Result, maxBytes int) ([]byte, Result, error) {
	output := &OutputInfo{MaxBytes: maxBytes, OmittedItems: map[string]int{}}
	result.Output = output
	result.Sources = cloneSources(result.Sources)
	encode := func() ([]byte, error) {
		data, err := json.Marshal(result)
		return append(data, '\n'), err
	}
	data, err := encode()
	if err != nil {
		return nil, result, err
	}
	mark := func(source string) {
		result.Complete = false
		output.Truncated = true
		output.RetryHint = "Rerun with a higher --max-output-bytes; evidence is collected again and may have changed."
		status := result.Sources[source]
		status.Complete = false
		status.Limitations = appendUnique(status.Limitations, "output_budget")
		result.Sources[source] = status
	}
	// trim keeps the longest prefix that fits. owners names the sources whose
	// records fall in the omitted tail; each candidate is encoded with exactly
	// those sources marked, so the size check includes their limitations.
	trim := func(name string, length int, set func(keep int), owners func(keep int) []string) error {
		if len(data) <= maxBytes || length == 0 {
			return nil
		}
		before := cloneSources(result.Sources)
		keepCount := func(keep int) {
			set(keep)
			output.OmittedItems[name] = length - keep
			result.Sources = cloneSources(before)
			for _, source := range owners(keep) {
				mark(source)
			}
		}
		keepCount(0)
		if data, err = encode(); err != nil || len(data) > maxBytes {
			return err
		}
		low, high := 0, length-1
		for low < high {
			middle := low + (high-low+1)/2
			keepCount(middle)
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
		keepCount(low)
		data, err = encode()
		return err
	}
	nonSuccess := result.CI.NonSuccess
	threads := result.Threads.Items
	commits := result.NewCommits
	var files []FileDelta
	if result.PRDiff != nil {
		diff := *result.PRDiff
		result.PRDiff = &diff
		files = diff.Files
	}
	only := func(source string) func(int) []string { return func(int) []string { return []string{source} } }
	// new_commits comes from the compare for a fast-forward and from patch
	// equivalence for a rebase.
	commitsSource := SourceHeadRelation
	if result.HeadRelation == RelationRebased {
		commitsSource = SourceCommits
	}
	steps := []struct {
		name   string
		length int
		set    func(int)
		owners func(int) []string
	}{
		{"ci.non_success", len(nonSuccess), func(keep int) { result.CI.NonSuccess = nonSuccess[:keep] }, func(keep int) []string {
			var sources []string
			for _, record := range nonSuccess[keep:] {
				sources = appendUnique(sources, ciSource(record.Kind))
			}
			return sources
		}},
		{"threads.items", len(threads), func(keep int) { result.Threads.Items = threads[:keep] }, only(SourceThreads)},
		{"new_commits", len(commits), func(keep int) { result.NewCommits = commits[:keep] }, only(commitsSource)},
		{"pr_diff.files", len(files), func(keep int) { result.PRDiff.Files = files[:keep] }, only(SourcePRDiff)},
	}
	for _, step := range steps {
		if err := trim(step.name, step.length, step.set, step.owners); err != nil {
			return nil, result, err
		}
	}
	if len(data) > maxBytes {
		return nil, result, errors.New("output budget cannot hold the result metadata")
	}
	return data, result, nil
}

// ciSource maps a CIRecord kind to the source that collected it.
func ciSource(kind string) string {
	if kind == "status" {
		return SourceStatuses
	}
	return SourceChecks
}

func cloneSources(sources map[string]evidence.Source) map[string]evidence.Source {
	clone := make(map[string]evidence.Source, len(sources))
	for key, value := range sources {
		value.Limitations = append([]string(nil), value.Limitations...)
		clone[key] = value
	}
	return clone
}

// WriteText prints a short human summary of a rendered result.
func WriteText(w io.Writer, result Result) error {
	var b strings.Builder
	fmt.Fprintf(&b, "AID.pr-delta %s/%s#%d (%s) reviewer %s\n", result.Reference.Owner, result.Reference.Name,
		result.Reference.Number, orDash(result.PR.State), orDash(result.Reviewer))
	parts := []string{baselineText(result.Baseline)}
	parts = append(parts, fmt.Sprintf("head %s %s", short(result.PR.HeadSHA), result.HeadRelation))
	if diff := result.PRDiff; diff != nil {
		touched := 0
		for status, count := range diff.Counts {
			if status != VerdictUnchanged {
				touched += count
			}
		}
		parts = append(parts, fmt.Sprintf("PR diff %s (%d files)", diff.Verdict, touched))
	} else {
		parts = append(parts, "PR diff not computed")
	}
	parts = append(parts, fmt.Sprintf("threads %d (%d unresolved, %d with author reply)",
		result.Threads.Total, result.Threads.Unresolved, result.Threads.UnresolvedWithAuthorReply))
	parts = append(parts, ciText(result.CI.Counts), stackText(result))
	fmt.Fprintln(&b, strings.Join(parts, "; "))
	if diff := result.PRDiff; diff != nil {
		if diff.Reason != "" {
			fmt.Fprintf(&b, "PR diff note: %s\n", diff.Reason)
		}
		shown := 0
		for _, file := range diff.Files {
			if file.Status == VerdictUnchanged {
				continue
			}
			if shown == 10 {
				fmt.Fprintln(&b, "  ... more files in --json")
				break
			}
			fmt.Fprintf(&b, "  %-10s %s (%d hunks)\n", file.Status, file.Path, len(file.ChangedHunks)+file.HunksOmitted)
			shown++
		}
	}
	if result.NewCommits != nil {
		fmt.Fprintf(&b, "new commits: %d\n", len(result.NewCommits))
		for index, commit := range result.NewCommits {
			if index == 10 {
				fmt.Fprintln(&b, "  ... more commits in --json")
				break
			}
			fmt.Fprintf(&b, "  %s %s\n", short(commit.SHA), commit.MessageFirstLine)
		}
	}
	if result.NewCommitsNote != "" {
		fmt.Fprintf(&b, "new commits note: %s\n", result.NewCommitsNote)
	}
	if len(result.Warnings) > 0 {
		fmt.Fprintf(&b, "warnings: %s\n", strings.Join(result.Warnings, ", "))
	}
	fmt.Fprintf(&b, "complete: %v%s\n", result.Complete, sourceErrors(result))
	_, err := io.WriteString(w, b.String())
	return err
}

func baselineText(baseline *Baseline) string {
	switch {
	case baseline == nil:
		return "baseline missing"
	case baseline.Source == "flag":
		return "baseline --since-commit " + short(baseline.Commit)
	default:
		return fmt.Sprintf("baseline %s at %s", baseline.ReviewState, short(baseline.Commit))
	}
}

func ciText(counts CICounts) string {
	var parts []string
	for _, item := range []struct {
		name  string
		count int
	}{{"success", counts.Success}, {"failure", counts.Failure}, {"pending", counts.Pending},
		{"skipped", counts.Skipped}, {"neutral", counts.Neutral}, {"cancelled", counts.Cancelled}, {"other", counts.Other}} {
		if item.count > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", item.count, item.name))
		}
	}
	if len(parts) == 0 {
		return "CI none"
	}
	return "CI " + strings.Join(parts, ", ")
}

func stackText(result Result) string {
	stack := result.Stack
	switch {
	case stack.BaseIsDefaultBranch:
		return "base " + result.PR.BaseRef + " (default branch)"
	case stack.BasePR != nil:
		return fmt.Sprintf("base PR #%d %s", stack.BasePR.Number, stack.BasePR.State)
	case stack.BaseBranchExists != nil && !*stack.BaseBranchExists:
		return "base branch " + result.PR.BaseRef + " missing"
	default:
		return "base " + orDash(result.PR.BaseRef)
	}
}

func sourceErrors(result Result) string {
	var failed []string
	for key, source := range result.Sources {
		if source.Error != nil {
			failed = append(failed, key+"="+source.Error.Code)
		} else if !source.Complete {
			failed = append(failed, key+"=incomplete")
		}
	}
	if len(failed) == 0 {
		return ""
	}
	sort.Strings(failed)
	return " (" + strings.Join(failed, ", ") + ")"
}

func short(sha string) string {
	if len(sha) > 9 {
		return sha[:9]
	}
	return orDash(sha)
}

func orDash(value string) string {
	if value == "" {
		return "-"
	}
	return value
}
