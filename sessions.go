package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// sboxRunFunc runs sbox-run in the VM as root with args and input on stdin.
// Tests pass a fake.
type sboxRunFunc func(ctx context.Context, in io.Reader, out, errOut io.Writer, args ...string) error

// _sessionID matches what sbox-run accepts.
var _sessionID = regexp.MustCompile(`^[a-f0-9]{8,32}$`)

// _sessionDone is what each session action prints on success.
var _sessionDone = map[string]string{"stop": "stopped", "pause": "paused", "resume": "resumed"}

// sessionMain implements `sbox ps`, and `sbox stop|pause|resume|log <id>`.
func sessionMain(ctx context.Context, cmd string, args []string, stdout, stderr io.Writer, vm sboxRunFunc) int {
	if cmd == "ps" {
		if len(args) != 0 {
			_, _ = fmt.Fprintln(stderr, "usage: sbox ps")
			return exitUsage
		}
		var out bytes.Buffer
		if err := vm(ctx, nil, &out, stderr, "ps"); err != nil {
			return vmFailed(stderr, err)
		}
		writeSessions(stdout, parseSessions(out.String()), time.Now())
		return exitOK
	}

	if len(args) != 1 {
		_, _ = fmt.Fprintf(stderr, "usage: sbox %s <session-id>\n", cmd)
		return exitUsage
	}
	id := args[0]
	if !_sessionID.MatchString(id) {
		_, _ = fmt.Fprintf(stderr, "sbox: %q is not a session id (sbox ps lists them)\n", id)
		return exitUsage
	}
	var out bytes.Buffer
	if err := vm(ctx, nil, &out, stderr, cmd, id); err != nil {
		return vmFailed(stderr, err)
	}
	if cmd == "log" {
		var b strings.Builder
		for _, line := range strings.SplitAfter(out.String(), "\n") {
			text, nl := strings.CutSuffix(line, "\n")
			b.WriteString(printable(strings.TrimSuffix(text, "\r"))) // the serial console sends CR LF
			if nl {
				b.WriteString("\n")
			}
		}
		_, _ = io.WriteString(stdout, b.String())
		return exitOK
	}
	_, _ = fmt.Fprintln(stdout, id, _sessionDone[cmd])
	return exitOK
}

// startMain implements `sbox start <dir> -- <command...>`: the run that
// sbox run would do, as a session that keeps running after this returns.
// It prints the session's id once the guest is up.
func startMain(ctx context.Context, args []string, stdout, stderr io.Writer, vm sboxRunFunc) int {
	if len(args) < 3 || args[1] != "--" {
		_, _ = fmt.Fprintln(stderr, "usage: sbox start <dir> -- <command> [args...]")
		return exitUsage
	}
	dir := args[0]
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		_, _ = fmt.Fprintf(stderr, "sbox: %s: not a directory\n", dir)
		return exitUsage
	}
	var ids [3]string // session id, nonce, canary
	for i, n := range []int{8, 16, 6} {
		var err error
		if ids[i], err = randomHex(n); err != nil {
			_, _ = fmt.Fprintln(stderr, "sbox:", err)
			return exitFail
		}
	}
	id, nonce, canary := ids[0], ids[1], ids[2]

	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(writeRunTar(pw, dir, guestJob{command: args[2:]}, nonce, canary)) }()
	var out bytes.Buffer
	err := vm(ctx, pr, &out, stderr, "start", id)
	_ = pr.Close() // unblocks the writer if sbox-run stopped reading early
	if err != nil {
		return vmFailed(stderr, err)
	}
	if !strings.Contains(out.String(), "READY "+id+"\n") {
		_, _ = fmt.Fprintf(stderr, "sbox: session %s did not report ready\n", id)
		return exitFail
	}
	_, _ = fmt.Fprintln(stdout, id)
	return exitOK
}

// vmFailed reports a failed sbox-run call. When sbox-run itself exited
// non-zero it has already said why; anything else (no limactl, no VM) is
// printed here.
func vmFailed(stderr io.Writer, err error) int {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		_, _ = fmt.Fprintln(stderr, "sbox:", err)
	}
	return exitFail
}

// session is a microVM started with sbox start, as sbox-run ps reports it.
type session struct {
	ID      string
	State   string // running, paused or exited
	Started time.Time
}

// parseSessions reads sbox-run ps lines ("<id> <state> <unix time>").
// Malformed lines are skipped.
func parseSessions(out string) []session {
	var list []session
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) != 3 {
			continue
		}
		secs, err := strconv.ParseInt(f[2], 10, 64)
		if err != nil {
			continue
		}
		list = append(list, session{ID: f[0], State: f[1], Started: time.Unix(secs, 0)})
	}
	return list
}

// writeSessions prints the sessions as a table, with their age at now.
func writeSessions(w io.Writer, list []session, now time.Time) {
	if len(list) == 0 {
		_, _ = fmt.Fprintln(w, "no sessions")
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%-16s  %-7s  %s\n", "ID", "STATE", "AGE")
	for _, s := range list {
		fmt.Fprintf(&b, "%-16s  %-7s  %s\n", printable(s.ID), printable(s.State), age(now.Sub(s.Started)))
	}
	_, _ = io.WriteString(w, b.String())
}

// age shortens a duration to its two largest units: 42s, 2h5m, 2d2h.
func age(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd%dh", int(d.Hours())/24, int(d.Hours())%24)
}
