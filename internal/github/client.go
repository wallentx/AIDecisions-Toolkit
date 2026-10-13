// Package github provides the read-only GitHub transport used by AID tools.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"aid/internal/bounded"
)

const (
	defaultHost                = "github.com"
	defaultMaxResponseBytes    = 4 << 20
	maximumMaxResponseBytes    = 64 << 20
	maximumGraphQLRequestBytes = 1 << 20
	commandWaitDelay           = 2 * time.Second
)

// Kind identifies a GitHub failure without exposing command output or response
// content.
type Kind string

const (
	KindTimeout          Kind = "timeout"
	KindPermissionDenied Kind = "permission_denied"
	KindNotFound         Kind = "not_found"
	KindGone             Kind = "gone"
	KindResponseLimit    Kind = "response_limit"
	KindInvalidResponse  Kind = "invalid_response"
	KindGraphQLError     Kind = "graphql_error"
	KindCommandFailed    Kind = "command_failed"
)

// Error is safe to display. Message never contains response bodies, stderr, or
// errors returned by the gh process.
type Error struct {
	Kind    Kind
	Message string
}

func (e *Error) Error() string { return e.Message }

// Client invokes the authenticated gh CLI using read-only API operations.
type Client struct {
	Executable       string
	Host             string
	MaxResponseBytes int64
}

// REST performs a GET against a relative GitHub API endpoint and decodes its
// JSON response into out.
func (client Client) REST(ctx context.Context, endpoint string, out any) error {
	host, err := normalizeHost(client.Host)
	if err != nil {
		return err
	}
	if err := validateEndpoint(endpoint); err != nil {
		return err
	}

	data, err := client.run(ctx, nil,
		"api",
		"--method", "GET",
		"--hostname", host,
		"-H", "Accept: application/vnd.github+json",
		"-H", "X-GitHub-Api-Version: 2022-11-28",
		endpoint,
	)
	if err != nil {
		return err
	}
	if isJSONNull(data) || json.Unmarshal(data, out) != nil {
		return safeError(KindInvalidResponse)
	}
	return nil
}

// RawREST performs a GET against a relative GitHub API endpoint and returns
// the response without decoding it. If the response exceeds the configured
// limit, the returned data is the bounded prefix and the error has
// KindResponseLimit. Raw response capture requires a gh version that supports
// --allow-escape-sequences.
func (client Client) RawREST(ctx context.Context, endpoint string) ([]byte, error) {
	host, err := normalizeHost(client.Host)
	if err != nil {
		return nil, err
	}
	if err := validateEndpoint(endpoint); err != nil {
		return nil, err
	}

	return client.runRaw(ctx,
		"api",
		"--method", "GET",
		"--hostname", host,
		"--allow-escape-sequences",
		"-H", "Accept: application/vnd.github+json",
		"-H", "X-GitHub-Api-Version: 2022-11-28",
		endpoint,
	)
}

// GraphQL performs a query operation. When GitHub returns both data and errors,
// data is decoded into out before a KindGraphQLError is returned.
func (client Client) GraphQL(ctx context.Context, query string, variables map[string]any, out any) error {
	if err := validateQuery(query); err != nil {
		return err
	}
	host, err := normalizeHost(client.Host)
	if err != nil {
		return err
	}
	if variables == nil {
		variables = map[string]any{}
	}

	input := bounded.NewBuffer(maximumGraphQLRequestBytes, "GraphQL request")
	encoder := json.NewEncoder(input)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}{Query: query, Variables: variables}); err != nil || input.Err() != nil {
		return errors.New("invalid GraphQL request")
	}

	data, err := client.run(ctx, bytes.NewReader(input.Bytes()),
		"api",
		"--method", "POST",
		"--hostname", host,
		"-H", "Accept: application/vnd.github+json",
		"graphql",
		"--input", "-",
	)
	if err != nil {
		var transportError *Error
		if errors.As(err, &transportError) && transportError.Kind == KindCommandFailed {
			if responseErr := decodeGraphQL(data, out); errorKind(responseErr) == KindGraphQLError {
				return responseErr
			}
		}
		return err
	}
	return decodeGraphQL(data, out)
}

func (client Client) run(ctx context.Context, input io.Reader, args ...string) ([]byte, error) {
	data, err := client.runCommand(ctx, input, classifyCommandError, args...)
	if errorKind(err) == KindResponseLimit {
		return nil, err
	}
	return data, err
}

func (client Client) runRaw(ctx context.Context, args ...string) ([]byte, error) {
	data, err := client.runCommand(ctx, nil, classifyRawCommandError, args...)
	if err != nil && errorKind(err) != KindResponseLimit {
		return nil, err
	}
	return data, err
}

