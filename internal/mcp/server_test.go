package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"aid/internal/catalog"
)

// The test binary doubles as a fake AID executable. AID_MCP_HELPER selects
// helper mode; --mode=NAME in argv selects its behavior.
func TestMain(m *testing.M) {
	if os.Getenv("AID_MCP_HELPER") == "1" {
		helperMain()
		return
	}
	os.Exit(m.Run())
}

func helperMain() {
	if path := os.Getenv("AID_MCP_COUNT"); path != "" {
		file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err == nil {
			file.WriteString("x")
			file.Close()
		}
	}
	mode := ""
	for _, arg := range os.Args[1:] {
		if value, ok := strings.CutPrefix(arg, "--mode="); ok {
			mode = value
		}
	}
	result := map[string]any{"schema_version": 1, "complete": true, "summary": map[string]any{"total": 2}, "args": os.Args[1:]}
	switch mode {
	case "incomplete":
		result["complete"] = false
		json.NewEncoder(os.Stdout).Encode(result)
		os.Exit(3)
	case "fail":
		fmt.Fprintln(os.Stderr, "secret diagnostic")
		os.Exit(1)
	case "usage":
		os.Exit(2)
	case "slow":
		time.Sleep(10 * time.Second)
	case "badjson":
		fmt.Println("not json")
		os.Exit(0)
	case "sleepok":
		time.Sleep(300 * time.Millisecond)
	}
	json.NewEncoder(os.Stdout).Encode(result)
	os.Exit(0)
}

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (clock *fakeClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *fakeClock) Advance(d time.Duration) {
	clock.mu.Lock()
	clock.now = clock.now.Add(d)
	clock.mu.Unlock()
}

func collectorTool(id, name, mode string, ttl int) catalog.Tool {
	return catalog.Tool{ID: id, Command: "AID." + id, Collector: &catalog.Collector{Name: name, Description: "test " + name,
		Args: []string{"--json", "--mode=" + mode}, WorkspaceFlag: "--workspace", TTLSeconds: ttl, TimeoutSeconds: 30}}
}

type testServer struct {
	*Server
	clock    *fakeClock
	count    string
	resolved []string
	mu       sync.Mutex
	cwd      string
}

func newTestServer(t *testing.T, tools ...catalog.Tool) *testServer {
	t.Helper()
	dir := t.TempDir()
	ts := &testServer{clock: &fakeClock{now: time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)}, count: filepath.Join(dir, "count"), cwd: dir}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ts.Server = New(Options{Tools: tools, Cwd: dir, Now: ts.clock.Now,
		Env: append(os.Environ(), "AID_MCP_HELPER=1", "AID_MCP_COUNT="+ts.count),
		Resolve: func(command string) (string, error) {
			ts.mu.Lock()
			ts.resolved = append(ts.resolved, command)
			ts.mu.Unlock()
			if command == "AID.missing" {
				return "", os.ErrNotExist
			}
			return self, nil
		}})
	return ts
}

func (ts *testServer) runs() int {
	data, _ := os.ReadFile(ts.count)
	return len(data)
}

// session drives Serve through in-memory pipes.
type session struct {
	t      *testing.T
	in     *io.PipeWriter
	out    *bufio.Scanner
	done   chan error
	closed bool
}

func startSession(t *testing.T, server *Server) *session {
	t.Helper()
	inReader, inWriter := io.Pipe()
	outReader, outWriter := io.Pipe()
	s := &session{t: t, in: inWriter, out: bufio.NewScanner(outReader), done: make(chan error, 1)}
	s.out.Buffer(make([]byte, 0, 64<<10), maxMessageBytes)
	go func() {
		err := server.Serve(context.Background(), inReader, outWriter)
		outWriter.Close()
		s.done <- err
	}()
	t.Cleanup(func() { s.close() })
	return s
}

func (s *session) send(line string) {
	s.t.Helper()
	if _, err := io.WriteString(s.in, line+"\n"); err != nil {
		s.t.Fatal(err)
	}
}

func (s *session) read() map[string]any {
	s.t.Helper()
	lines := make(chan string, 1)
	go func() {
		if s.out.Scan() {
			lines <- s.out.Text()
		}
		close(lines)
	}()
	select {
	case line, ok := <-lines:
		if !ok {
			s.t.Fatal("server closed its output")
		}
		var msg map[string]any
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			s.t.Fatalf("server wrote invalid JSON %q", line)
		}
		if strings.Contains(line, "\n") {
			s.t.Fatal("message contains an embedded newline")
		}
		return msg
	case <-time.After(10 * time.Second):
		s.t.Fatal("timed out waiting for a server message")
	}
	return nil
}

