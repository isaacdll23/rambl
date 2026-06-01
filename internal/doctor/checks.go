package doctor

import (
	"os"
	"os/exec"
	"path/filepath"
)

// Checks returns the built-in preflight checks rambl runs to verify the host
// environment is ready to drive workers.
func Checks() []Check {
	return []Check{
		{Name: "claude CLI", Run: checkClaudeCLI},
		{Name: "git", Run: checkGit},
		{Name: "~/.rambl writable", Run: checkRamblWritable},
	}
}

// checkClaudeCLI verifies the `claude` CLI is resolvable on PATH. It's the
// engine rambl shells out to for every worker, so a missing binary is fatal.
func checkClaudeCLI() Result {
	path, err := exec.LookPath("claude")
	if err != nil {
		return Result{
			Name:   "claude CLI",
			Status: Fail,
			Detail: "not found on PATH (install the claude CLI and log in)",
		}
	}
	return Result{Name: "claude CLI", Status: OK, Detail: path}
}

// checkGit verifies `git` is resolvable on PATH. rambl manages worktrees and
// branches per task, so git is required.
func checkGit() Result {
	path, err := exec.LookPath("git")
	if err != nil {
		return Result{Name: "git", Status: Fail, Detail: "not found on PATH"}
	}
	return Result{Name: "git", Status: OK, Detail: path}
}

// checkRamblWritable ensures the ~/.rambl state directory exists and is
// writable. A missing or unwritable state dir is recoverable (it can be
// recreated, or permissions fixed), so failures are Warn rather than Fail.
func checkRamblWritable() Result {
	const name = "~/.rambl writable"

	home, err := os.UserHomeDir()
	if err != nil {
		return Result{Name: name, Status: Warn, Detail: "cannot resolve home dir: " + err.Error()}
	}

	dir := filepath.Join(home, ".rambl")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Result{Name: name, Status: Warn, Detail: "cannot create dir: " + err.Error()}
	}

	// Probe writability by creating and removing a temp file inside the dir.
	f, err := os.CreateTemp(dir, ".doctor-*")
	if err != nil {
		return Result{Name: name, Status: Warn, Detail: "not writable: " + err.Error()}
	}
	f.Close()
	os.Remove(f.Name())

	return Result{Name: name, Status: OK, Detail: dir}
}
