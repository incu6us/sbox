package main

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestParseSessions(t *testing.T) {
	t.Parallel()

	give := "aaaaaaaaaaaaaaaa running 1700000000\n" +
		"junk\n" +
		"bbbbbbbbbbbbbbbb paused notanumber\n" +
		"cccccccccccccccc exited 1700000100\n"
	want := []session{
		{ID: "aaaaaaaaaaaaaaaa", State: "running", Started: time.Unix(1700000000, 0)},
		{ID: "cccccccccccccccc", State: "exited", Started: time.Unix(1700000100, 0)},
	}
	if got := parseSessions(give); !slices.Equal(got, want) {
		t.Errorf("parseSessions = %v, want %v", got, want)
	}
}

func TestWriteSessions(t *testing.T) {
	t.Parallel()

	now := time.Unix(1700100000, 0)
	tests := []struct {
		name string
		give []session
		want string
	}{
		{name: "none", want: "no sessions\n"},
		{
			name: "ages",
			give: []session{
				{ID: "aaaaaaaaaaaaaaaa", State: "running", Started: now.Add(-42 * time.Second)},
				{ID: "bbbbbbbbbbbbbbbb", State: "paused", Started: now.Add(-125 * time.Minute)},
				{ID: "cccccccccccccccc", State: "exited", Started: now.Add(-50 * time.Hour)},
			},
			want: "ID                STATE    AGE\n" +
				"aaaaaaaaaaaaaaaa  running  42s\n" +
				"bbbbbbbbbbbbbbbb  paused   2h5m\n" +
				"cccccccccccccccc  exited   2d2h\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			writeSessions(&buf, tt.give, now)
			if got := buf.String(); got != tt.want {
				t.Errorf("got\n%s\nwant\n%s", got, tt.want)
			}
		})
	}
}

// fakeVM stands in for sbox-run in the VM: it records each call and
// answers with canned output.
type fakeVM struct {
	calls          [][]string
	stdin          []byte
	stdout, stderr string
	answer         func(args []string) string // stdout computed from the call, instead of stdout
	err            error
}

func (f *fakeVM) run(_ context.Context, in io.Reader, out, errOut io.Writer, args ...string) error {
	f.calls = append(f.calls, args)
	if in != nil {
		var err error
		if f.stdin, err = io.ReadAll(in); err != nil {
			return err
		}
	}
	if f.answer != nil {
		_, _ = io.WriteString(out, f.answer(args))
	} else {
		_, _ = io.WriteString(out, f.stdout)
	}
	_, _ = io.WriteString(errOut, f.stderr)
	return f.err
}

func TestSessionMain(t *testing.T) {
	t.Parallel()

	const id = "0123456789abcdef"
	tests := []struct {
		name      string
		cmd       string
		args      []string
		vm        fakeVM
		wantCode  int
		wantOut   string // substring of stdout
		wantErr   string // substring of stderr
		wantCalls [][]string
	}{
		{name: "ps", cmd: "ps", vm: fakeVM{stdout: id + " running 1700000000\n"},
			wantOut: id + "  running", wantCalls: [][]string{{"ps"}}},
		{name: "ps takes no arguments", cmd: "ps", args: []string{id}, wantCode: exitUsage, wantErr: "usage"},
		{name: "stop", cmd: "stop", args: []string{id}, wantOut: id + " stopped", wantCalls: [][]string{{"stop", id}}},
		{name: "pause", cmd: "pause", args: []string{id}, wantOut: id + " paused", wantCalls: [][]string{{"pause", id}}},
		{name: "resume", cmd: "resume", args: []string{id}, wantOut: id + " resumed", wantCalls: [][]string{{"resume", id}}},
		{name: "log escapes guest output", cmd: "log", args: []string{id}, vm: fakeVM{stdout: "hi\n\x1b[2Jx\n"},
			wantOut: "hi\n\\x1b[2Jx\n", wantCalls: [][]string{{"log", id}}},
		{name: "missing id", cmd: "stop", wantCode: exitUsage, wantErr: "usage"},
		{name: "bad id never reaches the VM", cmd: "pause", args: []string{"NOT-HEX"}, wantCode: exitUsage, wantErr: "session id"},
		{name: "VM-side failure", cmd: "resume", args: []string{id},
			vm:       fakeVM{stderr: "sbox-run: session " + id + " is not paused\n", err: &exec.ExitError{}},
			wantCode: exitFail, wantErr: "is not paused", wantCalls: [][]string{{"resume", id}}},
		{name: "limactl missing", cmd: "ps", vm: fakeVM{err: errors.New(`exec: "limactl": executable file not found in $PATH`)},
			wantCode: exitFail, wantErr: "sbox: exec: \"limactl\"", wantCalls: [][]string{{"ps"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var stdout, stderr bytes.Buffer
			code := sessionMain(context.Background(), tt.cmd, tt.args, &stdout, &stderr, tt.vm.run)
			if code != tt.wantCode || !strings.Contains(stdout.String(), tt.wantOut) || !strings.Contains(stderr.String(), tt.wantErr) {
				t.Errorf("exit %d, stdout %q, stderr %q; want exit %d, stdout with %q, stderr with %q",
					code, stdout.String(), stderr.String(), tt.wantCode, tt.wantOut, tt.wantErr)
			}
			if !slices.EqualFunc(tt.vm.calls, tt.wantCalls, slices.Equal) {
				t.Errorf("sbox-run calls %q, want %q", tt.vm.calls, tt.wantCalls)
			}
		})
	}
}

// tarFiles reads a run tar into name → content.
func tarFiles(t *testing.T, data []byte) map[string]string {
	t.Helper()
	files := make(map[string]string)
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return files
		}
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		files[hdr.Name] = string(body)
	}
}

