package ci

import (
	"strings"
	"testing"

	"aid/internal/github"
)

func TestProcessLogRedactsBeforeTruncationAndPreservesLineReferences(t *testing.T) {
	opts := DefaultOptions()
	opts.MaxLineCharacters = 80
	data := []byte(strings.Join([]string{
		"before",
		"-----BEGIN RSA PRIVATE KEY-----",
		"private-body-secret",
		"-----END RSA PRIVATE KEY-----",
		"Authorization: token opaque-secret-value",
		"error after key",
		"partial token=must-not-survive",
	}, "\n"))
	log := processLog(data, nil, nil, opts, true)
	if log.ExcerptSelectionComplete {
		t.Error("processLog(..., truncated) excerpt_selection_complete = true, want false")
	}
	joined := excerptText(log.Excerpts)
	for _, secret := range []string{"opaque-secret-value", "private-body-secret", "must-not-survive"} {
		if strings.Contains(joined, secret) {
			t.Errorf("processLog() output contains secret %q: %q", secret, joined)
		}
	}
	if !strings.Contains(joined, "Authorization: [REDACTED]") {
		t.Errorf("processLog() output = %q, want redacted authorization header", joined)
	}
	if len(log.Excerpts) == 0 || log.Excerpts[0].StartLine > 6 || log.Excerpts[0].EndLine < 6 {
		t.Errorf("excerpt refs = %+v, want raw line 6 preserved after PEM block", log.Excerpts)
	}
}

func TestProcessLogTailFallbackAndLineLimit(t *testing.T) {
	opts := DefaultOptions()
	opts.MaxLogLines = 3
	opts.MaxExcerptLines = 2
	log := processLog([]byte("one\ntwo\nthree\nfour\nfive\n"), nil, nil, opts, false)
	if log.OmittedLines != 2 || log.ExcerptSelectionComplete {
		t.Errorf("line limit state = omitted:%d selection:%v, want 2/false", log.OmittedLines, log.ExcerptSelectionComplete)
	}
	if len(log.Excerpts) != 1 || log.Excerpts[0].SelectionReason != "tail_fallback" || log.Excerpts[0].StartLine != 2 {
		t.Errorf("tail fallback = %+v, want observed lines 2-3", log.Excerpts)
	}
}

func TestProcessLogLineCharacterLimitMarksSelectionIncomplete(t *testing.T) {
	opts := DefaultOptions()
	opts.MaxLineCharacters = 8
	log := processLog([]byte("prefix-value-error\n"), nil, nil, opts, false)
	if log.TruncatedLines != 1 || log.ExcerptSelectionComplete {
		t.Errorf("character limit state = truncated:%d selection:%v, want 1/false", log.TruncatedLines, log.ExcerptSelectionComplete)
	}
}

func TestProcessLogSelectsBareFAILOutsideTailFallback(t *testing.T) {
	lines := make([]string, 100)
	for index := range lines {
		lines[index] = "ordinary output"
	}
	lines[4] = "FAIL"
	log := processLog([]byte(strings.Join(lines, "\n")), nil, nil, DefaultOptions(), false)
	if len(log.Excerpts) == 0 || log.Excerpts[0].SelectionReason != "error_marker" || log.Excerpts[0].StartLine > 5 || log.Excerpts[0].EndLine < 5 {
		t.Errorf("bare FAIL excerpts = %+v, want error_marker covering line 5", log.Excerpts)
	}
}

func TestUnavailableLogStatesDoNotClaimExpiry(t *testing.T) {
	tests := []struct {
		kind       github.Kind
		wantState  string
		wantReason string
	}{
		{github.KindNotFound, "unavailable", "expired_or_unavailable"},
		{github.KindGone, "unavailable", "gone"},
		{github.KindPermissionDenied, "permission_denied", "permission_denied"},
	}
	for _, test := range tests {
		got := unavailableLog(&github.Error{Kind: test.kind, Message: "safe"})
		if got.State != test.wantState || got.Reason != test.wantReason {
			t.Errorf("unavailableLog(%s) = %s/%s, want %s/%s", test.kind, got.State, got.Reason, test.wantState, test.wantReason)
		}
	}
}

func TestAnnotationNeedlesAreBounded(t *testing.T) {
	annotations := make([]Annotation, 500)
	for index := range annotations {
		annotations[index] = Annotation{Title: "annotation title " + strings.Repeat("x", index%20)}
	}
	needles := normalizedAnnotationNeedles(annotations, 32)
	if len(needles) > 32 {
		t.Fatalf("normalizedAnnotationNeedles() count = %d, want at most 32", len(needles))
	}
	if !containsNeedle("prefix annotation title x suffix", needles) {
		t.Error("containsNeedle() = false for a bounded known annotation")
	}
}

func excerptText(excerpts []LogExcerpt) string {
	var lines []string
	for _, excerpt := range excerpts {
		lines = append(lines, excerpt.Lines...)
	}
	return strings.Join(lines, "\n")
}
