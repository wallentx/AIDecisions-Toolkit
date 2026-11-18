package installer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"aid/internal/buildinfo"
	"aid/internal/catalog"
)

type operation struct {
	Path, Action string
	Label        string
	Content      []byte
	Remove       bool
	Before       []byte
	BeforeMode   os.FileMode
	Existed      bool
}

type plan struct {
	Operations    []operation
	Guards        []operation
	RequiresTools bool
	StatePath     string
	StateBefore   []byte
	StateAfter    []byte
}

func skillPath(p provider, tool catalog.Tool) string {
	return filepath.Join(p.Root, "aid-"+tool.ID, "SKILL.md")
}

func skillContent(tool catalog.Tool, executableDir string) []byte {
	command := toolExecutable(tool, executableDir)
	quoted := "'" + strings.ReplaceAll(command, "'", "'\"'\"'") + "'"
	content := string(tool.Skill)
	if runtime.GOOS == "windows" {
		quoted = "& '" + strings.ReplaceAll(command, "'", "''") + "'"
		content = strings.ReplaceAll(content, "```sh", "```powershell")
	}
	return []byte(strings.ReplaceAll(content, "{{AID_COMMAND}}", quoted))
}

func toolExecutable(tool catalog.Tool, executableDir string) string {
	path := filepath.Join(executableDir, tool.Command)
	if runtime.GOOS == "windows" {
		path += ".exe"
	}
	return path
}

func verifyLauncher(executableDir string) error {
	path := launcherExecutable(executableDir)
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("required launcher %s is unavailable; build the complete AID suite first: %w", path, err)
	}
	if !info.Mode().IsRegular() || (runtime.GOOS != "windows" && info.Mode().Perm()&0111 == 0) {
		return fmt.Errorf("required launcher is not an executable file: %s", path)
	}
	return nil
}

func verifyExecutables(tools []catalog.Tool, executableDir string) error {
	for _, tool := range tools {
		path := toolExecutable(tool, executableDir)
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("required tool %s is unavailable; build the complete AID suite first: %w", path, err)
		}
		if !info.Mode().IsRegular() || (runtime.GOOS != "windows" && info.Mode().Perm()&0111 == 0) {
			return fmt.Errorf("required tool is not an executable file: %s", path)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		cmd := exec.CommandContext(ctx, path, "--version", "--json")
		var output bytes.Buffer
		cmd.Stdout = &boundedOutput{Buffer: &output, Remaining: 4096}
		cmd.Stderr = io.Discard
		err = cmd.Run()
		cancel()
		if err != nil {
			return fmt.Errorf("verify %s version: %w", path, err)
		}
		if err := validateToolVersion(tool, output.Bytes()); err != nil {
			return err
		}
	}
	return nil
}

type boundedOutput struct {
	Buffer    *bytes.Buffer
	Remaining int
}

func (b *boundedOutput) Write(data []byte) (int, error) {
	if len(data) > b.Remaining {
		return 0, errors.New("tool version output exceeds 4096 bytes")
	}
	b.Remaining -= len(data)
	return b.Buffer.Write(data)
}

func validateToolVersion(tool catalog.Tool, data []byte) error {
	var info buildinfo.Info
	if err := json.Unmarshal(data, &info); err != nil {
		return fmt.Errorf("invalid %s version metadata: %w", tool.Command, err)
	}
	if info.Command != tool.Command || info.ToolVersion != tool.Version || info.ReleaseVersion != buildinfo.Version {
		return fmt.Errorf("%s has incompatible version metadata; rebuild the complete AID suite before installing skills", tool.Command)
	}
	return nil
}

var toolIDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

func validateManifest(m manifest, ps []provider, _ []catalog.Tool) error {
	allowed := map[string]provider{}
	for _, p := range ps {
		allowed[p.ID] = p
	}
	for _, r := range m.Files {
		provider, known := allowed[r.Provider]
		if !known || !toolIDPattern.MatchString(r.Tool) || r.Path != skillPath(provider, catalog.Tool{ID: r.Tool}) {
			return fmt.Errorf("installer state has an unexpected path %s; restore its original client directory before updating", r.Path)
		}
	}
	for _, r := range m.MCP {
		provider, known := allowed[r.Provider]
		if !known || provider.MCPPath == "" || r.Path != provider.MCPPath {
			return fmt.Errorf("installer state has an unexpected MCP config path %s; restore its original client directory before updating", r.Path)
		}
	}
	return nil
}

