package ci

import (
	"strings"
	"testing"
	"unicode"
)

func TestPrivacyAcceptanceHeaderCredentialsAreFullyHidden(t *testing.T) {
	lines := []string{
		"Authorization: Token fake-token-value-should-not-survive",
		"Authorization: Digest username=demo, response=fake-digest-value-should-not-survive",
		"Proxy-Authorization: Basic fake-proxy-value-should-not-survive",
	}

	for _, line := range lines {
		got := sanitizeLogLine(line, 1_000)
		if !strings.HasSuffix(got, "[REDACTED]") {
			t.Errorf("sanitizeLogLine(%q) = %q, want a fully redacted header value", line, got)
		}
		for _, secret := range []string{"fake-token-value-should-not-survive", "fake-digest-value-should-not-survive", "fake-proxy-value-should-not-survive"} {
			if strings.Contains(got, secret) {
				t.Errorf("sanitizeLogLine(%q) exposed %q in %q", line, secret, got)
			}
		}
	}
}

func TestPrivacyAcceptanceAnnotationPEMBlocksHideEverySecretLine(t *testing.T) {
	annotation := Annotation{
		Message: sanitizeText(strings.Join([]string{
			"annotation context",
			"-----BEGIN DEMO PRIVATE KEY-----",
			"FAKE_ANNOTATION_MESSAGE_SECRET_0123456789",
			"-----END DEMO PRIVATE KEY-----",
			"annotation continues",
		}, "\n"), 2_000),
		RawDetails: sanitizeText(strings.Join([]string{
			"details context",
			"-----BEGIN DEMO PRIVATE KEY-----",
			"FAKE_RAW_DETAILS_SECRET_0123456789",
			"-----END DEMO PRIVATE KEY-----",
		}, "\n"), 2_000),
	}

	for name, value := range map[string]string{"message": annotation.Message, "raw_details": annotation.RawDetails} {
		if !strings.Contains(value, "[REDACTED PRIVATE KEY BLOCK]") {
			t.Errorf("annotation %s = %q, want private-key placeholder", name, value)
		}
		for _, secret := range []string{"FAKE_ANNOTATION_MESSAGE_SECRET_0123456789", "FAKE_RAW_DETAILS_SECRET_0123456789"} {
			if strings.Contains(value, secret) {
				t.Errorf("annotation %s exposed %q in %q", name, secret, value)
			}
		}
	}
}

func TestPrivacyAcceptanceLogErrorAfterPEMRetainsRawLineIndices(t *testing.T) {
	opts := DefaultOptions()
	opts.ContextLines = 3
	log := processLog([]byte(strings.Join([]string{
		"setup started",
		"-----BEGIN DEMO PRIVATE KEY-----",
		"FAKE_LOG_PRIVATE_KEY_BODY_0123456789",
		"-----END DEMO PRIVATE KEY-----",
		"error: deployment rejected",
		"cleanup follows",
	}, "\n")), nil, nil, opts, false)

	if len(log.Excerpts) != 1 {
		t.Fatalf("processLog() excerpts = %+v, want one error excerpt", log.Excerpts)
	}
	excerpt := log.Excerpts[0]
	if excerpt.SelectionReason != "error_marker" || excerpt.StartLine > 5 || excerpt.EndLine < 5 {
		t.Errorf("error excerpt = %+v, want raw line 5 selected as an error", excerpt)
	}
	joined := strings.Join(excerpt.Lines, "\n")
	if strings.Contains(joined, "FAKE_LOG_PRIVATE_KEY_BODY_0123456789") {
		t.Errorf("error excerpt exposed PEM body: %q", joined)
	}
	if !strings.Contains(joined, "[REDACTED PRIVATE KEY BLOCK]") {
		t.Errorf("error excerpt = %q, want PEM placeholder", joined)
	}
}

func TestPrivacyAcceptanceTruncatedPrefixDropsPartialFinalLine(t *testing.T) {
	opts := DefaultOptions()
	partial := "partial-private-value-must-not-be-rendered"
	log := processLog([]byte("retained context\nerror: retained marker\n"+partial), nil, nil, opts, true)

	if log.RetrievalComplete || log.ExcerptSelectionComplete {
		t.Errorf("truncated prefix completeness = retrieval:%v excerpts:%v, want both false", log.RetrievalComplete, log.ExcerptSelectionComplete)
	}
	if log.LinesObserved != 2 {
		t.Errorf("truncated prefix lines_observed = %d, want 2 complete lines", log.LinesObserved)
	}
	if strings.Contains(privacyAcceptanceExcerptText(log.Excerpts), partial) {
		t.Errorf("truncated prefix rendered partial final line in %+v", log.Excerpts)
	}
}

func TestPrivacyAcceptanceControlsAndLargeInputStayBounded(t *testing.T) {
	opts := DefaultOptions()
	opts.MaxLogLines = 80
	opts.MaxExcerptLines = 6
	opts.MaxLineCharacters = 120
	largeLog := strings.Repeat("ordinary output\n", 14_000) + "error: fixture failure\n"
	annotationNeedle := strings.Repeat("x", 4_096) + "unreachable-needle"
	log := processLog([]byte(largeLog), []string{annotationNeedle}, []Annotation{{Title: annotationNeedle}}, opts, false)

	if log.LinesObserved != opts.MaxLogLines || log.OmittedLines == 0 || log.ExcerptSelectionComplete {
		t.Errorf("bounded log state = lines:%d omitted:%d complete:%v, want bounded incomplete evidence", log.LinesObserved, log.OmittedLines, log.ExcerptSelectionComplete)
	}
	for _, excerpt := range log.Excerpts {
		if len(excerpt.Lines) > opts.MaxExcerptLines {
			t.Errorf("excerpt has %d lines, want at most %d", len(excerpt.Lines), opts.MaxExcerptLines)
		}
		for _, line := range excerpt.Lines {
			if len([]rune(line)) > opts.MaxLineCharacters {
				t.Errorf("excerpt line length = %d, want at most %d", len([]rune(line)), opts.MaxLineCharacters)
			}
		}
	}

	got := sanitizeLogLine("\x1b[31merror\x1b[0m \u202Ereordered\u200e text", 1_000)
	for _, character := range got {
		if unicode.IsControl(character) || unicode.Is(unicode.Cf, character) {
			t.Errorf("sanitizeLogLine() retained control %U in %q", character, got)
		}
	}
	if !strings.Contains(got, "error reordered text") {
		t.Errorf("sanitizeLogLine() = %q, want readable control-free text", got)
	}
}

func privacyAcceptanceExcerptText(excerpts []LogExcerpt) string {
	lines := make([]string, 0)
	for _, excerpt := range excerpts {
		lines = append(lines, excerpt.Lines...)
	}
	return strings.Join(lines, "\n")
}
