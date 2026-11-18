package installer

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"runtime"
	"strings"
)

type mcpFormat string

const (
	mcpNone         mcpFormat = ""
	mcpJSON         mcpFormat = "json"
	mcpOpenCodeJSON mcpFormat = "opencode-json"
	mcpCodexTOML    mcpFormat = "toml"

	aidMCPServerName = "aid"
)

func launcherExecutable(executableDir string) string {
	path := filepath.Join(executableDir, "AID")
	if runtime.GOOS == "windows" {
		path += ".exe"
	}
	return path
}

func desiredMCPEntryBytes(format mcpFormat, executableDir string) ([]byte, error) {
	exe := launcherExecutable(executableDir)
	switch format {
	case mcpJSON:
		return canonicalJSONBytes(map[string]any{
			"command": exe,
			"args":    []string{"--mcp"},
		})
	case mcpOpenCodeJSON:
		return canonicalJSONBytes(map[string]any{
			"type":    "local",
			"command": []string{exe, "--mcp"},
			"enabled": true,
		})
	case mcpCodexTOML:
		quotedExe, err := json.Marshal(exe)
		if err != nil {
			return nil, err
		}
		section := fmt.Sprintf("[mcp_servers.%s]\ncommand = %s\nargs = [\"--mcp\"]\n", aidMCPServerName, string(quotedExe))
		return []byte(section), nil
	default:
		return nil, fmt.Errorf("unsupported MCP config format %q", format)
	}
}

// inspectMCPEntry extracts the canonicalized bytes of an existing "aid" MCP entry
// from data, reporting found=false when the file has no "aid" server configured.
func inspectMCPEntry(p provider, data []byte) (entryCanonical []byte, found bool, err error) {
	switch p.MCPFormat {
	case mcpJSON, mcpOpenCodeJSON:
		root, err := decodeJSONObjectMap(data, p.MCPPath)
		if err != nil {
			return nil, false, err
		}
		rawBucket, hasBucket := root[p.MCPTopKey]
		if !hasBucket || bytes.Equal(bytes.TrimSpace(rawBucket), []byte("null")) {
			return nil, false, nil
		}
		bucket, err := decodeJSONObjectMap(rawBucket, fmt.Sprintf("%s in %s", p.MCPTopKey, p.MCPPath))
		if err != nil {
			return nil, false, err
		}
		rawEntry, hasEntry := bucket[aidMCPServerName]
		if !hasEntry {
			return nil, false, nil
		}
		canon, err := canonicalizeRawJSON(rawEntry)
		if err != nil {
			return nil, false, fmt.Errorf("invalid %s.%s entry in %s: %w", p.MCPTopKey, aidMCPServerName, p.MCPPath, err)
		}
		return canon, true, nil

	case mcpCodexTOML:
		start, end, present, err := codexAIDSectionRange(string(data), p.MCPPath)
		if err != nil || !present {
			return nil, false, err
		}
		section := canonicalTOMLSection(string(data)[start:end])
		return []byte(section), true, nil

	default:
		return nil, false, fmt.Errorf("unsupported MCP config format %q", p.MCPFormat)
	}
}

// upsertMCPEntry returns updated file contents with the "aid" MCP server configured,
// preserving all unrelated settings and foreign MCP servers.
func upsertMCPEntry(p provider, before []byte, existed bool, executableDir string) ([]byte, []byte, error) {
	desiredCanon, err := desiredMCPEntryBytes(p.MCPFormat, executableDir)
	if err != nil {
		return nil, nil, err
	}

	switch p.MCPFormat {
	case mcpJSON, mcpOpenCodeJSON:
		root := map[string]json.RawMessage{}
		if existed {
			root, err = decodeJSONObjectMap(before, p.MCPPath)
			if err != nil {
				return nil, nil, err
			}
		}
		bucket := map[string]json.RawMessage{}
		if rawBucket, hasBucket := root[p.MCPTopKey]; hasBucket && !bytes.Equal(bytes.TrimSpace(rawBucket), []byte("null")) {
			bucket, err = decodeJSONObjectMap(rawBucket, fmt.Sprintf("%s in %s", p.MCPTopKey, p.MCPPath))
			if err != nil {
				return nil, nil, err
			}
		}
		bucket[aidMCPServerName] = json.RawMessage(desiredCanon)
		encodedBucket, err := json.Marshal(bucket)
		if err != nil {
			return nil, nil, err
		}
		root[p.MCPTopKey] = json.RawMessage(encodedBucket)
		out, err := formatJSONDocument(root)
		if err != nil {
			return nil, nil, err
		}
		return out, desiredCanon, nil

	case mcpCodexTOML:
		text := ""
		if existed {
			text = string(before)
		}
		start, end, present, err := codexAIDSectionRange(text, p.MCPPath)
		if err != nil {
			return nil, nil, err
		}
		section := string(desiredCanon)
		var updated string
		if present {
			suffix := text[end:]
			if suffix != "" && !strings.HasPrefix(suffix, "\n") {
				section += "\n"
			}
			updated = text[:start] + section + suffix
		} else {
			updated = text
			if updated != "" && !strings.HasSuffix(updated, "\n") {
				updated += "\n"
			}
			if strings.TrimSpace(updated) != "" && !strings.HasSuffix(updated, "\n\n") {
				updated += "\n"
			}
			updated += section
		}
		return []byte(updated), []byte(canonicalTOMLSection(string(desiredCanon))), nil

	default:
		return nil, nil, fmt.Errorf("unsupported MCP config format %q", p.MCPFormat)
	}
}