func TestStartMain(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.js"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	ready := func(args []string) string { return "READY " + args[1] + "\n" }

	t.Run("ready", func(t *testing.T) {
		t.Parallel()
		vm := fakeVM{answer: ready}
		var stdout, stderr bytes.Buffer
		code := startMain(context.Background(), []string{dir, "--", "sh", "-c", "echo hi"}, &stdout, &stderr, vm.run)
		if code != exitOK || len(vm.calls) != 1 {
			t.Fatalf("exit %d, calls %q, stderr %q; want exit 0 and one call", code, vm.calls, stderr.String())
		}
		call := vm.calls[0]
		if len(call) != 2 || call[0] != "start" || !_sessionID.MatchString(call[1]) {
			t.Fatalf("call %q, want start <session id>", call)
		}
		if got := stdout.String(); got != call[1]+"\n" {
			t.Errorf("stdout %q, want the session id", got)
		}
		files := tarFiles(t, vm.stdin)
		if files["skill/app.js"] != "hi" || !strings.Contains(files["sbox/cmd.sh"], "'sh' '-c' 'echo hi'") || len(files["sbox/nonce"]) != 32 {
			t.Errorf("run tar %v lacks the project, the command or the nonce", slices.Sorted(maps.Keys(files)))
		}
	})

	t.Run("no ready line", func(t *testing.T) {
		t.Parallel()
		vm := fakeVM{stdout: "something else\n"}
		var stdout, stderr bytes.Buffer
		code := startMain(context.Background(), []string{dir, "--", "true"}, &stdout, &stderr, vm.run)
		if code != exitFail || stdout.Len() != 0 || !strings.Contains(stderr.String(), "did not report ready") {
			t.Errorf("exit %d, stdout %q, stderr %q; want exit 1 and an explanation", code, stdout.String(), stderr.String())
		}
	})

	t.Run("not a directory", func(t *testing.T) {
		t.Parallel()
		vm := fakeVM{answer: ready}
		var stdout, stderr bytes.Buffer
		code := startMain(context.Background(), []string{filepath.Join(dir, "app.js"), "--", "true"}, &stdout, &stderr, vm.run)
		if code != exitUsage || len(vm.calls) != 0 || !strings.Contains(stderr.String(), "not a directory") {
			t.Errorf("exit %d, calls %q, stderr %q; want usage, no call, not a directory", code, vm.calls, stderr.String())
		}
	})

	for _, args := range [][]string{nil, {dir}, {dir, "true"}, {dir, "--"}} {
		vm := fakeVM{answer: ready}
		var stdout, stderr bytes.Buffer
		if code := startMain(context.Background(), args, &stdout, &stderr, vm.run); code != exitUsage || len(vm.calls) != 0 {
			t.Errorf("args %q: exit %d, calls %q; want usage and no call", args, code, vm.calls)
		}
	}
}
