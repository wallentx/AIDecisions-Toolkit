package installer

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"aid/internal/catalog"
	"golang.org/x/term"
)

type providerFlags []string

func (p *providerFlags) String() string { return strings.Join(*p, ",") }
func (p *providerFlags) Set(value string) error {
	for _, id := range strings.Split(value, ",") {
		id = strings.TrimSpace(id)
		if id == "" {
			return errors.New("provider name cannot be empty")
		}
		*p = append(*p, id)
	}
	return nil
}

// Run starts the installer or performs an explicitly requested CLI operation.
func Run(args []string, stdin *os.File, stdout, stderr io.Writer) int {
	code, _ := RunWithResult(args, stdin, stdout, stderr)
	return code
}

// RunWithResult also reports whether the user applied a real installation
// selection. Cancellation, --list, --dry-run, and errors always return false.
func RunWithResult(args []string, stdin *os.File, stdout, stderr io.Writer) (int, bool) {
	flags := flag.NewFlagSet("AID --init", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var selected providerFlags
	var dryRun, list, remove, yes bool
	flags.Var(&selected, "provider", "client to manage (repeatable or comma-separated)")
	flags.BoolVar(&dryRun, "dry-run", false, "preview the selected changes without writing files")
	flags.BoolVar(&list, "list", false, "show agent clients, detection, and install state")
	flags.BoolVar(&remove, "remove", false, "remove managed skills from explicitly named providers")
	flags.BoolVar(&yes, "yes", false, "apply explicit --provider selections (already noninteractive)")
	flags.Usage = func() {
		fmt.Fprintln(stderr, "Usage: AID --init [--provider codex] [--dry-run|--remove]\n\nWithout flags, open the agent-client installer TUI.\nExplicit providers apply only to the named clients; other clients are unchanged.")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0, false
		}
		return 2, false
	}
	fail := func(err error) (int, bool) {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1, false
	}
	if flags.NArg() != 0 {
		return fail(fmt.Errorf("unexpected argument: %s", flags.Arg(0)))
	}
	if (yes || remove) && len(selected) == 0 {
		return fail(errors.New("--yes and --remove require at least one --provider"))
	}
	if list && (len(selected) != 0 || remove || yes || dryRun) {
		return fail(errors.New("--list cannot be combined with installation options"))
	}
	ps, err := providers()
	if err != nil {
		return fail(err)
	}
	tools, err := catalog.All()
	if err != nil {
		return fail(err)
	}
	stateFile, err := statePath()
	if err != nil {
		return fail(err)
	}
	m, _, err := readManifest(stateFile)
	if err != nil {
		return fail(err)
	}
	if err := validateManifest(m, ps, tools); err != nil {
		return fail(err)
	}
	for i := range ps {
		for _, r := range m.Files {
			if r.Provider == ps[i].ID {
				ps[i].Installed = true
			}
		}
	}
	if list {
		showProviders(stdout, ps)
		return 0, false
	}
	executable, err := os.Executable()
	if err != nil {
		return fail(fmt.Errorf("locate AID executable: %w", err))
	}
	// Resolve launcher symlinks so skills invoke sibling tools in its real
	// installation directory, regardless of the invoking agent's cwd.
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return fail(err)
	}
	executableDir := filepath.Dir(executable)
	desired := map[string]bool{}
	for _, id := range selected {
		found := false
		for _, p := range ps {
			if p.ID == id {
				found = true
			}
		}
		if !found {
			return fail(fmt.Errorf("unknown provider %q; use AID --init --list", id))
		}
		desired[id] = !remove
	}
	if len(selected) == 0 {
		for _, p := range ps {
			desired[p.ID] = p.Installed
		}
	}
	if len(selected) == 0 && !dryRun {
		outputFile, ok := stderr.(*os.File)
		if !ok || stdin == nil || os.Getenv("TERM") == "dumb" || !term.IsTerminal(int(stdin.Fd())) || !term.IsTerminal(int(outputFile.Fd())) {
			return fail(errors.New("interactive installer requires a terminal; use AID --init --provider <client> --dry-run to preview"))
		}
		p, applied, err := runTUI(stdin, outputFile, ps, tools, stateFile, executableDir)
		if err != nil {
			return fail(err)
		}
		if !applied {
			fmt.Fprintln(stdout, "Installation cancelled. No files changed.")
			return 0, false
		}
		if p.RequiresTools {
			if err := verifyExecutables(tools, executableDir); err != nil {
				return fail(err)
			}
		}
		if err := p.apply(); err != nil {
			return fail(err)
		}
		showPlan(stdout, p)
		fmt.Fprintln(stdout, "Agent skills are ready. Restart an existing agent session to load them.")
		return 0, true
	}
	p, err := makePlan(ps, tools, desired, stateFile, executableDir)
	if err != nil {
		return fail(err)
	}
	showPlan(stdout, p)
	if dryRun {
		fmt.Fprintln(stdout, "Dry run: no files changed.")
		return 0, false
	}
	if !remove {
		if err := verifyExecutables(tools, executableDir); err != nil {
			return fail(err)
		}
	}
	if err := p.apply(); err != nil {
		return fail(err)
	}
	fmt.Fprintln(stdout, "Agent skills updated.")
	return 0, true
}

func showProviders(out io.Writer, ps []provider) {
	for _, p := range ps {
		state := "not detected"
		if p.Detected {
			state = "detected"
		}
		if p.Installed {
			state = "installed"
		}
		fmt.Fprintf(out, "%-12s %-12s %s\n", p.ID, state, p.Root)
	}
}
