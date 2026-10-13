package ci

import (
	"bytes"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

var (
	errorMarker         = regexp.MustCompile(`(?i)(::error|##\[error\]|\b(error|fail(?:ed|ure)?|fatal|panic|exception|timed[ -]?out|cancel(?:led|ed))\b)`)
	ansiEscape          = regexp.MustCompile(`\x1b(?:\[[0-?]*[ -/]*[@-~]|\][^\x07]*(?:\x07|\x1b\\)|[@-_])`)
	authorizationHeader = regexp.MustCompile(`(?i)((?:proxy-)?authorization\s*:\s*)[^\r\n]*`)
	bearerToken         = regexp.MustCompile(`(?i)(\bbearer\s+)[A-Za-z0-9._~+/=-]{8,}`)
	githubToken         = regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9_]{16,}|github_pat_[A-Za-z0-9_]{16,})\b`)
	awsAccessKey        = regexp.MustCompile(`\b(?:AKIA|ASIA)[A-Z0-9]{16}\b`)
	googleAPIKey        = regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{20,}\b`)
	slackToken          = regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,}\b`)
	urlCredentials      = regexp.MustCompile(`(?i)(https?://)[^/@\s:]+:[^/@\s]+@`)
	secretQuery         = regexp.MustCompile(`(?i)([?&](?:access_token|api[_-]?key|auth|password|secret|signature|token)=)[^&#\s]+`)
	secretAssignment    = regexp.MustCompile(`(?i)(\b(?:[A-Za-z0-9_]*(?:access[_-]?token|account[_-]?key|api[_-]?key|client[_-]?secret|password|private[_-]?key|secret|token)[A-Za-z0-9_]*|authorization)\b\s*[:=]\s*)('[^']*'|"[^"]*"|[^\s,;]+)`)
	privateKeyBegin     = regexp.MustCompile(`(?i)-----BEGIN (?:[A-Z0-9]+ )?(?:ENCRYPTED )?PRIVATE KEY-----`)
	privateKeyEnd       = regexp.MustCompile(`(?i)-----END (?:[A-Z0-9]+ )?(?:ENCRYPTED )?PRIVATE KEY-----`)
)

func processLog(data []byte, failedSteps []string, annotations []Annotation, opts Options, prefixTruncated bool) LogEvidence {
	received := len(data)
	if prefixTruncated {
		lastCompleteLine := bytes.LastIndexByte(data, '\n')
		if lastCompleteLine < 0 {
			data = nil
		} else {
			data = data[:lastCompleteLine+1]
		}
	}
	if len(data) == 0 {
		return LogEvidence{State: "empty", Reason: "empty_response", RetrievalComplete: !prefixTruncated, ExcerptSelectionComplete: !prefixTruncated, BytesReceived: received, Excerpts: []LogExcerpt{}}
	}
	data = bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
	rawLines, omittedLines := boundedLogLines(data, opts.MaxLogLines)
	rawLines = redactPrivateKeyBlocks(rawLines)
	lines := make([]string, len(rawLines))
	truncatedLines := 0
	for index, line := range rawLines {
		var truncated bool
		lines[index], truncated = sanitizeLogLineWithState(line, opts.MaxLineCharacters)
		if truncated {
			truncatedLines++
		}
	}
	markers := make([]logMarker, 0)
	failedNeedles := normalizedNeedles(failedSteps, 32)
	annotationNeedles := normalizedAnnotationNeedles(annotations, 32)
	for index, line := range lines {
		reason := ""
		if errorMarker.MatchString(line) {
			reason = "error_marker"
		}
		if reason == "" && containsNeedle(line, failedNeedles) {
			reason = "failed_step_name"
		}
		if reason == "" && containsNeedle(line, annotationNeedles) {
			reason = "annotation_context"
		}
		if reason != "" {
			markers = append(markers, logMarker{line: index, reason: reason})
		}
	}
	excerpts, omitted := selectExcerpts(lines, markers, opts)
	return LogEvidence{
		State:                    "available",
		Reason:                   "retrieved",
		RetrievalComplete:        !prefixTruncated,
		ExcerptSelectionComplete: omitted == 0 && omittedLines == 0 && truncatedLines == 0 && !prefixTruncated,
		BytesReceived:            received,
		LinesObserved:            len(lines),
		OmittedLines:             omittedLines,
		TruncatedLines:           truncatedLines,
		Excerpts:                 excerpts,
		OmittedExcerpts:          omitted,
	}
}

func redactPrivateKeyBlocks(lines []string) []string {
	redacted := make([]string, len(lines))
	inPrivateKey := false
	for index, line := range lines {
		if !inPrivateKey && privateKeyBegin.MatchString(line) {
			redacted[index] = "[REDACTED PRIVATE KEY BLOCK]"
			inPrivateKey = !privateKeyEnd.MatchString(line)
			continue
		}
		if inPrivateKey {
			if privateKeyEnd.MatchString(line) {
				inPrivateKey = false
			}
			redacted[index] = ""
			continue
		}
		redacted[index] = line
	}
	return redacted
}