func (s *session) close() {
	if s.closed {
		return
	}
	s.closed = true
	s.in.Close()
	select {
	case err := <-s.done:
		if err != nil {
			s.t.Errorf("Serve returned %v", err)
		}
	case <-time.After(10 * time.Second):
		s.t.Error("Serve did not return after stdin closed")
	}
}

func toolText(t *testing.T, msg map[string]any) (map[string]any, bool) {
	t.Helper()
	result, ok := msg["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result in %v", msg)
	}
	content := result["content"].([]any)[0].(map[string]any)
	var payload map[string]any
	if content["type"] != "text" || json.Unmarshal([]byte(content["text"].(string)), &payload) != nil {
		t.Fatalf("content = %v", content)
	}
	return payload, result["isError"] == true
}

func TestFramingAndMethodSet(t *testing.T) {
	ts := newTestServer(t, collectorTool("queue", "github_queue", "ok", 60), catalog.Tool{ID: "plain", Command: "AID.plain"})
	s := startSession(t, ts.Server)
	s.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`)
	init := s.read()
	result := init["result"].(map[string]any)
	if init["id"] != 1.0 || result["protocolVersion"] != "2025-03-26" || result["capabilities"].(map[string]any)["tools"] == nil ||
		result["serverInfo"].(map[string]any)["name"] != "AID" {
		t.Fatalf("initialize = %v", init)
	}
	s.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	s.send(`{"jsonrpc":"2.0","id":"p","method":"ping"}`)
	if ping := s.read(); ping["id"] != "p" || !reflect.DeepEqual(ping["result"], map[string]any{}) {
		t.Errorf("ping = %v", ping)
	}
	s.send(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	list := s.read()["result"].(map[string]any)["tools"].([]any)
	var names []string
	for _, tool := range list {
		definition := tool.(map[string]any)
		names = append(names, definition["name"].(string))
		if definition["inputSchema"].(map[string]any)["type"] != "object" {
			t.Errorf("tool %v lacks an object schema", definition["name"])
		}
	}
	if !reflect.DeepEqual(names, []string{"stats", "collector", "pr_delta"}) {
		t.Errorf("tools = %v", names)
	}
	collectorSchema := list[1].(map[string]any)["inputSchema"].(map[string]any)["properties"].(map[string]any)["name"].(map[string]any)
	if !reflect.DeepEqual(collectorSchema["enum"], []any{"github_queue"}) {
		t.Errorf("collector enum = %v", collectorSchema["enum"])
	}
	for _, test := range []struct {
		line string
		id   any
		code float64
	}{
		{`{"jsonrpc":"2.0","id":3,"method":"resources/list"}`, 3.0, codeMethodNotFound},
		{`{not json`, nil, codeParseError},
		{`{"jsonrpc":"1.0","id":4,"method":"ping"}`, 4.0, codeInvalidRequest},
		{`[{"jsonrpc":"2.0","id":5,"method":"ping"}]`, nil, codeInvalidRequest},
		{`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"nope","arguments":{}}}`, 6.0, codeInvalidParams},
		{`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":"bad"}`, 7.0, codeInvalidParams},
	} {
		s.send(test.line)
		reply := s.read()
		rpcErr, ok := reply["error"].(map[string]any)
		if !ok || rpcErr["code"] != test.code || reply["id"] != test.id {
			t.Errorf("%s -> %v", test.line, reply)
		}
	}
	s.send(`{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"stats","arguments":{}}}`)
	payload, isError := toolText(t, s.read())
	collectors := payload["collectors"].([]any)
	if isError || len(collectors) != 1 {
		t.Fatalf("stats = %v", payload)
	}
	envelope := collectors[0].(map[string]any)
	if envelope["name"] != "github_queue" || envelope["exit_code"] != 0.0 || envelope["complete"] != true ||
		envelope["error"] != nil || envelope["summary"].(map[string]any)["total"] != 2.0 || envelope["result"] != nil {
		t.Errorf("envelope = %v", envelope)
	}
	// An unanswered request is still answered after stdin closes.
	s.send(`{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"collector","arguments":{"name":"github_queue","refresh":true}}}`)
	s.in.Close()
	payload, isError = toolText(t, s.read())
	if isError || payload["result"] == nil || payload["cached"] != false {
		t.Errorf("collector = %v", payload)
	}
}

