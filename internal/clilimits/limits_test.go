package clilimits_test

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"aid/tools/ci"
	"aid/tools/discover"
	"aid/tools/pr"
	prdelta "aid/tools/pr-delta"
	reviewqueue "aid/tools/review-queue"
	runtimeTool "aid/tools/runtime"
)

type cli func([]string, io.Reader, io.Writer, io.Writer) int
type bound struct {
	flag     string
	min, max int64
	noMax    bool
}

// These boundary expectations are independent of the production tables.
func TestLimitDiagnosticsAndHelp(t *testing.T) {
	tools := []struct {
		name   string
		run    cli
		args   []string
		bounds []bound
	}{
		{"discover", discover.Run, nil, []bound{
			{"days", 1, 36500, false}, {"offset", 0, 0, true}, {"page-size", 1, 100, false},
			{"min-sessions", 1, 0, true}, {"text-offset", 0, 0, true}, {"text-limit", 1, 12000, false},
			{"max-output-bytes", 4096, 1048576, false},
		}},
		{"pr", pr.Run, []string{"--repo", "owner/repo", "--pr", "1"}, []bound{
			{"page-size", 1, 100, false}, {"max-pages", 1, 1000, false}, {"max-items", 1, 100000, false},
			{"max-comments", 1, 100000, false}, {"max-requests", 2, 10000, false},
			{"max-retained-bytes", 4096, 67108864, false}, {"max-response-bytes", 4096, 33554432, false},
			{"max-output-bytes", 4096, 16777216, false}, {"text-limit", 0, 1048576, false},
		}},
		{"pr-delta", prdelta.Run, []string{"--repo", "owner/repo", "--pr", "1"}, []bound{
			{"max-pages", 1, 100, false}, {"max-requests", 4, 2000, false}, {"max-commit-patches", 0, 500, false},
			{"max-hunks", 1, 1000, false}, {"max-response-bytes", 4096, 33554432, false},
			{"max-output-bytes", 4096, 16777216, false}, {"text-limit", 0, 65536, false},
		}},
		{"review-queue", reviewqueue.Run, []string{"--owner", "owner"}, []bound{
			{"max-prs", 1, 1000, false}, {"delta-concurrency", 1, 16, false}, {"title-limit", 1, 4096, false},
			{"max-response-bytes", 4096, 33554432, false}, {"max-output-bytes", 4096, 16777216, false},
		}},
		{"ci", ci.Run, []string{"--repo", "owner/repo", "--run", "1"}, []bound{
			{"page-size", 1, 100, false}, {"max-pages", 1, 20, false}, {"max-jobs", 1, 500, false},
			{"max-log-jobs", 1, 20, false}, {"max-requests", 2, 200, false}, {"max-retained-bytes", 4096, 16777216, false},
			{"max-annotations", 1, 500, false}, {"max-artifacts", 1, 500, false}, {"max-log-bytes", 4096, 16777216, false},
			{"max-response-bytes", 4096, 33554432, false}, {"max-excerpts", 1, 5, false}, {"context-lines", 0, 3, false},
			{"max-excerpt-lines", 1, 60, false}, {"max-line-characters", 1, 2000, false}, {"max-log-lines", 1, 100000, false},
			{"max-output-bytes", 4096, 16777216, false},
		}},
		{"runtime", runtimeTool.Run, []string{"--file", "/tmp/not-read-by-dry-run"}, []bound{{"max-file-bytes", 0, 2147483648, false}}},
	}
	config := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(config, []byte("version: 1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools {
		t.Run(tool.name, func(t *testing.T) {
			var help bytes.Buffer
			if code := tool.run([]string{"--help"}, nil, io.Discard, &help); code != 0 {
				t.Fatalf("help exit %d", code)
			}
			base := append([]string{"--config", config, "--dry-run"}, tool.args...)
			for _, b := range tool.bounds {
				span := fmt.Sprintf("%d..%d", b.min, b.max)
				if b.noMax {
					span = fmt.Sprintf("at least %d", b.min)
				}
				if !strings.Contains(help.String(), span) {
					t.Errorf("help lacks %s: %s", b.flag, span)
				}
				values := []int64{b.min - 1, b.min}
				if !b.noMax {
					values = append(values, b.max, b.max+1)
				}
				for _, value := range values {
					t.Run(fmt.Sprintf("%s/%d", b.flag, value), func(t *testing.T) {
						var out, stderr bytes.Buffer
						args := append(append([]string{}, base...), "--"+b.flag, strconv.FormatInt(value, 10))
						code := tool.run(args, nil, &out, &stderr)
						valid := value >= b.min && (b.noMax || value <= b.max)
						if valid {
							if code != 0 {
								t.Fatalf("boundary rejected: %d %s", code, &stderr)
							}
							return
						}
						want := fmt.Sprintf("--%s must be %s (got %d)", b.flag, span, value)
						if code != 2 || out.Len() != 0 || !strings.Contains(stderr.String(), want) {
							t.Fatalf("exit=%d stderr=%s; want %s", code, &stderr, want)
						}
					})
				}
			}
			for _, value := range []string{"0", "-1", "3601", "NaN", "+Inf", "-Inf"} {
				var stderr bytes.Buffer
				code := tool.run(append(append([]string{}, base...), "--timeout", value), nil, io.Discard, &stderr)
				if code != 2 || !strings.Contains(stderr.String(), "--timeout must be greater than 0 and at most 3600") {
					t.Errorf("timeout %s: %d %s", value, code, &stderr)
				}
			}
		})
	}
}

func TestDiscoveryConfigLimits(t *testing.T) {
	for _, b := range []bound{{"max_input_bytes", 1024, 67108864, false}, {"max_records", 1, 1000000, false}, {"max_record_bytes", 256, 1048576, false}, {"cache_ttl_seconds", 0, 86400, false}} {
		for _, value := range []string{strconv.FormatInt(b.min-1, 10), strconv.FormatInt(b.max+1, 10)} {
			t.Run(b.flag+"/"+value, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "limits.yml")
				body := fmt.Sprintf("version: 1\ntools:\n  discover:\n    %s: %s\n", b.flag, value)
				if err := os.WriteFile(path, []byte(body), 0600); err != nil {
					t.Fatal(err)
				}
				var stderr bytes.Buffer
				code := discover.Run([]string{"--config", path, "--dry-run"}, nil, io.Discard, &stderr)
				for _, want := range []string{"tools.discover." + b.flag, path, fmt.Sprintf("%d..%d", b.min, b.max), "got " + value} {
					if code != 2 || !strings.Contains(stderr.String(), want) {
						t.Errorf("exit=%d stderr=%s; want %s", code, &stderr, want)
					}
				}
			})
		}
	}
}

func TestDiscoveryCacheDuration(t *testing.T) {
	config := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(config, []byte("version: 1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"-1ns", "0s", "48h"} {
		var stderr bytes.Buffer
		code := discover.Run([]string{"--config", config, "--dry-run", "--cache-ttl", value}, nil, io.Discard, &stderr)
		if value == "-1ns" {
			if code != 2 || !strings.Contains(stderr.String(), "--cache-ttl must be at least 0") {
				t.Fatalf("%d %s", code, &stderr)
			}
		} else if code != 0 {
			t.Fatalf("%s rejected: %d %s", value, code, &stderr)
		}
	}
}
