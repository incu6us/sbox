//go:build vm

package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// lastTick reads a session's console and returns the counter's last value.
func lastTick(t *testing.T, id string) int {
	t.Helper()
	var out bytes.Buffer
	if code := sessionMain(context.Background(), "log", []string{id}, &out, io.Discard, sboxRun); code != exitOK {
		t.Fatalf("sbox log: exit %d", code)
	}
	m := _counterTick.FindAllStringSubmatch(out.String(), -1)
	if len(m) == 0 {
		t.Fatalf("no ticks in the console:\n%s", out.String())
	}
	n, _ := strconv.Atoi(m[len(m)-1][1])
	return n
}

func sessionCmd(t *testing.T, cmd string, args ...string) string {
	t.Helper()
	var out, errOut bytes.Buffer
	if code := sessionMain(context.Background(), cmd, args, &out, &errOut, sboxRun); code != exitOK {
		t.Fatalf("sbox %s %v: exit %d: %s", cmd, args, code, errOut.String())
	}
	return out.String()
}

// TestSessionLifecycleOnVM starts a session whose guest counts once a
// second, and checks that pause freezes the count, resume continues it,
// and stop leaves nothing in sbox ps.
func TestSessionLifecycleOnVM(t *testing.T) {
	ctx := context.Background()
	if ok, _ := doctor(ctx, io.Discard); !ok {
		t.Skip("the sbox VM is not ready: run sbox setup")
	}

	var out, errOut bytes.Buffer
	if code := startMain(ctx, []string{t.TempDir(), "--", "sh", "-c", _counter}, &out, &errOut, sboxRun); code != exitOK {
		t.Fatalf("sbox start: exit %d: %s", code, errOut.String())
	}
	id := strings.TrimSpace(out.String())
	t.Cleanup(func() { _ = sessionMain(ctx, "stop", []string{id}, io.Discard, io.Discard, sboxRun) })

	if ps := sessionCmd(t, "ps"); !strings.Contains(ps, id+"  running") {
		t.Fatalf("sbox ps:\n%s\nwant %s running", ps, id)
	}
	time.Sleep(3 * time.Second)

	sessionCmd(t, "pause", id)
	if ps := sessionCmd(t, "ps"); !strings.Contains(ps, id+"  paused") {
		t.Errorf("sbox ps:\n%s\nwant %s paused", ps, id)
	}
	frozen := lastTick(t, id)
	time.Sleep(3 * time.Second)
	if got := lastTick(t, id); got != frozen {
		t.Errorf("counter moved from %d to %d while paused", frozen, got)
	}

	sessionCmd(t, "resume", id)
	time.Sleep(3 * time.Second)
	if got := lastTick(t, id); got <= frozen {
		t.Errorf("counter at %d after resume, was %d when paused", got, frozen)
	}

	sessionCmd(t, "stop", id)
	if ps := sessionCmd(t, "ps"); strings.Contains(ps, id) {
		t.Errorf("sbox ps after stop:\n%s\nstill lists %s", ps, id)
	}
}

// TestAttachOnVM attaches to a session over SSH, runs a command in the
// project folder as the runner, and gets its output and exit code back.
func TestAttachOnVM(t *testing.T) {
	ctx := context.Background()
	if ok, _ := doctor(ctx, io.Discard); !ok {
		t.Skip("the sbox VM is not ready: run sbox setup")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("hello from the host\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var errOut bytes.Buffer
	id, code := startSession(ctx, dir, []string{"sleep", "infinity"}, &errOut, sboxRun)
	if code != exitOK {
		t.Fatalf("start: exit %d: %s", code, errOut.String())
	}
	t.Cleanup(func() { _ = sessionMain(ctx, "stop", []string{id}, io.Discard, io.Discard, sboxRun) })

	var out bytes.Buffer
	code = attachMain(ctx, []string{id, "--", "sh", "-c", "cat hello.txt; id -u; pwd; exit 7"}, strings.NewReader(""), &out, &errOut, sboxRun)
	if code != 7 {
		t.Errorf("attach: exit %d, want the guest command's 7; stderr: %s", code, errOut.String())
	}
	for _, want := range []string{"hello from the host", "1000", "/home/runner/work"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("attach output %q lacks %q", out.String(), want)
		}
	}
}

// TestAttachTerminalOnVM attaches from a terminal (script(1) provides one)
// and checks the guest command gets a terminal too.
func TestAttachTerminalOnVM(t *testing.T) {
	ctx := context.Background()
	if ok, _ := doctor(ctx, io.Discard); !ok {
		t.Skip("the sbox VM is not ready: run sbox setup")
	}
	var errOut bytes.Buffer
	id, code := startSession(ctx, t.TempDir(), []string{"sleep", "infinity"}, &errOut, sboxRun)
	if code != exitOK {
		t.Fatalf("start: exit %d: %s", code, errOut.String())
	}
	t.Cleanup(func() { _ = sessionMain(ctx, "stop", []string{id}, io.Discard, io.Discard, sboxRun) })

	out, err := exec.Command("script", "-q", "/dev/null", limactlBin(), "shell", "--workdir", "/", _vmName,
		"sudo", _runScriptPath, "attach", id, "--", "sh", "-c", "tty; stty size").CombinedOutput()
	if err != nil || !strings.Contains(string(out), "/dev/pts/") {
		t.Errorf("attach from a terminal: %v, output %q; want a /dev/pts terminal in the guest", err, out)
	}
}
