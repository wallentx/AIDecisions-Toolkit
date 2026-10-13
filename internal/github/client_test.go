package github

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

var fakeGHExecutable string

func TestMain(m *testing.M) {
	directory, err := os.MkdirTemp("", "aid-fake-gh-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "create fake gh directory:", err)
		os.Exit(1)
	}
	executable := "fake-gh"
	if runtime.GOOS == "windows" {
		executable += ".exe"
	}
	fakeGHExecutable = filepath.Join(directory, executable)
	command := exec.Command("go", "build", "-o", fakeGHExecutable, "./testdata/fakegh")
	if output, err := command.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build fake gh: %v\n%s", err, output)
		_ = os.RemoveAll(directory)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(directory)
	os.Exit(code)
}

type observation struct {
	Args        []string       `json:"args"`
	Stdin       string         `json:"stdin"`
	Query       string         `json:"query"`
	Variables   map[string]any `json:"variables"`
	Environment struct {
		GHDebugSet bool   `json:"gh_debug_set"`
		DebugSet   bool   `json:"debug_set"`
		PagerSet   bool   `json:"pager_set"`
		Prompt     string `json:"prompt"`
		GHPager    string `json:"gh_pager"`
		NoColor    string `json:"no_color"`
	} `json:"environment"`
}

func TestRESTUsesFixedReadOnlyCommand(t *testing.T) {
	t.Setenv("AID_FAKE_GH_MODE", "inspect-rest")
	t.Setenv("GH_DEBUG", "token=super-secret")
	t.Setenv("DEBUG", "super-secret")
	t.Setenv("PAGER", "unsafe-pager")
	client := Client{Executable: fakeGHExecutable, Host: "GHE.Example.com"}
	var got observation
	endpoint := "repos/acme/widget/pulls/7?per_page=100&state=open"
	if err := client.REST(context.Background(), endpoint, &got); err != nil {
		t.Fatalf("REST(%q) error: %v", endpoint, err)
	}
	wantArgs := []string{
		"api", "--method", "GET", "--hostname", "ghe.example.com",
		"-H", "Accept: application/vnd.github+json",
		"-H", "X-GitHub-Api-Version: 2022-11-28", endpoint,
	}
	if !reflect.DeepEqual(got.Args, wantArgs) {
		t.Errorf("REST(%q) args = %q, want %q", endpoint, got.Args, wantArgs)
	}
	if got.Stdin != "" {
		t.Errorf("REST(%q) stdin = %q, want empty", endpoint, got.Stdin)
	}
	checkSafeEnvironment(t, got)
}

func TestRawRESTUsesFixedReadOnlyCommand(t *testing.T) {
	t.Setenv("AID_FAKE_GH_MODE", "inspect-rest")
	t.Setenv("GH_DEBUG", "token=super-secret")
	t.Setenv("DEBUG", "super-secret")
	t.Setenv("PAGER", "unsafe-pager")
	client := Client{Executable: fakeGHExecutable, Host: "GHE.Example.com"}
	endpoint := "repos/acme/widget/actions/jobs/9/logs"
	data, err := client.RawREST(context.Background(), endpoint)
	if err != nil {
		t.Fatalf("RawREST(%q) error: %v", endpoint, err)
	}
	var got observation
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("RawREST(%q) returned invalid observation: %v", endpoint, err)
	}
	wantArgs := []string{
		"api", "--method", "GET", "--hostname", "ghe.example.com",
		"--allow-escape-sequences",
		"-H", "Accept: application/vnd.github+json",
		"-H", "X-GitHub-Api-Version: 2022-11-28", endpoint,
	}
	if !reflect.DeepEqual(got.Args, wantArgs) {
		t.Errorf("RawREST(%q) args = %q, want %q", endpoint, got.Args, wantArgs)
	}
	if got.Stdin != "" {
		t.Errorf("RawREST(%q) stdin = %q, want empty", endpoint, got.Stdin)
	}
	checkSafeEnvironment(t, got)
}

