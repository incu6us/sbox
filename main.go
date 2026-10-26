// Command sbox runs AI agent skills in a Firecracker microVM with fake
// secrets and a fake internet, and reports what they try to do.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
)

const (
	exitOK = iota
	exitFail
	exitUsage
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	usage := func() int {
		_, _ = fmt.Fprint(stderr, "usage: sbox <command>\n\n"+
			"  setup    create and provision the sbox VM (Lima + Firecracker), then run doctor\n"+
			"  doctor   check the VM, KVM, pinned artifacts, a microVM boot and its network boundary\n"+
			"  run      run a command on a directory in a fresh microVM: sbox run <dir> -- <command>\n")
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
