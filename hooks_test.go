package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// shHooks runs hookScript's output with sh in home, a stand-in for the
// runner's home with the project in home/work, and a fake timeout that
// just runs the command, unless the test put its own in home/.fakebin.
func shHooks(t *testing.T, home, script string) (string, error) {
	t.Helper()
	bin := filepath.Join(home, ".fakebin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	fake := filepath.Join(bin, "timeout")
	if _, err := os.Stat(fake); err != nil {
		if err := os.WriteFile(fake, []byte("#!/bin/sh\nshift\nexec \"$@\"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("sh", "-c", script)
	cmd.Env = []string{"HOME=" + home, "PATH=" + bin + ":/usr/bin:/bin"}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return out.String(), err
}

func newHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	if err := os.Mkdir(filepath.Join(home, "work"), 0o755); err != nil {
		t.Fatal(err)
	}
	return home
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	data, _ := os.ReadFile(p)
	return string(data)
}

func TestHookScript(t *testing.T) {
	t.Parallel()
	setup := []string{"echo setup-1 >> ~/log", "echo setup-2 >> ~/log"}
	start := []string{"echo start >> ~/log", "pwd > ~/where"}

	t.Run("setup once per list, start every time", func(t *testing.T) {
		t.Parallel()
		home := newHome(t)
		for range 2 {
			if out, err := shHooks(t, home, hookScript(setup, start, time.Minute, false)); err != nil {
				t.Fatalf("%v: %s", err, out)
			}
		}
		if got := readFile(t, filepath.Join(home, "log")); got != "setup-1\nsetup-2\nstart\nstart\n" {
			t.Errorf("log %q, want setup once, start twice", got)
		}
		if got := strings.TrimSpace(readFile(t, filepath.Join(home, "where"))); got != filepath.Join(home, "work") && !strings.HasSuffix(got, filepath.Join(home, "work")) {
			t.Errorf("steps ran in %q, want the project folder", got)
		}

		changed := append([]string{"echo setup-0 >> ~/log"}, setup...)
		if out, err := shHooks(t, home, hookScript(changed, nil, time.Minute, false)); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
		if out, err := shHooks(t, home, hookScript(changed, nil, time.Minute, true)); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
		if got := readFile(t, filepath.Join(home, "log")); strings.Count(got, "setup-0") != 2 {
			t.Errorf("log %q, want setup rerun for a changed list and again when forced", got)
		}
	})

	t.Run("a failed step stops the list and leaves no stamp", func(t *testing.T) {
		t.Parallel()
		home := newHome(t)
		failing := []string{"echo before >> ~/log", "exit 3", "echo after >> ~/log"}
		out, err := shHooks(t, home, hookScript(failing, start, time.Minute, false))
		if err == nil || !strings.Contains(out, "setup failed (exit 3): exit 3") {
			t.Errorf("err %v, output %q; want a failure naming the step", err, out)
		}
		if got := readFile(t, filepath.Join(home, "log")); got != "before\n" {
			t.Errorf("log %q, want only the steps before the failure, and no start", got)
		}
		if out, _ := shHooks(t, home, hookScript(failing, nil, time.Minute, false)); !strings.Contains(out, "setup failed") {
			t.Errorf("second run %q: want setup tried again, since the failure left no stamp", out)
		}
	})

	t.Run("a step that times out", func(t *testing.T) {
		t.Parallel()
		home := newHome(t)
		if err := os.MkdirAll(filepath.Join(home, ".fakebin"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, ".fakebin", "timeout"), []byte("#!/bin/sh\nexit 124\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		out, err := shHooks(t, home, hookScript([]string{"npm ci"}, nil, 10*time.Minute, false))
		if err == nil || !strings.Contains(out, "setup timed out after 10m0s: npm ci") {
			t.Errorf("err %v, output %q; want the timeout named", err, out)
		}
	})

	t.Run("a start step still running after 30s gets a hint", func(t *testing.T) {
		t.Parallel()
		// The fake sleep shortens the script's 30 s wait to wait, so a
		// one-second step outlasts it and a quick one doesn't.
		hint := func(wait string, step string) (string, time.Duration) {
			home := newHome(t)
			if err := os.MkdirAll(filepath.Join(home, ".fakebin"), 0o755); err != nil {
				t.Fatal(err)
			}
			fake := "#!/bin/sh\n[ \"$1\" = 30 ] && set -- " + wait + "\nexec /bin/sleep \"$@\"\n"
			if err := os.WriteFile(filepath.Join(home, ".fakebin", "sleep"), []byte(fake), 0o755); err != nil {
				t.Fatal(err)
			}
			began := time.Now()
			out, err := shHooks(t, home, hookScript(nil, []string{step}, time.Minute, false))
			if err != nil {
				t.Fatalf("%v: %s", err, out)
			}
			return out, time.Since(began)
		}
		const want = "sbox: start: still running after 30s: /bin/sleep 1 (end a server's line with & to run it in the background)"
		if out, _ := hint("0.2", "/bin/sleep 1"); !strings.Contains(out, want) {
			t.Errorf("slow step: output %q, want %q", out, want)
		}
		if out, took := hint("10", "true"); strings.Contains(out, "still running") || took > 5*time.Second {
			t.Errorf("quick step: output %q after %v; want no hint, and nothing left holding the output", out, took)
		}
	})

	t.Run("a start step ending in & runs detached", func(t *testing.T) {
		t.Parallel()
		home := newHome(t)
		bg := []string{"sleep 1; echo served; echo done > ~/bg &"}
		began := time.Now()
		if out, err := shHooks(t, home, hookScript(nil, bg, time.Minute, false)); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
		if waited := time.Since(began); waited > 900*time.Millisecond {
			t.Errorf("the script waited %v for a background step", waited)
		}
		for deadline := time.Now().Add(5 * time.Second); readFile(t, filepath.Join(home, "bg")) == "" && time.Now().Before(deadline); {
			time.Sleep(50 * time.Millisecond)
		}
		if readFile(t, filepath.Join(home, "bg")) != "done\n" || !strings.Contains(readFile(t, filepath.Join(home, ".sbox", "start.log")), "served") {
			t.Errorf("background step: bg %q, start.log %q; want it finished with its output logged",
				readFile(t, filepath.Join(home, "bg")), readFile(t, filepath.Join(home, ".sbox", "start.log")))
		}
	})
}