func TestInitializeVersionNegotiation(t *testing.T) {
	for requested, want := range map[string]string{
		"2025-06-18": "2025-06-18", "2025-03-26": "2025-03-26", "2024-11-05": "2024-11-05",
		"2025-11-25": "2025-11-25", "1999-01-01": supportedVersions[0], "": supportedVersions[0],
	} {
		ts := newTestServer(t)
		s := startSession(t, ts.Server)
		s.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"` + requested + `","capabilities":{}}}`)
		if got := s.read()["result"].(map[string]any)["protocolVersion"]; got != want {
			t.Errorf("requested %q got %v want %s", requested, got, want)
		}
		s.send(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"stats","arguments":{}}}`)
		_, structured := s.read()["result"].(map[string]any)["structuredContent"]
		if structured != (want >= "2025-06-18") {
			t.Errorf("version %s structuredContent = %v", want, structured)
		}
		s.close()
	}
}

func TestCollectorDiscoverySkipsToolsWithoutBlock(t *testing.T) {
	server := New(Options{Tools: []catalog.Tool{
		{ID: "pr", Command: "AID.pr"},
		{ID: "pr-delta", Command: "AID.pr-delta"},
		collectorTool("queue", "github_queue", "ok", 60),
	}, Cwd: t.TempDir()})
	if len(server.collectors) != 1 || server.collectors[0].Command != "AID.queue" || server.collectors[0].TTL != time.Minute ||
		server.prDelta != "AID.pr-delta" {
		t.Errorf("collectors = %+v prDelta %q", server.collectors, server.prDelta)
	}
	tools, err := catalog.All()
	if err != nil {
		t.Fatal(err)
	}
	embedded := New(Options{Tools: tools, Cwd: t.TempDir()})
	if len(embedded.collectors) != 1 || embedded.collectors[0].Name != "github_review_queue" || embedded.collectors[0].Command != "AID.review-queue" {
		t.Errorf("embedded collectors = %+v", embedded.collectors)
	}
}

func TestCacheTTLRefreshAndCoalescing(t *testing.T) {
	ts := newTestServer(t, collectorTool("queue", "github_queue", "sleepok", 60))
	spec := ts.collectors[0]
	ctx := context.Background()
	first := ts.runCollector(ctx, spec, false, true)
	if first.Cached || ts.runs() != 1 || first.Error != nil {
		t.Fatalf("first = %+v runs %d", first, ts.runs())
	}
	ts.clock.Advance(30 * time.Second)
	second := ts.runCollector(ctx, spec, false, true)
	if !second.Cached || ts.runs() != 1 || second.AgeSeconds != 30 || string(second.Result) != string(first.Result) {
		t.Errorf("second cached %v runs %d age %v", second.Cached, ts.runs(), second.AgeSeconds)
	}
	if refreshed := ts.runCollector(ctx, spec, true, true); refreshed.Cached || ts.runs() != 2 {
		t.Errorf("refresh cached %v runs %d", refreshed.Cached, ts.runs())
	}
	ts.clock.Advance(61 * time.Second)
	if expired := ts.runCollector(ctx, spec, false, true); expired.Cached || ts.runs() != 3 {
		t.Errorf("expired cached %v runs %d", expired.Cached, ts.runs())
	}
	ts.clock.Advance(61 * time.Second)
	var wait sync.WaitGroup
	results := make([]Envelope, 5)
	for index := range results {
		wait.Add(1)
		go func() {
			defer wait.Done()
			results[index] = ts.runCollector(ctx, spec, false, false)
		}()
	}
	wait.Wait()
	if ts.runs() != 4 {
		t.Errorf("coalesced runs = %d, want 4", ts.runs())
	}
	for _, result := range results {
		if result.Error != nil || !result.Complete {
			t.Errorf("coalesced result = %+v", result)
		}
	}
	// A different workspace root is a different cache key.
	ts.Server.opts.Cwd = t.TempDir()
	if other := ts.runCollector(ctx, spec, false, true); other.Cached || ts.runs() != 5 {
		t.Errorf("other root cached %v runs %d", other.Cached, ts.runs())
	}
}