// removeMCPEntry removes only the "aid" MCP server from before, preserving all
// other settings and foreign MCP servers.
func removeMCPEntry(p provider, before []byte) ([]byte, error) {
	switch p.MCPFormat {
	case mcpJSON, mcpOpenCodeJSON:
		root, err := decodeJSONObjectMap(before, p.MCPPath)
		if err != nil {
			return nil, err
		}
		rawBucket, hasBucket := root[p.MCPTopKey]
		if !hasBucket || bytes.Equal(bytes.TrimSpace(rawBucket), []byte("null")) {
			return before, nil
		}
		bucket, err := decodeJSONObjectMap(rawBucket, fmt.Sprintf("%s in %s", p.MCPTopKey, p.MCPPath))
		if err != nil {
			return nil, err
		}
		delete(bucket, aidMCPServerName)
		encodedBucket, err := json.Marshal(bucket)
		if err != nil {
			return nil, err
		}
		root[p.MCPTopKey] = json.RawMessage(encodedBucket)
		return formatJSONDocument(root)

	case mcpCodexTOML:
		text := string(before)
		start, end, present, err := codexAIDSectionRange(text, p.MCPPath)
		if err != nil || !present {
			return before, err
		}
		prefix := text[:start]
		suffix := text[end:]
		if suffix == "" && strings.HasSuffix(prefix, "\n\n") {
			prefix = strings.TrimRight(prefix, "\n") + "\n"
		} else if strings.HasSuffix(prefix, "\n\n") && strings.HasPrefix(suffix, "\n") {
			suffix = suffix[1:]
		}
		if strings.TrimSpace(prefix) == "" && strings.TrimSpace(suffix) == "" {
			return []byte(""), nil
		}
		if strings.TrimSpace(prefix) == "" {
			prefix = ""
			suffix = strings.TrimLeft(suffix, "\r\n")
		}
		return []byte(prefix + suffix), nil

	default:
		return nil, fmt.Errorf("unsupported MCP config format %q", p.MCPFormat)
	}
}

func decodeJSONObjectMap(data []byte, label string) (map[string]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var out map[string]json.RawMessage
	if err := dec.Decode(&out); err != nil || out == nil {
		return nil, fmt.Errorf("refusing to rewrite invalid JSON object in %s", label)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("refusing to rewrite trailing content in %s", label)
	}
	return out, nil
}

func canonicalizeRawJSON(raw []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return nil, err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, errors.New("unexpected trailing JSON tokens")
	}
	return canonicalJSONBytes(value)
}

func canonicalJSONBytes(value any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

func formatJSONDocument(root map[string]json.RawMessage) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(root); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// codexAIDSectionRange locates [mcp_servers.aid] and any contiguous [mcp_servers.aid.*]
// subtables in a TOML document. Scattered or duplicate [mcp_servers.aid*] tables return an error.
func codexAIDSectionRange(text, path string) (start, end int, present bool, err error) {
	inSection := false
	finishedSection := false
	offset := 0
	for _, line := range splitInclusiveLines(text) {
		header, isHeader := parseTOMLTableHeader(line)
		if isHeader {
			isAID := isAIDTOMLTable(header)
			switch {
			case isAID && finishedSection:
				return 0, 0, false, fmt.Errorf("non-contiguous [mcp_servers.%s] sections in %s; consolidate them before running AID --init", aidMCPServerName, path)
			case isAID && !inSection:
				if header != "mcp_servers."+aidMCPServerName {
					return 0, 0, false, fmt.Errorf("subtable [%s] appears before [mcp_servers.%s] in %s", header, aidMCPServerName, path)
				}
				start = offset
				inSection = true
				present = true
			case !isAID && inSection:
				end = offset
				inSection = false
				finishedSection = true
			}
		}
		offset += len(line)
	}
	if inSection {
		end = len(text)
	}
	return start, end, present, nil
}

func splitInclusiveLines(s string) []string {
	if s == "" {
		return nil
	}
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			lines = append(lines, s[start:i+1])
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}

func parseTOMLTableHeader(line string) (string, bool) {
	stripped := strings.TrimSpace(stripTOMLComment(line))
	if len(stripped) < 3 || stripped[0] != '[' || stripped[len(stripped)-1] != ']' {
		return "", false
	}
	// Array-of-tables [[...]] is also a table boundary.
	if strings.HasPrefix(stripped, "[[") && strings.HasSuffix(stripped, "]]") {
		inner := strings.TrimSpace(stripped[2 : len(stripped)-2])
		return inner, inner != ""
	}
	inner := strings.TrimSpace(stripped[1 : len(stripped)-1])
	return inner, inner != ""
}

func isAIDTOMLTable(header string) bool {
	target := "mcp_servers." + aidMCPServerName
	if header == target || strings.HasPrefix(header, target+".") {
		return true
	}
	quoted := `mcp_servers."` + aidMCPServerName + `"`
	return header == quoted || strings.HasPrefix(header, quoted+".")
}

func stripTOMLComment(line string) string {
	inDouble := false
	inSingle := false
	escaped := false
	for i := 0; i < len(line); i++ {
		c := line[i]
		if escaped {
			escaped = false
			continue
		}
		if inDouble {
			if c == '\\' {
				escaped = true
			} else if c == '"' {
				inDouble = false
			}
			continue
		}
		if inSingle {
			if c == '\'' {
				inSingle = false
			}
			continue
		}
		switch c {
		case '"':
			inDouble = true
		case '\'':
			inSingle = true
		case '#':
			return line[:i]
		}
	}
	return line
}

func canonicalTOMLSection(section string) string {
	normalized := strings.ReplaceAll(section, "\r\n", "\n")
	var kept []string
	for _, line := range strings.Split(normalized, "\n") {
		trimmed := strings.TrimSpace(stripTOMLComment(line))
		if trimmed != "" {
			kept = append(kept, trimmed)
		}
	}
	return strings.Join(kept, "\n") + "\n"
}