func TestGraphQLUsesJSONInputWithoutFieldCoercion(t *testing.T) {
	t.Setenv("AID_FAKE_GH_MODE", "inspect-graphql")
	t.Setenv("GH_DEBUG", "super-secret")
	client := Client{Executable: fakeGHExecutable}
	query := "query PullRequest($cursor: String) { repository { name } }"
	variables := map[string]any{"cursor": "@private-file", "literal": "true", "count": 7}
	var got observation
	if err := client.GraphQL(context.Background(), query, variables, &got); err != nil {
		t.Fatalf("GraphQL() error: %v", err)
	}
	wantArgs := []string{
		"api", "--method", "POST", "--hostname", "github.com",
		"-H", "Accept: application/vnd.github+json", "graphql", "--input", "-",
	}
	if !reflect.DeepEqual(got.Args, wantArgs) {
		t.Errorf("GraphQL() args = %q, want %q", got.Args, wantArgs)
	}
	if got.Query != query || got.Variables["cursor"] != "@private-file" || got.Variables["literal"] != "true" || got.Variables["count"] != float64(7) {
		t.Errorf("GraphQL() body = query %q variables %#v", got.Query, got.Variables)
	}
	checkSafeEnvironment(t, got)
}

func checkSafeEnvironment(t *testing.T, got observation) {
	t.Helper()
	environment := got.Environment
	if environment.GHDebugSet || environment.DebugSet || environment.PagerSet {
		t.Errorf("GitHub command inherited diagnostic environment: %+v", environment)
	}
	if environment.Prompt != "1" || environment.GHPager != "cat" || environment.NoColor != "1" {
		t.Errorf("GitHub command environment = %+v, want noninteractive settings", environment)
	}
}

func TestRESTRejectsUnsafeEndpointsBeforeExecution(t *testing.T) {
	client := Client{Executable: filepath.Join(t.TempDir(), "must-not-run")}
	for _, endpoint := range []string{
		"", "/repos/acme/widget", "https://api.github.com/repos/acme/widget",
		"//evil.example/repos/acme/widget", "repos/acme/../widget", "repos/acme/%2e%2e/widget",
		"repos/acme/widget#fragment", "repos/acme/widget?bad=%zz", "repos/acme\\widget", "repos/acme/widget\nnext",
	} {
		var out any
		if err := client.REST(context.Background(), endpoint, &out); err == nil {
			t.Errorf("REST(%q) accepted unsafe endpoint", endpoint)
		}
		if _, err := client.RawREST(context.Background(), endpoint); err == nil {
			t.Errorf("RawREST(%q) accepted unsafe endpoint", endpoint)
		}
	}
}

func TestGraphQLAllowsMutationTextButRejectsMutationOperations(t *testing.T) {
	client := Client{Executable: fakeGHExecutable}
	t.Setenv("AID_FAKE_GH_MODE", "inspect-graphql")
	for _, query := range []string{
		`query Q { field(argument: "mutation") }`,
		"query Q { field } # mutation is only a comment\n",
		`query Q { field(argument: """mutation""") }`,
	} {
		var out observation
		if err := client.GraphQL(context.Background(), query, nil, &out); err != nil {
			t.Errorf("GraphQL(%q) error: %v", query, err)
		}
	}
	for _, query := range []string{
		`mutation M { deleteThing }`,
		`query Q { field } mutation M { deleteThing }`,
		`subscription S { events }`,
		`fragment F on Thing { name } query Q { thing { ...F } }`,
	} {
		if err := client.GraphQL(context.Background(), query, nil, &observation{}); err == nil {
			t.Errorf("GraphQL(%q) accepted non-query document", query)
		}
	}
}

func TestGraphQLPartialDataReturnsSafeError(t *testing.T) {
	t.Setenv("AID_FAKE_GH_MODE", "partial-exit-1")
	var got struct {
		Value string `json:"value"`
	}
	err := (Client{Executable: fakeGHExecutable}).GraphQL(context.Background(), "query Q { value }", nil, &got)
	checkErrorKind(t, err, KindGraphQLError)
	if got.Value != "kept" {
		t.Errorf("GraphQL() partial value = %q, want kept", got.Value)
	}
	if strings.Contains(err.Error(), "super-secret") {
		t.Errorf("GraphQL() error exposed response content: %v", err)
	}
}