func TestErrorEnvelopes(t *testing.T) {
	for _, test := range []struct {
		mode     string
		code     string
		exit     any
		complete bool
	}{
		{"ok", "", 0.0, true},
		{"incomplete", "", 3.0, false},
		{"fail", "failed", 1.0, false},
		{"usage", "usage", 2.0, false},
		{"badjson", "invalid_json", 0.0, false},
		{"slow", "timeout", nil, false},
	} {
		t.Run(test.mode, func(t *testing.T) {
			tool := collectorTool("queue", "github_queue", test.mode, 60)
			ts := newTestServer(t, tool)
			if test.mode == "slow" {
				ts.collectors[0].Timeout = 2 * time.Second
			}
			result, rpcErr := ts.callTool(context.Background(), "collector", map[string]json.RawMessage{"name": json.RawMessage(`"github_queue"`)})
			if rpcErr != nil {
				t.Fatal(rpcErr)
			}
			payload, isError := toolText(t, map[string]any{"result": roundTrip(t, result)})
			errValue, _ := payload["error"].(map[string]any)
			code, _ := errValue["code"].(string)
			if code != test.code || isError != (test.code != "") || payload["complete"] != test.complete {
				t.Errorf("payload = %v isError %v", payload, isError)
			}
			if test.exit != nil && payload["exit_code"] != test.exit {
				t.Errorf("exit_code = %v", payload["exit_code"])
			}
			if strings.Contains(fmt.Sprint(payload), "secret diagnostic") {
				t.Error("stderr leaked into the envelope")
			}
		})
	}
	ts := newTestServer(t, collectorTool("missing", "github_missing", "ok", 60))
	envelope := ts.runCollector(context.Background(), ts.collectors[0], false, true)
	if envelope.Error == nil || envelope.Error.Code != "exec_failed" || envelope.ExitCode != nil {
		t.Errorf("missing executable = %+v", envelope)
	}
}

