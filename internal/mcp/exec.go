package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"strconv"
	"time"

	"aid/internal/bounded"
)

const (
	maxCollectorOutput = 16 << 20
	maxCollectorStderr = 64 << 10
	commandWaitDelay   = 2 * time.Second
)

func lookPath(name string) (string, error) { return exec.LookPath(name) }

// CollectorError is a safe classification of a failed run. Messages never
// include the executable's stderr or output.
type CollectorError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// runOutcome is one execution of an AID executable.
type runOutcome struct {
	At       time.Time
	ExitCode *int
	Complete bool
	Result   json.RawMessage
	Summary  json.RawMessage
	Error    *CollectorError
}

func (outcome runOutcome) cacheable() bool { return outcome.Error == nil }

// run executes path with argv only (no shell) under timeout and classifies
// the result by exit code and JSON validity.
func run(path string, args, env []string, timeout time.Duration, now func() time.Time) runOutcome {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	stdout := bounded.NewBuffer(maxCollectorOutput, "collector output")
	stderr := bounded.NewBuffer(maxCollectorStderr, "collector stderr")
	command := exec.CommandContext(ctx, path, args...)
	command.Stdout, command.Stderr, command.Env, command.WaitDelay = stdout, stderr, env, commandWaitDelay
	err := command.Run()
	outcome := runOutcome{At: now().UTC()}
	if command.ProcessState != nil {
		code := command.ProcessState.ExitCode()
		if code >= 0 {
			outcome.ExitCode = &code
		}
	}
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		outcome.Error = &CollectorError{"timeout", "collector did not finish within its timeout"}
		return outcome
	case errors.Is(stdout.Err(), bounded.ErrLimit):
		outcome.Error = &CollectorError{"output_limit", "collector output exceeded the size limit"}
		return outcome
	case outcome.ExitCode == nil && command.ProcessState != nil:
		outcome.Error = &CollectorError{"failed", "collector was terminated by a signal"}
		return outcome
	case outcome.ExitCode == nil:
		message := "collector could not be started"
		if errors.Is(err, exec.ErrNotFound) {
			message = "collector executable was not found"
		}
		outcome.Error = &CollectorError{"exec_failed", message}
		return outcome
	}
	switch *outcome.ExitCode {
	case 0, 3:
	case 2:
		outcome.Error = &CollectorError{"usage", "collector rejected its arguments (exit 2)"}
		return outcome
	default:
		outcome.Error = &CollectorError{"failed", "collector failed (exit " + strconv.Itoa(*outcome.ExitCode) + ")"}
		return outcome
	}
	data := bytes.TrimSpace(stdout.Bytes())
	var fields map[string]json.RawMessage
	if len(data) == 0 || data[0] != '{' || json.Unmarshal(data, &fields) != nil {
		outcome.Error = &CollectorError{"invalid_json", "collector stdout was not a JSON object"}
		return outcome
	}
	outcome.Result = json.RawMessage(data)
	var complete bool
	if raw, ok := fields["complete"]; ok && json.Unmarshal(raw, &complete) == nil {
		outcome.Complete = complete && *outcome.ExitCode == 0
	} else {
		outcome.Complete = *outcome.ExitCode == 0
	}
	if summary, ok := fields["summary"]; ok && len(summary) > 0 && summary[0] == '{' {
		outcome.Summary = summary
	}
	return outcome
}
