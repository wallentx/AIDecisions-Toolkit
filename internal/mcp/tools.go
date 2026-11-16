package mcp

import (
	"context"
	"encoding/json"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Envelope is the result of one collector or pr_delta run.
type Envelope struct {
	Name       string          `json:"name"`
	Command    string          `json:"command"`
	AsOf       *time.Time      `json:"as_of"`
	AgeSeconds float64         `json:"age_seconds"`
	Cached     bool            `json:"cached"`
	ExitCode   *int            `json:"exit_code"`
	Complete   bool            `json:"complete"`
	Error      *CollectorError `json:"error"`
	Summary    json.RawMessage `json:"summary"`
	Workspace  *workspace      `json:"workspace,omitempty"`
	Result     json.RawMessage `json:"result,omitempty"`
}

func (s *Server) toolDefinitions() []map[string]any {
	names := make([]string, 0, len(s.collectors))
	for _, collector := range s.collectors {
		names = append(names, collector.Name)
	}
	sort.Strings(names)
	readOnly := map[string]any{"readOnlyHint": true, "openWorldHint": true}
	refresh := map[string]any{"type": "boolean", "description": "Bypass the cache and run now."}
	collectorName := map[string]any{"type": "string", "description": "Collector name from stats."}
	if len(names) > 0 {
		collectorName["enum"] = names
	}
	return []map[string]any{
		{
			"name": "stats",
			"description": "Latest result envelope for every AID collector (name, as_of, age_seconds, cached, exit_code, " +
				"complete, error, summary). Cheap first call; results are cached per collector and workspace root.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"refresh": refresh}, "additionalProperties": false},
			"annotations": readOnly,
		},
		{
			"name":        "collector",
			"description": "One collector's full JSON result inside the same envelope.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"name": collectorName, "refresh": refresh},
				"required": []string{"name"}, "additionalProperties": false},
			"annotations": readOnly,
		},
		{
			"name":        "pr_delta",
			"description": "Run AID.pr-delta --json once (not cached): what changed in a pull request since a reviewer's last review.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
				"repo":         map[string]any{"type": "string", "description": "OWNER/NAME", "pattern": repoPattern.String()},
				"pr":           map[string]any{"type": "integer", "minimum": 1},
				"reviewer":     map[string]any{"type": "string", "description": "GitHub login; default is the authenticated gh user"},
				"since_commit": map[string]any{"type": "string", "description": "7 to 40 hexadecimal characters", "pattern": shaPattern.String()},
			}, "required": []string{"repo", "pr"}, "additionalProperties": false},
			"annotations": readOnly,
		},
	}
}

var (
	repoPattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}/[A-Za-z0-9._-]{1,100}$`)
	shaPattern   = regexp.MustCompile(`^[0-9a-fA-F]{7,40}$`)
	loginPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}(\[bot\])?$`)
)

type argumentError struct{ message string }

func (s *Server) callTool(ctx context.Context, name string, args map[string]json.RawMessage) (any, *rpcError) {
	var payload any
	var failed bool
	var argErr *argumentError
	switch name {
	case "stats":
		var refresh bool
		if refresh, argErr = parseRefresh(args, "refresh"); argErr == nil {
			payload = s.stats(ctx, refresh)
		}
	case "collector":
		var collectorName string
		var refresh bool
		if collectorName, refresh, argErr = parseCollectorArgs(args); argErr == nil {
			spec, ok := s.collector(collectorName)
			if !ok {
				argErr = &argumentError{"unknown collector; call stats for the available names"}
				break
			}
			envelope := s.runCollector(ctx, spec, refresh, true)
			payload, failed = envelope, envelope.Error != nil
		}
	case "pr_delta":
		var argv []string
		if argv, argErr = prDeltaArgs(args); argErr == nil {
			envelope := s.runPRDelta(argv)
			payload, failed = envelope, envelope.Error != nil
		}
	default:
		return nil, &rpcError{codeInvalidParams, "unknown tool: " + truncate(name, 64)}
	}
	if argErr != nil {
		payload = map[string]any{"error": CollectorError{"invalid_arguments", argErr.message}}
		failed = true
	}
	text, err := json.Marshal(payload)
	if err != nil {
		return nil, &rpcError{codeInternalError, "cannot encode tool result"}
	}
	result := map[string]any{"content": []map[string]any{{"type": "text", "text": string(text)}}, "isError": failed}
	s.stateMu.Lock()
	structured := s.version >= "2025-06-18"
	s.stateMu.Unlock()
	if structured {
		result["structuredContent"] = json.RawMessage(text)
	}
	return result, nil
}

func (s *Server) collector(name string) (collectorSpec, bool) {
	for _, spec := range s.collectors {
		if spec.Name == name {
			return spec, true
		}
	}
	return collectorSpec{}, false
}

func (s *Server) stats(ctx context.Context, refresh bool) map[string]any {
	envelopes := make([]Envelope, len(s.collectors))
	var wait sync.WaitGroup
	for index, spec := range s.collectors {
		wait.Add(1)
		go func() {
			defer wait.Done()
			envelopes[index] = s.runCollector(ctx, spec, refresh, false)
		}()
	}
	wait.Wait()
	sort.Slice(envelopes, func(i, j int) bool { return envelopes[i].Name < envelopes[j].Name })
	return map[string]any{"as_of": s.opts.Now().UTC(), "collectors": envelopes}
}

