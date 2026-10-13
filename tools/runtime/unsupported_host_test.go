//go:build !linux && !android

package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestUnsupportedHostReturnsBoundedEvidence(t *testing.T) {
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("not executed"), 0600); err != nil {
		t.Fatal(err)
	}
	report, err := Collect(context.Background(), target, nil, Options{})
	if err != nil || report.Complete || report.Status != "unavailable" || report.Issue == nil {
		t.Fatalf("report=%+v err=%v", report, err)
	}
	packet, complete, err := Render(report)
	if err != nil || complete || !json.Valid(packet) || len(packet) > MaximumOutputBytes {
		t.Fatalf("complete=%v err=%v packet=%s", complete, err, packet)
	}
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"--version"}, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("version exit=%d stderr=%s", code, &stderr)
	}
}