func TestGraphQLCommandFailureNeverBecomesSuccess(t *testing.T) {
	t.Setenv("AID_FAKE_GH_MODE", "data-exit-1")
	var got struct {
		Value string `json:"value"`
	}
	err := (Client{Executable: fakeGHExecutable}).GraphQL(context.Background(), "query Q { value }", nil, &got)
	checkErrorKind(t, err, KindCommandFailed)
	if strings.Contains(err.Error(), "super-secret") {
		t.Errorf("GraphQL() error exposed command output: %v", err)
	}
}

func TestSafeFailureKinds(t *testing.T) {
	for _, test := range []struct {
		mode string
		kind Kind
	}{
		{"not-found", KindNotFound},
		{"denied", KindPermissionDenied},
		{"gone", KindCommandFailed},
		{"failed", KindCommandFailed},
		{"malformed", KindInvalidResponse},
		{"null", KindInvalidResponse},
	} {
		t.Run(test.mode, func(t *testing.T) {
			t.Setenv("AID_FAKE_GH_MODE", test.mode)
			var out map[string]any
			err := (Client{Executable: fakeGHExecutable}).REST(context.Background(), "repos/acme/widget", &out)
			checkErrorKind(t, err, test.kind)
			if strings.Contains(err.Error(), "super-secret") {
				t.Errorf("REST() error exposed command or response content: %v", err)
			}
		})
	}
}

func TestResponseLimitsApplyToStdoutAndStderr(t *testing.T) {
	for _, mode := range []string{"oversize", "stderr-oversize"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("AID_FAKE_GH_MODE", mode)
			var out any
			err := (Client{Executable: fakeGHExecutable, MaxResponseBytes: 32}).REST(context.Background(), "repos/acme/widget", &out)
			checkErrorKind(t, err, KindResponseLimit)
		})
	}
}

func TestRawRESTReturnsExactBoundAndTruncatedPrefix(t *testing.T) {
	client := Client{Executable: fakeGHExecutable, MaxResponseBytes: 8}
	t.Setenv("AID_FAKE_GH_MODE", "raw-exact")
	data, err := client.RawREST(context.Background(), "repos/acme/widget/actions/jobs/9/logs")
	if err != nil || string(data) != "12345678" {
		t.Fatalf("RawREST() at exact bound = %q, %v, want %q, nil", data, err, "12345678")
	}

	t.Setenv("AID_FAKE_GH_MODE", "raw-over")
	data, err = client.RawREST(context.Background(), "repos/acme/widget/actions/jobs/9/logs")
	checkErrorKind(t, err, KindResponseLimit)
	if got, want := string(data), "12345678"; got != want {
		t.Errorf("RawREST() truncated prefix = %q, want %q", got, want)
	}

	t.Setenv("AID_FAKE_GH_MODE", "stderr-oversize")
	data, err = client.RawREST(context.Background(), "repos/acme/widget/actions/jobs/9/logs")
	checkErrorKind(t, err, KindResponseLimit)
	if data != nil {
		t.Errorf("RawREST() data with oversized stderr = %q, want nil", data)
	}
}

func TestRawRESTPreservesTerminalEscapeSequences(t *testing.T) {
	t.Setenv("AID_FAKE_GH_MODE", "raw-ansi")
	data, err := (Client{Executable: fakeGHExecutable}).RawREST(
		context.Background(),
		"repos/acme/widget/actions/jobs/9/logs",
	)
	if err != nil {
		t.Fatalf("RawREST() ANSI response error: %v", err)
	}
	if got, want := string(data), "\x1b[31mred\x1b[0m\r\n"; got != want {
		t.Errorf("RawREST() ANSI response = %q, want %q", got, want)
	}
}