// makePlan performs every ownership and path check without creating files.
// desired includes only providers this invocation is authorized to manage.
func makePlan(ps []provider, tools []catalog.Tool, desired map[string]bool, stateFile, executableDir string) (plan, error) {
	m, original, err := readManifest(stateFile)
	if err != nil {
		return plan{}, err
	}
	if err := validateManifest(m, ps, tools); err != nil {
		return plan{}, err
	}
	owned := map[string]record{}
	for _, r := range m.Files {
		owned[r.Path] = r
	}
	ownedMCP := map[string]mcpRecord{}
	for _, r := range m.MCP {
		ownedMCP[r.Path] = r
	}

	p := plan{StatePath: stateFile, StateBefore: original}
	for _, provider := range ps {
		install, manage := desired[provider.ID]
		if !manage {
			continue
		}
		p.RequiresTools = p.RequiresTools || install
		for _, tool := range tools {
			path := skillPath(provider, tool)
			old, managed := owned[path]
			if !install && !managed {
				continue
			}
			before, mode, exists, err := readRegular(path)
			if err != nil {
				return plan{}, err
			}
			if managed && exists && digest(before) != old.SHA256 {
				return plan{}, fmt.Errorf("managed skill was edited: %s; preserve your edits before reinstalling", path)
			}
			op := operation{Path: path, Label: provider.Name + " / " + tool.ID, Before: before, BeforeMode: mode, Existed: exists}
			if !install {
				delete(owned, path)
				if exists {
					op.Remove, op.Action = true, "remove"
					p.Operations = append(p.Operations, op)
				} else {
					p.Guards = append(p.Guards, op)
				}
				continue
			}
			content := skillContent(tool, executableDir)
			if !managed && exists && !bytes.Equal(before, content) {
				return plan{}, fmt.Errorf("unmanaged skill already exists: %s; move it aside before installing AID", path)
			}
			owned[path] = record{Provider: provider.ID, Tool: tool.ID, Path: path, SHA256: digest(content)}
			if exists && bytes.Equal(before, content) {
				p.Guards = append(p.Guards, op)
				continue
			}
			op.Content, op.Action = content, "install"
			if exists {
				op.Action = "update"
			}
			p.Operations = append(p.Operations, op)
		}

		if err := planProviderMCP(provider, install, executableDir, ownedMCP, &p); err != nil {
			return plan{}, err
		}
	}

	currentTools := map[string]bool{}
	for _, tool := range tools {
		currentTools[tool.ID] = true
	}
	for path, old := range owned {
		_, manage := desired[old.Provider]
		if !manage || currentTools[old.Tool] {
			continue
		}
		// Retired catalog tools are still ours only at their exact provider
		// skill path and only while their recorded digest matches.
		before, mode, exists, err := readRegular(path)
		if err != nil {
			return plan{}, err
		}
		if exists && digest(before) != old.SHA256 {
			return plan{}, fmt.Errorf("managed skill was edited: %s; preserve your edits before removing the retired tool", path)
		}
		delete(owned, path)
		if exists {
			p.Operations = append(p.Operations, operation{Path: path, Label: old.Provider + " / " + old.Tool, Action: "remove", Remove: true, Before: before, BeforeMode: mode, Existed: true})
		} else {
			p.Guards = append(p.Guards, operation{Path: path})
		}
	}

	sort.Slice(p.Operations, func(i, j int) bool { return p.Operations[i].Path < p.Operations[j].Path })
	updated := manifest{
		Version: 1,
		Files:   make([]record, 0, len(owned)),
		MCP:     make([]mcpRecord, 0, len(ownedMCP)),
	}
	for _, r := range owned {
		updated.Files = append(updated.Files, r)
	}
	sort.Slice(updated.Files, func(i, j int) bool { return updated.Files[i].Path < updated.Files[j].Path })
	for _, r := range ownedMCP {
		updated.MCP = append(updated.MCP, r)
	}
	sort.Slice(updated.MCP, func(i, j int) bool { return updated.MCP[i].Path < updated.MCP[j].Path })

	p.StateAfter, err = json.MarshalIndent(updated, "", "  ")
	if err != nil {
		return plan{}, err
	}
	p.StateAfter = append(p.StateAfter, '\n')
	return p, nil
}

func planProviderMCP(
	provider provider,
	install bool,
	executableDir string,
	ownedMCP map[string]mcpRecord,
	p *plan,
) error {
	if provider.MCPPath == "" || provider.MCPFormat == mcpNone {
		return nil
	}
	path := provider.MCPPath
	old, managed := ownedMCP[path]
	if !install && !managed {
		return nil
	}

	before, mode, exists, err := readRegular(path)
	if err != nil {
		return err
	}
	op := operation{
		Path:       path,
		Label:      provider.Name + " / mcp (" + aidMCPServerName + ")",
		Before:     before,
		BeforeMode: mode,
		Existed:    exists,
	}

	var existingCanon []byte
	var entryFound bool
	if exists {
		existingCanon, entryFound, err = inspectMCPEntry(provider, before)
		if err != nil {
			return err
		}
	}

	if !install {
		delete(ownedMCP, path)
		if !exists || !entryFound {
			p.Guards = append(p.Guards, op)
			return nil
		}
		if digest(existingCanon) != old.SHA256 {
			return fmt.Errorf("managed MCP server %q in %s was edited; preserve your edits before removing", aidMCPServerName, path)
		}
		after, err := removeMCPEntry(provider, before)
		if err != nil {
			return err
		}
		op.Content = after
		op.Action = "remove"
		p.Operations = append(p.Operations, op)
		return nil
	}

	after, desiredCanon, err := upsertMCPEntry(provider, before, exists, executableDir)
	if err != nil {
		return err
	}
	desiredDigest := digest(desiredCanon)

	if entryFound {
		existingDigest := digest(existingCanon)
		if existingDigest == desiredDigest {
			ownedMCP[path] = mcpRecord{Provider: provider.ID, Path: path, SHA256: desiredDigest}
			p.Guards = append(p.Guards, op)
			return nil
		}
		if managed && existingDigest != old.SHA256 {
			return fmt.Errorf("managed MCP server %q in %s was edited; preserve your edits before reinstalling", aidMCPServerName, path)
		}
		if !managed {
			return fmt.Errorf("unmanaged MCP server %q already exists in %s; remove or rename it before installing AID", aidMCPServerName, path)
		}
	}

	ownedMCP[path] = mcpRecord{Provider: provider.ID, Path: path, SHA256: desiredDigest}
	op.Content = after
	op.Action = "install"
	if exists {
		op.Action = "update"
	}
	p.Operations = append(p.Operations, op)
	return nil
}