func (client Client) runCommand(
	ctx context.Context,
	input io.Reader,
	classifyError func([]byte) error,
	args ...string,
) ([]byte, error) {
	limit, err := client.responseLimit()
	if err != nil {
		return nil, err
	}
	executable := client.Executable
	if executable == "" {
		executable = "gh"
	}

	stdout := bounded.NewBuffer(limit, "GitHub response")
	stderr := bounded.NewBuffer(limit, "GitHub error response")
	command := exec.CommandContext(ctx, executable, args...)
	command.Stdin = input
	command.Stdout = stdout
	command.Stderr = stderr
	command.Env = githubEnvironment(os.Environ())
	command.WaitDelay = commandWaitDelay
	runErr := command.Run()

	data := append([]byte(nil), stdout.Bytes()...)
	if errors.Is(stdout.Err(), bounded.ErrLimit) {
		return data, safeError(KindResponseLimit)
	}
	if errors.Is(stderr.Err(), bounded.ErrLimit) {
		return nil, safeError(KindResponseLimit)
	}
	if ctx.Err() != nil {
		return nil, safeError(KindTimeout)
	}
	if runErr != nil {
		return data, classifyError(stderr.Bytes())
	}
	return data, nil
}

func errorKind(err error) Kind {
	var githubError *Error
	if errors.As(err, &githubError) {
		return githubError.Kind
	}
	return ""
}

func (client Client) responseLimit() (int64, error) {
	limit := client.MaxResponseBytes
	if limit == 0 {
		limit = defaultMaxResponseBytes
	}
	if limit < 1 || limit > maximumMaxResponseBytes {
		return 0, errors.New("invalid GitHub response limit")
	}
	return limit, nil
}

func classifyCommandError(stderr []byte) error {
	text := strings.ToUpper(string(stderr))
	switch {
	case strings.Contains(text, "HTTP 401"), strings.Contains(text, "HTTP 403"):
		return safeError(KindPermissionDenied)
	case strings.Contains(text, "HTTP 404"):
		return safeError(KindNotFound)
	default:
		return safeError(KindCommandFailed)
	}
}

func classifyRawCommandError(stderr []byte) error {
	text := strings.ToUpper(string(stderr))
	if strings.Contains(text, "UNKNOWN FLAG: --ALLOW-ESCAPE-SEQUENCES") {
		return &Error{
			Kind:    KindCommandFailed,
			Message: "GitHub CLI does not support safe raw response capture; update gh",
		}
	}
	if strings.Contains(text, "HTTP 410") {
		return safeError(KindGone)
	}
	return classifyCommandError(stderr)
}

func safeError(kind Kind) *Error {
	messages := map[Kind]string{
		KindTimeout:          "GitHub request timed out or was canceled",
		KindPermissionDenied: "GitHub denied the request",
		KindNotFound:         "GitHub resource was not found",
		KindGone:             "GitHub resource is no longer available",
		KindResponseLimit:    "GitHub response exceeded the configured limit",
		KindInvalidResponse:  "GitHub returned an invalid response",
		KindGraphQLError:     "GitHub GraphQL response contained errors",
		KindCommandFailed:    "GitHub request failed",
	}
	return &Error{Kind: kind, Message: messages[kind]}
}

func githubEnvironment(environment []string) []string {
	blocked := map[string]bool{
		"CLICOLOR_FORCE":     true,
		"DEBUG":              true,
		"GH_DEBUG":           true,
		"GH_FORCE_TTY":       true,
		"GH_PAGER":           true,
		"GH_PROMPT_DISABLED": true,
		"NO_COLOR":           true,
		"PAGER":              true,
	}
	cleaned := make([]string, 0, len(environment)+3)
	for _, entry := range environment {
		key, _, _ := strings.Cut(entry, "=")
		if !blocked[strings.ToUpper(key)] {
			cleaned = append(cleaned, entry)
		}
	}
	return append(cleaned, "GH_PROMPT_DISABLED=1", "GH_PAGER=cat", "NO_COLOR=1")
}

type graphQLEnvelope struct {
	Data   json.RawMessage `json:"data"`
	Errors json.RawMessage `json:"errors"`
}

func decodeGraphQL(data []byte, out any) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return safeError(KindInvalidResponse)
	}

	var envelope graphQLEnvelope
	if json.Unmarshal(trimmed, &envelope) != nil {
		return safeError(KindInvalidResponse)
	}

	hasErrors := false
	if len(envelope.Errors) != 0 && !isJSONNull(envelope.Errors) {
		var responseErrors []json.RawMessage
		if json.Unmarshal(envelope.Errors, &responseErrors) != nil {
			return safeError(KindInvalidResponse)
		}
		hasErrors = len(responseErrors) != 0
	}

	if len(envelope.Data) != 0 && !isJSONNull(envelope.Data) {
		if json.Unmarshal(envelope.Data, out) != nil {
			return safeError(KindInvalidResponse)
		}
	} else if !hasErrors {
		return safeError(KindInvalidResponse)
	}
	if hasErrors {
		return safeError(KindGraphQLError)
	}
	return nil
}

func isJSONNull(data []byte) bool {
	return bytes.Equal(bytes.TrimSpace(data), []byte("null"))
}
