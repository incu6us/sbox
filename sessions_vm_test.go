//go:build vm

package main

import (
	"bytes"
	"context"
	"io"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

var _tick = regexp.MustCompile(`(?m)^tick (\d+)$`)

// lastTick reads a session's console and returns the counter's last value.
func lastTick(t *testing.T, id string) int {
	t.Helper()
	var out bytes.Buffer
	if code := sessionMain(context.Background(), "log", []string{id}, &out, io.Discard, sboxRun); code != exitOK {
		t.Fatalf("sbox log: exit %d", code)
	}
	m := _tick.FindAllStringSubmatch(out.String(), -1)
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
	counter := "i=0; while true; do i=$((i+1)); echo tick $i; sleep 1; done"
	if code := startMain(ctx, []string{t.TempDir(), "--", "sh", "-c", counter}, &out, &errOut, sboxRun); code != exitOK {
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
