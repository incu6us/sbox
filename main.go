// Command sbox runs AI agent skills in a Firecracker microVM with fake
// secrets and a fake internet, and reports what they try to do.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
)

const (
	exitOK = iota
	exitFail
	exitUsage
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// hostStateDir is where sbox keeps state on the Mac (Lima, sync bases).
func hostStateDir() string {
	home, _ := os.UserHomeDir() // "" leaves a relative path, which still works
	return filepath.Join(home, ".local", "share", "sbox")
}

func run(args []string, stdout, stderr io.Writer) int {
	usage := func() int {
		_, _ = fmt.Fprint(stderr, "usage: sbox <command>\n\n"+
			"  setup    create and provision the sbox VM (Lima + Firecracker), then run doctor\n"+
			"  doctor   check the VM, KVM, pinned artifacts, a microVM boot and its network boundary\n"+
			"  run      run a command on a directory in a fresh microVM: sbox run <dir> -- <command>\n"+
			"  scan     run a skill's hooks, MCP servers and install scripts under strace: sbox scan <dir>\n"+
			"\nsessions (a microVM that keeps running):\n"+
			"  dev      a terminal in a session on a directory, stopped on exit; its home is kept: sbox dev [-fresh] <dir> [-- <command>]\n"+
			"  start    start a session on a directory and print its id: sbox start <dir> -- <command>\n"+
			"  attach   a terminal in a running session: sbox attach <id> [-- <command>]\n"+
			"  sync     show what the Mac and the guest each changed; -apply brings it across: sbox sync [-apply] <dir>\n"+
			"  ps       list sessions and their state\n"+
			"  stop     stop a session and remove everything it left: sbox stop <id>\n"+
			"  pause    freeze a session in memory: sbox pause <id>\n"+
			"  resume   continue a paused session: sbox resume <id>\n"+
			"  log      print a session's console output: sbox log <id>\n"+
			"  clean    remove every project's saved state (what sbox dev keeps)\n")
		return exitUsage
	}
	if len(args) == 0 {
		return usage()
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	switch args[0] {
	case "run":
		return runMain(ctx, args[1:], stdout, stderr)
	case "scan":
		return scanMain(ctx, args[1:], stdout, stderr)
	case "start":
		return startMain(ctx, args[1:], stdout, stderr, sboxRun)
	case "attach":
		return attachMain(ctx, args[1:], os.Stdin, stdout, stderr, sboxRun)
	case "sync":
		return syncMain(ctx, args[1:], stdout, stderr, sboxRun, hostStateDir())
	case "dev":
		home, _ := os.UserHomeDir()
		var keep func(string) bool
		if isTerminal(os.Stdin) {
			keep = askKeep(os.Stdin, stderr)
		}
		return devMain(ctx, args[1:], os.Stdin, stdout, stderr, sboxRun, hostStateDir(), filepath.Join(home, ".config", "sbox", "config.yaml"), keep)
	case "ps", "stop", "pause", "resume", "log", "clean":
		return sessionMain(ctx, args[0], args[1:], stdout, stderr, sboxRun)
	case "setup":
		if err := setup(ctx, stdout); err != nil {
			_, _ = fmt.Fprintln(stderr, "sbox:", err)
			return exitFail
		}
	case "doctor":
		ok, err := doctor(ctx, stdout)
		if err != nil {
			_, _ = fmt.Fprintln(stderr, "sbox:", err)
		}
		if err != nil || !ok {
			return exitFail
		}
	default:
		return usage()
	}
	return exitOK
}