func showPlan(out io.Writer, p plan) {
	for _, op := range p.Operations {
		fmt.Fprintf(out, "%s  %s\n", op.Action, op.Path)
	}
	if len(p.Operations) == 0 {
		fmt.Fprintln(out, "Skills and MCP configuration already match the selection.")
	}
}

// ensureDirectories records only directories created by this invocation so
// rollback can remove empty directories without touching a user's directory.
func ensureDirectories(path string, created *[]string) error {
	if err := checkPath(path); err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err == nil {
		if !info.IsDir() {
			return fmt.Errorf("not a directory: %s", path)
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := ensureDirectories(filepath.Dir(path), created); err != nil {
		return err
	}
	if err := os.Mkdir(path, 0700); err != nil {
		return err
	}
	*created = append(*created, path)
	return nil
}

func atomicWrite(path string, content []byte, mode os.FileMode) error {
	if err := checkPath(path); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".aid-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name) // A successful rename has already removed the temporary name.
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(content); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func verifyUnchanged(op operation) error {
	data, _, exists, err := readRegular(op.Path)
	if err != nil {
		return err
	}
	if exists != op.Existed || !bytes.Equal(data, op.Before) {
		return fmt.Errorf("file changed since preview: %s; rerun AID --init", op.Path)
	}
	return nil
}

func (p plan) apply() error {
	var created []string
	committed := false
	defer func() {
		if !committed {
			for i := len(created) - 1; i >= 0; i-- {
				os.Remove(created[i]) // Only empty directories can be removed.
			}
		}
	}()
	if err := ensureDirectories(filepath.Dir(p.StatePath), &created); err != nil {
		return err
	}
	lockPath := p.StatePath + ".lock"
	if err := os.Mkdir(lockPath, 0700); err != nil {
		return fmt.Errorf("acquire installer lock %s (another installer may be running): %w", lockPath, err)
	}
	defer os.Remove(lockPath)
	stateBefore, stateMode, stateExists, err := readRegular(p.StatePath)
	if err != nil {
		return err
	}
	if !bytes.Equal(stateBefore, p.StateBefore) {
		return errors.New("installer state changed since preview; rerun AID --init")
	}
	ops := append([]operation(nil), p.Operations...)
	if !bytes.Equal(p.StateBefore, p.StateAfter) {
		ops = append(ops, operation{Path: p.StatePath, Content: p.StateAfter, Before: stateBefore, BeforeMode: stateMode, Existed: stateExists})
	}
	// Preflight every file before touching the first skill or MCP config.
	for _, op := range p.Guards {
		if err := verifyUnchanged(op); err != nil {
			return err
		}
	}
	for _, op := range ops {
		if err := verifyUnchanged(op); err != nil {
			return err
		}
	}
	var applied []operation
	rollback := func(cause error) error {
		for i := len(applied) - 1; i >= 0; i-- {
			op := applied[i]
			var err error
			if op.Existed {
				mode := os.FileMode(0600)
				if op.BeforeMode != 0 {
					mode = op.BeforeMode
				}
				err = atomicWrite(op.Path, op.Before, mode)
			} else {
				err = os.Remove(op.Path)
				if errors.Is(err, os.ErrNotExist) {
					err = nil
				}
			}
			if err != nil {
				cause = errors.Join(cause, fmt.Errorf("rollback %s: %w", op.Path, err))
			}
		}
		return cause
	}
	for _, op := range ops {
		if op.Path == p.StatePath {
			for _, guard := range p.Guards {
				if err := verifyUnchanged(guard); err != nil {
					return rollback(err)
				}
			}
		}
		if err := ensureDirectories(filepath.Dir(op.Path), &created); err != nil {
			return rollback(err)
		}
		if err := verifyUnchanged(op); err != nil {
			return rollback(err)
		}
		if op.Remove {
			err = os.Remove(op.Path)
		} else {
			mode := os.FileMode(0600)
			if op.Existed && op.BeforeMode != 0 {
				mode = op.BeforeMode
			}
			err = atomicWrite(op.Path, op.Content, mode)
		}
		if err != nil {
			return rollback(fmt.Errorf("apply %s: %w", op.Path, err))
		}
		applied = append(applied, op)
	}
	committed = true
	return nil
}
