package update

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
)

type replacementPlan struct {
	ParentPID  int        `json:"parent_pid"`
	InstallDir string     `json:"install_dir"`
	Artifacts  []artifact `json:"artifacts"`
}

// Windows keeps the running executable locked. The updater launches a private
// copy which waits for its parent to exit before performing the same transaction.
func scheduleReplacement(workspace, installDir string, artifacts []artifact, stdout io.Writer) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	helper := filepath.Join(workspace, "AID-replace.exe")
	if err := copyFile(executable, helper, 0700); err != nil {
		return fmt.Errorf("copy update helper: %w", err)
	}
	planPath := filepath.Join(workspace, "replace.json")
	plan := replacementPlan{ParentPID: os.Getpid(), InstallDir: installDir, Artifacts: artifacts}
	data, err := json.Marshal(plan)
	if err != nil {
		return err
	}
	if err := os.WriteFile(planPath, data, 0600); err != nil {
		return err
	}
	logPath := filepath.Join(workspace, "update.log")
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer log.Close()
	command := exec.Command(helper, "__replace", "--plan", planPath)
	command.Dir = workspace
	command.Stdout = log
	command.Stderr = log
	if err := startHelper(command); err != nil {
		return fmt.Errorf("start update helper: %w", err)
	}
	_ = command.Process.Release()
	fmt.Fprintf(stdout, "Build verified. Windows replacement is scheduled after AID exits.\nCheck the result in %s\n", logPath)
	return nil
}

// RunReplace is an internal launcher command used only by the Windows helper.
func RunReplace(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("AID __replace", flag.ContinueOnError)
	flags.SetOutput(stderr)
	planPath := flags.String("plan", "", "private update plan")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || *planPath == "" {
		fmt.Fprintln(stderr, "error: internal replacement requires --plan path")
		return 2
	}
	file, err := os.Open(*planPath)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	var plan replacementPlan
	err = json.NewDecoder(io.LimitReader(file, 1<<20)).Decode(&plan)
	_ = file.Close()
	if err == nil {
		err = applyPlan(plan, waitForParent, os.Rename)
	}
	if err != nil {
		fmt.Fprintln(stderr, "error: update replacement:", err)
		return 1
	}
	fmt.Fprintf(stdout, "Update installed successfully in %s. Run AID --version to verify.\n", plan.InstallDir)
	// The temporary workspace remains for diagnosis and OS/user cleanup. Windows
	// cannot unlink the helper while it is still executing.
	return 0
}

func applyPlan(plan replacementPlan, wait func(int) error, rename func(string, string) error) error {
	if plan.ParentPID <= 0 || !filepath.IsAbs(plan.InstallDir) || len(plan.Artifacts) == 0 {
		return errors.New("invalid replacement plan")
	}
	if err := wait(plan.ParentPID); err != nil {
		return err
	}
	return replaceArtifacts(plan.InstallDir, plan.Artifacts, rename)
}