func TestRawRESTReportsUnsupportedEscapeFlagSafely(t *testing.T) {
	t.Setenv("AID_FAKE_GH_MODE", "escape-flag-unsupported")
	data, err := (Client{Executable: fakeGHExecutable}).RawREST(
		context.Background(),
		"repos/acme/widget/actions/jobs/9/logs",
	)
	checkErrorKind(t, err, KindCommandFailed)
	if data != nil {
		t.Errorf("RawREST() data with unsupported gh = %q, want nil", data)
	}
	if !strings.Contains(err.Error(), "update gh") {
		t.Errorf("RawREST() unsupported-gh error = %q, want update guidance", err)
	}
	if strings.Contains(err.Error(), "super-secret") {
		t.Errorf("RawREST() unsupported-gh error exposed command output: %v", err)
	}
}

func TestRawRESTClassifiesHTTPStatusWithoutReturningErrorBody(t *testing.T) {
	for _, test := range []struct {
		mode string
		kind Kind
	}{
		{mode: "not-found", kind: KindNotFound},
		{mode: "gone", kind: KindGone},
	} {
		t.Run(test.mode, func(t *testing.T) {
			t.Setenv("AID_FAKE_GH_MODE", test.mode)
			data, err := (Client{Executable: fakeGHExecutable}).RawREST(
				context.Background(),
				"repos/acme/widget/actions/jobs/9/logs",
			)
			checkErrorKind(t, err, test.kind)
			if data != nil {
				t.Errorf("RawREST() data on %s = %q, want nil", test.mode, data)
			}
			if strings.Contains(err.Error(), "super-secret") {
				t.Errorf("RawREST() error exposed response content: %v", err)
			}
		})
	}
}

func TestContextCancellationStopsCommand(t *testing.T) {
	t.Setenv("AID_FAKE_GH_MODE", "sleep")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	var out any
	err := (Client{Executable: fakeGHExecutable}).REST(ctx, "repos/acme/widget", &out)
	checkErrorKind(t, err, KindTimeout)
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Errorf("REST() cancellation took %v, want less than 3s", elapsed)
	}
}

func TestRawRESTContextCancellationStopsCommand(t *testing.T) {
	t.Setenv("AID_FAKE_GH_MODE", "sleep")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	data, err := (Client{Executable: fakeGHExecutable}).RawREST(
		ctx,
		"repos/acme/widget/actions/jobs/9/logs",
	)
	checkErrorKind(t, err, KindTimeout)
	if data != nil {
		t.Errorf("RawREST() data after timeout = %q, want nil", data)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Errorf("RawREST() cancellation took %v, want less than 3s", elapsed)
	}
}

func TestGraphQLRejectsNullEnvelopeButPreservesGitHubError(t *testing.T) {
	t.Setenv("AID_FAKE_GH_MODE", "null-data-error")
	var out any
	err := (Client{Executable: fakeGHExecutable}).GraphQL(context.Background(), "query Q { value }", nil, &out)
	checkErrorKind(t, err, KindGraphQLError)

	for _, body := range [][]byte{nil, []byte("null"), []byte("{}"), []byte(`{"data":null}`), []byte(`{"data":{},"errors":{}}`)} {
		if err := decodeGraphQL(body, &out); errorKind(err) != KindInvalidResponse {
			t.Errorf("decodeGraphQL(%s) error = %v, want %s", body, err, KindInvalidResponse)
		}
	}
}

func TestExecutableFailureDoesNotExposePath(t *testing.T) {
	secretPath := filepath.Join(t.TempDir(), "super-secret-token")
	var out any
	err := (Client{Executable: secretPath}).REST(context.Background(), "repos/acme/widget", &out)
	checkErrorKind(t, err, KindCommandFailed)
	if strings.Contains(err.Error(), "super-secret-token") {
		t.Errorf("REST() error exposed executable path: %v", err)
	}
}

func checkErrorKind(t *testing.T, err error, want Kind) {
	t.Helper()
	if got := errorKind(err); got != want {
		t.Fatalf("error kind = %q from %v, want %q", got, err, want)
	}
}