func boundedLogLines(data []byte, maxLines int) ([]string, int) {
	capacity := maxLines
	if capacity > 4096 {
		capacity = 4096
	}
	lines := make([]string, 0, capacity)
	remaining := data
	for len(remaining) > 0 && len(lines) < maxLines {
		newline := bytes.IndexByte(remaining, '\n')
		if newline < 0 {
			lines = append(lines, string(remaining))
			remaining = nil
			break
		}
		lines = append(lines, string(remaining[:newline]))
		remaining = remaining[newline+1:]
	}
	omitted := bytes.Count(remaining, []byte{'\n'})
	if len(remaining) > 0 && remaining[len(remaining)-1] != '\n' {
		omitted++
	}
	return lines, omitted
}

type logMarker struct {
	line   int
	reason string
}

type lineRange struct {
	start  int
	end    int
	reason string
}

func selectExcerpts(lines []string, markers []logMarker, opts Options) ([]LogExcerpt, int) {
	if len(lines) == 0 {
		return []LogExcerpt{}, 0
	}
	if len(markers) == 0 {
		start := len(lines) - opts.MaxExcerptLines
		if start < 0 {
			start = 0
		}
		return []LogExcerpt{makeExcerpt(lines, start, len(lines)-1, "tail_fallback")}, 0
	}
	ranges := make([]lineRange, 0, len(markers))
	for _, marker := range markers {
		start := marker.line - opts.ContextLines
		if start < 0 {
			start = 0
		}
		end := marker.line + opts.ContextLines
		if end >= len(lines) {
			end = len(lines) - 1
		}
		if end-start+1 > opts.MaxExcerptLines {
			end = start + opts.MaxExcerptLines - 1
		}
		if len(ranges) > 0 && start <= ranges[len(ranges)-1].end+1 {
			previous := &ranges[len(ranges)-1]
			if end > previous.end {
				previous.end = end
			}
			if previous.end-previous.start+1 > opts.MaxExcerptLines {
				previous.end = previous.start + opts.MaxExcerptLines - 1
			}
			continue
		}
		ranges = append(ranges, lineRange{start: start, end: end, reason: marker.reason})
	}
	omitted := 0
	if len(ranges) > opts.MaxExcerpts {
		omitted = len(ranges) - opts.MaxExcerpts
		ranges = ranges[:opts.MaxExcerpts]
	}
	excerpts := make([]LogExcerpt, 0, len(ranges))
	for _, selected := range ranges {
		excerpts = append(excerpts, makeExcerpt(lines, selected.start, selected.end, selected.reason))
	}
	return excerpts, omitted
}

func makeExcerpt(lines []string, start, end int, reason string) LogExcerpt {
	selected := append([]string(nil), lines[start:end+1]...)
	return LogExcerpt{StartLine: start + 1, EndLine: end + 1, Lines: selected, SelectionReason: reason}
}

func normalizedNeedles(values []string, limit int) []string {
	needles := make([]string, 0, limit)
	seen := make(map[string]struct{})
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(truncateRunes(value, 256)))
		if utf8.RuneCountInString(value) < 4 {
			continue
		}
		if _, duplicate := seen[value]; duplicate {
			continue
		}
		seen[value] = struct{}{}
		needles = append(needles, value)
		if len(needles) == limit {
			break
		}
	}
	return needles
}

func normalizedAnnotationNeedles(annotations []Annotation, limit int) []string {
	values := make([]string, 0, limit)
	for _, annotation := range annotations {
		values = append(values, annotation.Title, annotation.Message)
		if len(values) >= limit*2 {
			break
		}
	}
	return normalizedNeedles(values, limit)
}

func containsNeedle(line string, needles []string) bool {
	if line == "" || len(needles) == 0 {
		return false
	}
	lower := strings.ToLower(line)
	for _, needle := range needles {
		if strings.Contains(lower, needle) {
			return true
		}
	}
	return false
}

// sanitizeLogLine redacts the complete line before applying the character
// limit, so truncation cannot preserve a credential prefix while dropping the
// part a pattern needs to recognize.
func sanitizeLogLine(line string, maxCharacters int) string {
	sanitized, _ := sanitizeLogLineWithState(line, maxCharacters)
	return sanitized
}

func sanitizeLogLineWithState(line string, maxCharacters int) (string, bool) {
	line = authorizationHeader.ReplaceAllString(line, `${1}[REDACTED]`)
	line = bearerToken.ReplaceAllString(line, `${1}[REDACTED]`)
	line = githubToken.ReplaceAllString(line, `[REDACTED]`)
	line = awsAccessKey.ReplaceAllString(line, `[REDACTED]`)
	line = googleAPIKey.ReplaceAllString(line, `[REDACTED]`)
	line = slackToken.ReplaceAllString(line, `[REDACTED]`)
	line = urlCredentials.ReplaceAllString(line, `${1}[REDACTED]@`)
	line = secretQuery.ReplaceAllString(line, `${1}[REDACTED]`)
	line = secretAssignment.ReplaceAllString(line, `${1}[REDACTED]`)
	line = stripControls(line)
	truncated := maxCharacters >= 0 && utf8.RuneCountInString(line) > maxCharacters
	return truncateRunes(line, maxCharacters), truncated
}

func stripControls(value string) string {
	value = ansiEscape.ReplaceAllString(value, "")
	return strings.Map(func(char rune) rune {
		if char == '\t' || !unicode.IsControl(char) && !unicode.Is(unicode.Cf, char) {
			return char
		}
		return -1
	}, value)
}

func truncateRunes(value string, limit int) string {
	if limit < 0 || utf8.RuneCountInString(value) <= limit {
		return value
	}
	if limit == 0 {
		return ""
	}
	runes := []rune(value)
	return string(runes[:limit])
}
