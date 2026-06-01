// Command rambl is the CLI for the PM-driven environment.
//
//	rambl            launch the PM environment in the current repo
//	rambl pm         explicit environment launch (with flags)
//	rambl monitor    read-only worker dashboard (--once for a snapshot)
//	rambl env-once   drive the PM through one brief, non-interactively (verification)
//
// Plus the hidden `__hook` subcommand, invoked by each worker's Stop hook.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"rambl/internal/environment"
	"rambl/internal/hook"
	"rambl/internal/monitor"
)

func main() {
	if len(os.Args) >= 2 && os.Args[1] == "__hook" {
		// Hidden: invoked by Claude Code's Stop hook. Forward stdin to the
		// worker socket and exit fast. Never disrupt claude on error.
		if len(os.Args) >= 3 {
			_ = hook.Client(os.Args[2], os.Stdin)
		}
		return
	}

	// Bare `rambl` launches the environment in the current directory.
	if len(os.Args) < 2 {
		check(environment.Run(environment.Options{RepoPath: "."}))
		return
	}
	switch os.Args[1] {
	case "pm":
		pmCmd(os.Args[2:])
	case "monitor":
		monitorCmd(os.Args[2:])
	case "env-once":
		envOnceCmd(os.Args[2:])
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  rambl              launch the PM environment in the current directory
  rambl pm        -repo <path> [-model <m>]
  rambl monitor   -repo <path> [--once]     (read-only dashboard)
  rambl env-once  -repo <path> -brief <text>  (non-interactive verification)`)
	os.Exit(2)
}

func pmCmd(argv []string) {
	fs := flag.NewFlagSet("pm", flag.ExitOnError)
	repo := fs.String("repo", ".", "repository this environment manages")
	db := fs.String("db", defaultDBPath(), "SQLite state path")
	base := fs.String("base", "HEAD", "base ref for worker branches")
	model := fs.String("model", "", "optional claude model override")
	_ = fs.Parse(argv)
	check(environment.Run(environment.Options{RepoPath: *repo, DBPath: *db, Base: *base, Model: *model}))
}

func monitorCmd(argv []string) {
	fs := flag.NewFlagSet("monitor", flag.ExitOnError)
	repo := fs.String("repo", ".", "repository to monitor")
	db := fs.String("db", defaultDBPath(), "SQLite state path")
	once := fs.Bool("once", false, "print a single snapshot and exit")
	_ = fs.Parse(argv)
	if *once {
		check(monitor.Once(*db, *repo))
		return
	}
	check(monitor.Run(*db, *repo))
}

// envOnceCmd drives the PM through a single brief non-interactively (verification).
func envOnceCmd(argv []string) {
	fs := flag.NewFlagSet("env-once", flag.ExitOnError)
	repo := fs.String("repo", ".", "repository this environment manages")
	brief := fs.String("brief", "", "what to tell the PM (required)")
	db := fs.String("db", defaultDBPath(), "SQLite state path")
	base := fs.String("base", "HEAD", "base ref for worker branches")
	timeout := fs.Duration("timeout", 8*time.Minute, "overall timeout")
	_ = fs.Parse(argv)
	if *brief == "" {
		fs.Usage()
		os.Exit(2)
	}
	fmt.Printf("• driving PM with brief (non-interactive)…\n")
	tasks, err := environment.RunOnce(environment.Options{RepoPath: *repo, DBPath: *db, Base: *base}, *brief, *timeout)
	fmt.Printf("• final task states:\n")
	for _, t := range tasks {
		q := ""
		if t.Question != "" {
			q = "  ? " + t.Question
		}
		fmt.Printf("    %-16s %-11s deps=%v%s\n", t.Slug, t.Status, t.Deps, q)
	}
	check(err)
}

func defaultDBPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".rambl", "state.db")
}

func check(err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "\nFAIL: %v\n", err)
		os.Exit(1)
	}
}