func roundTrip(t *testing.T, value any) map[string]any {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestPRDeltaArgumentValidation(t *testing.T) {
	ts := newTestServer(t, catalog.Tool{ID: "pr-delta", Command: "AID.pr-delta"})
	for name, args := range map[string]string{
		"semicolon":          `{"repo":"acme/widget; rm -rf /","pr":7}`,
		"substitution":       `{"repo":"$(id)/widget","pr":7}`,
		"pipe":               `{"repo":"acme/widget|cat","pr":7}`,
		"dot name":           `{"repo":"acme/..","pr":7}`,
		"leading dash owner": `{"repo":"-acme/widget","pr":7}`,
		"three segments":     `{"repo":"acme/widget/x","pr":7}`,
		"missing pr":         `{"repo":"acme/widget"}`,
		"string pr":          `{"repo":"acme/widget","pr":"7"}`,
		"zero pr":            `{"repo":"acme/widget","pr":0}`,
		"negative pr":        `{"repo":"acme/widget","pr":-1}`,
		"fraction pr":        `{"repo":"acme/widget","pr":1.5}`,
		"huge pr":            `{"repo":"acme/widget","pr":1e12}`,
		"space reviewer":     `{"repo":"acme/widget","pr":7,"reviewer":"a b"}`,
		"flag reviewer":      `{"repo":"acme/widget","pr":7,"reviewer":"--gh=/bin/sh"}`,
		"backtick reviewer":  "{\"repo\":\"acme/widget\",\"pr\":7,\"reviewer\":\"`id`\"}",
		"non-hex sha":        `{"repo":"acme/widget","pr":7,"since_commit":"zzzzzzz"}`,
		"short sha":          `{"repo":"acme/widget","pr":7,"since_commit":"abc"}`,
		"newline sha":        `{"repo":"acme/widget","pr":7,"since_commit":"abcdef1\nx"}`,
		"unknown key":        `{"repo":"acme/widget","pr":7,"gh":"/bin/sh"}`,
	} {
		var arguments map[string]json.RawMessage
		if err := json.Unmarshal([]byte(args), &arguments); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		result, rpcErr := ts.callTool(context.Background(), "pr_delta", arguments)
		if rpcErr != nil {
			t.Fatalf("%s: protocol error %v", name, rpcErr)
		}
		payload, isError := toolText(t, map[string]any{"result": roundTrip(t, result)})
		if !isError || payload["error"].(map[string]any)["code"] != "invalid_arguments" {
			t.Errorf("%s: accepted, payload %v", name, payload)
		}
	}
	if len(ts.resolved) != 0 {
		t.Fatalf("invalid arguments resolved executables %v", ts.resolved)
	}
	var arguments map[string]json.RawMessage
	json.Unmarshal([]byte(`{"repo":"acme/widget.go","pr":7,"reviewer":"octo[bot]","since_commit":"ABCDEF1"}`), &arguments)
	result, _ := ts.callTool(context.Background(), "pr_delta", arguments)
	payload, isError := toolText(t, map[string]any{"result": roundTrip(t, result)})
	args := payload["result"].(map[string]any)["args"]
	want := []any{"--json", "--repo", "acme/widget.go", "--pr", "7", "--reviewer", "octo[bot]", "--since-commit", "ABCDEF1"}
	if isError || !reflect.DeepEqual(args, want) || payload["name"] != "pr_delta" || payload["cached"] != false {
		t.Errorf("valid pr_delta payload = %v", payload)
	}
	for _, bad := range []string{`{"refresh":"yes"}`, `{"other":true}`} {
		json.Unmarshal([]byte(bad), &arguments)
		result, _ := ts.callTool(context.Background(), "stats", arguments)
		if _, isError := toolText(t, map[string]any{"result": roundTrip(t, result)}); !isError {
			t.Errorf("stats accepted %s", bad)
		}
	}
	result, _ = ts.callTool(context.Background(), "collector", map[string]json.RawMessage{"name": json.RawMessage(`"nope"`)})
	if _, isError := toolText(t, map[string]any{"result": roundTrip(t, result)}); !isError {
		t.Error("collector accepted an unknown name")
	}
}

func TestRootsListAndCwdFallback(t *testing.T) {
	root := t.TempDir()
	ts := newTestServer(t, collectorTool("queue", "github_queue", "ok", 60))
	s := startSession(t, ts.Server)
	s.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{"roots":{"listChanged":true}}}}`)
	s.read()
	s.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	request := s.read()
	if request["method"] != "roots/list" || request["id"] == nil {
		t.Fatalf("expected roots/list request, got %v", request)
	}
	// The call waits for the roots response, which arrives afterwards.
	s.send(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"collector","arguments":{"name":"github_queue"}}}`)
	uri := (&url.URL{Scheme: "file", Path: filepath.ToSlash(root)}).String()
	reply, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": request["id"], "result": map[string]any{
		"roots": []any{map[string]any{"uri": "https://example.com/x"}, map[string]any{"uri": uri, "name": "project"}}}})
	s.send(string(reply))
	payload, isError := toolText(t, s.read())
	workspace := payload["workspace"].(map[string]any)
	args := payload["result"].(map[string]any)["args"].([]any)
	if isError || workspace["source"] != "roots" || workspace["root"] != root || args[len(args)-1] != root || args[len(args)-2] != "--workspace" {
		t.Errorf("roots payload workspace %v args %v", workspace, args)
	}
	// A list_changed notification triggers a new request; an error reply
	// falls back to the working directory.
	s.send(`{"jsonrpc":"2.0","method":"notifications/roots/list_changed"}`)
	again := s.read()
	if again["method"] != "roots/list" {
		t.Fatalf("expected a second roots/list, got %v", again)
	}
	failure, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": again["id"], "error": map[string]any{"code": -32601, "message": "no"}})
	s.send(string(failure))
	s.send(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"stats","arguments":{}}}`)
	payload, _ = toolText(t, s.read())
	envelope := payload["collectors"].([]any)[0].(map[string]any)
	if ws := envelope["workspace"].(map[string]any); ws["source"] != "cwd" || ws["root"] != ts.cwd {
		t.Errorf("fallback workspace = %v", ws)
	}
}

func TestNoRootsCapabilityUsesCwd(t *testing.T) {
	ts := newTestServer(t, collectorTool("queue", "github_queue", "ok", 60))
	s := startSession(t, ts.Server)
	s.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{}}}`)
	s.read()
	s.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	s.send(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"stats","arguments":{"refresh":true}}}`)
	msg := s.read()
	if msg["method"] != nil {
		t.Fatalf("server sent a request to a client without roots: %v", msg)
	}
	payload, _ := toolText(t, msg)
	envelope := payload["collectors"].([]any)[0].(map[string]any)
	if ws := envelope["workspace"].(map[string]any); ws["source"] != "cwd" || ws["root"] != ts.cwd {
		t.Errorf("workspace = %v", ws)
	}
}

func TestFileURIPath(t *testing.T) {
	dir := t.TempDir()
	good := (&url.URL{Scheme: "file", Path: filepath.ToSlash(dir)}).String()
	if path, ok := fileURIPath(good); !ok || path != dir {
		t.Errorf("fileURIPath(%q) = %q %v", good, path, ok)
	}
	for _, bad := range []string{"https://example.com/x", "file://remote-host/x", "file:///definitely/missing/dir", "not a uri"} {
		if _, ok := fileURIPath(bad); ok {
			t.Errorf("accepted %q", bad)
		}
	}
}
