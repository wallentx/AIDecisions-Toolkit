package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

type request struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables"`
}

func main() {
	input, _ := io.ReadAll(os.Stdin)
	switch os.Getenv("AID_FAKE_GH_MODE") {
	case "inspect-rest":
		writeJSON(map[string]any{
			"args": os.Args[1:], "stdin": string(input), "environment": environment(),
		})
	case "inspect-graphql":
		var body request
		if err := json.Unmarshal(input, &body); err != nil {
			os.Exit(2)
		}
		writeJSON(map[string]any{"data": map[string]any{
			"args": os.Args[1:], "query": body.Query, "variables": body.Variables, "environment": environment(),
		}})
	case "partial-exit-1":
		fmt.Print(`{"data":{"value":"kept"},"errors":[{"message":"super-secret"}]}`)
		fmt.Fprintln(os.Stderr, "gh: super-secret GraphQL error")
		os.Exit(1)
	case "data-exit-1":
		fmt.Print(`{"data":{"value":"kept"}}`)
		fmt.Fprintln(os.Stderr, "gh: super-secret command error")
		os.Exit(1)
	case "null-data-error":
		fmt.Print(`{"data":null,"errors":[{"message":"super-secret"}]}`)
	case "null":
		fmt.Print(`null`)
	case "malformed":
		fmt.Print(`{"secret":"super-secret"`)
	case "oversize":
		fmt.Print(strings.Repeat("x", 1024))
	case "raw-exact":
		fmt.Print("12345678")
	case "raw-over":
		fmt.Print("123456789")
	case "raw-ansi":
		fmt.Print("\x1b[31mred\x1b[0m\r\n")
	case "escape-flag-unsupported":
		fmt.Fprintln(os.Stderr, "unknown flag: --allow-escape-sequences; super-secret")
		os.Exit(1)
	case "stderr-oversize":
		fmt.Fprint(os.Stderr, strings.Repeat("x", 1024))
		os.Exit(1)
	case "not-found":
		fmt.Fprintln(os.Stderr, "super-secret (HTTP 404)")
		os.Exit(1)
	case "gone":
		fmt.Fprintln(os.Stderr, "super-secret (HTTP 410)")
		os.Exit(1)
	case "denied":
		fmt.Fprintln(os.Stderr, "token=super-secret (HTTP 403)")
		os.Exit(1)
	case "failed":
		fmt.Fprintln(os.Stderr, "token=super-secret")
		os.Exit(1)
	case "sleep":
		time.Sleep(10 * time.Second)
		fmt.Print(`{}`)
	default:
		fmt.Print(`{"ok":true}`)
	}
}

func environment() map[string]any {
	_, debugSet := os.LookupEnv("GH_DEBUG")
	_, genericDebugSet := os.LookupEnv("DEBUG")
	_, pagerSet := os.LookupEnv("PAGER")
	return map[string]any{
		"gh_debug_set": debugSet, "debug_set": genericDebugSet, "pager_set": pagerSet,
		"prompt": os.Getenv("GH_PROMPT_DISABLED"), "gh_pager": os.Getenv("GH_PAGER"), "no_color": os.Getenv("NO_COLOR"),
	}
}

func writeJSON(value any) {
	if err := json.NewEncoder(os.Stdout).Encode(value); err != nil {
		os.Exit(2)
	}
}