func (s *Server) runCollector(ctx context.Context, spec collectorSpec, refresh, full bool) Envelope {
	args := append([]string{}, spec.Args...)
	var ws *workspace
	key := spec.Name
	if spec.WorkspaceFlag != "" {
		root := s.workspaceRoot(ctx)
		ws = &root
		args = append(args, spec.WorkspaceFlag, root.Root)
		key += "\x00" + root.Root
	}
	outcome, cached := s.cache.get(key, spec.TTL, refresh, func() runOutcome {
		return s.execute(spec.Command, args, spec.Timeout)
	})
	envelope := s.envelope(spec.Name, spec.Command, outcome, cached, full)
	envelope.Workspace = ws
	return envelope
}

func (s *Server) runPRDelta(args []string) Envelope {
	command := s.prDelta
	if command == "" {
		command = "AID.pr-delta"
	}
	return s.envelope("pr_delta", command, s.execute(command, args, s.opts.PRDeltaTimeout), false, true)
}

func (s *Server) execute(command string, args []string, timeout time.Duration) runOutcome {
	path, err := s.opts.Resolve(command)
	if err != nil {
		return runOutcome{At: s.opts.Now().UTC(), Error: &CollectorError{"exec_failed", "collector executable was not found"}}
	}
	s.logf("running %s", command)
	return run(path, args, s.opts.Env, timeout, s.opts.Now)
}

func (s *Server) envelope(name, command string, outcome runOutcome, cached, full bool) Envelope {
	at := outcome.At
	envelope := Envelope{Name: name, Command: command, AsOf: &at, Cached: cached, ExitCode: outcome.ExitCode,
		Complete: outcome.Complete && outcome.Error == nil, Error: outcome.Error, Summary: outcome.Summary}
	envelope.AgeSeconds = math.Max(0, math.Round(s.opts.Now().Sub(at).Seconds()*1000)/1000)
	if len(envelope.Summary) == 0 {
		envelope.Summary = json.RawMessage("null")
	}
	if full && outcome.Result != nil {
		envelope.Result = outcome.Result
	}
	return envelope
}

// ---- argument validation ----

func parseRefresh(args map[string]json.RawMessage, allowed ...string) (bool, *argumentError) {
	if err := onlyKeys(args, allowed...); err != nil {
		return false, err
	}
	raw, ok := args["refresh"]
	if !ok {
		return false, nil
	}
	var refresh bool
	if json.Unmarshal(raw, &refresh) != nil {
		return false, &argumentError{"refresh must be a boolean"}
	}
	return refresh, nil
}

func parseCollectorArgs(args map[string]json.RawMessage) (string, bool, *argumentError) {
	refresh, err := parseRefresh(args, "name", "refresh")
	if err != nil {
		return "", false, err
	}
	var name string
	if raw, ok := args["name"]; !ok || json.Unmarshal(raw, &name) != nil || name == "" {
		return "", false, &argumentError{"name must be a collector name string"}
	}
	return name, refresh, nil
}

func prDeltaArgs(args map[string]json.RawMessage) ([]string, *argumentError) {
	if err := onlyKeys(args, "repo", "pr", "reviewer", "since_commit"); err != nil {
		return nil, err
	}
	var repo string
	if raw, ok := args["repo"]; !ok || json.Unmarshal(raw, &repo) != nil || !validRepo(repo) {
		return nil, &argumentError{"repo must be OWNER/NAME"}
	}
	var pr float64
	if raw, ok := args["pr"]; !ok || json.Unmarshal(raw, &pr) != nil || pr < 1 || pr > math.MaxInt32 || pr != math.Trunc(pr) {
		return nil, &argumentError{"pr must be a positive integer"}
	}
	argv := []string{"--json", "--repo", repo, "--pr", strconv.Itoa(int(pr))}
	for _, field := range []struct {
		key, flag string
		valid     func(string) bool
		message   string
	}{
		{"reviewer", "--reviewer", loginPattern.MatchString, "reviewer must be a GitHub login"},
		{"since_commit", "--since-commit", shaPattern.MatchString, "since_commit must be 7 to 40 hexadecimal characters"},
	} {
		raw, ok := args[field.key]
		if !ok {
			continue
		}
		var value string
		if json.Unmarshal(raw, &value) != nil || !field.valid(value) {
			return nil, &argumentError{field.message}
		}
		argv = append(argv, field.flag, value)
	}
	return argv, nil
}

func validRepo(repo string) bool {
	_, name, _ := strings.Cut(repo, "/")
	return repoPattern.MatchString(repo) && name != "." && name != ".."
}

func onlyKeys(args map[string]json.RawMessage, allowed ...string) *argumentError {
	for key := range args {
		known := false
		for _, name := range allowed {
			known = known || key == name
		}
		if !known {
			return &argumentError{"unknown argument " + strconv.Quote(truncate(key, 64))}
		}
	}
	return nil
}
