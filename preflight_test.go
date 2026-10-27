package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestHostChecks(t *testing.T) {
	t.Parallel()

	const gib = 1 << 30
	tests := []struct {
		name    string
		host    hostInfo
		wantBad string // name of the failing check, "" when all pass
		hint    string // substring of its detail
	}{
		{name: "M4 Pro, macOS 26", host: hostInfo{arch: "arm64", chip: "Apple M4 Pro", macOS: "26.6.2", freeDisk: 31 * gib}},
		{name: "M3", host: hostInfo{arch: "arm64", chip: "Apple M3", macOS: "15.0", freeDisk: 20 * gib}},
		{name: "M5 Max", host: hostInfo{arch: "arm64", chip: "Apple M5 Max", macOS: "26.7", freeDisk: 20 * gib}},
		{name: "M1 has no nested virtualization", host: hostInfo{arch: "arm64", chip: "Apple M1 Pro", macOS: "15.3", freeDisk: 20 * gib},
			wantBad: "chip", hint: "M3 or later"},
		{name: "M2", host: hostInfo{arch: "arm64", chip: "Apple M2", macOS: "15.3", freeDisk: 20 * gib},
			wantBad: "chip", hint: "M3 or later"},
		{name: "Intel", host: hostInfo{arch: "amd64", chip: "Intel(R) Core(TM) i9-9980HK CPU @ 2.40GHz", macOS: "15.3", freeDisk: 20 * gib},
			wantBad: "arch", hint: "Apple Silicon"},
		{name: "macOS 14", host: hostInfo{arch: "arm64", chip: "Apple M3", macOS: "14.7.1", freeDisk: 20 * gib},
			wantBad: "macOS", hint: "15 or later"},
		{name: "unreadable macOS version", host: hostInfo{arch: "arm64", chip: "Apple M3", macOS: "", freeDisk: 20 * gib},
			wantBad: "macOS", hint: "15 or later"},
		{name: "low disk", host: hostInfo{arch: "arm64", chip: "Apple M4", macOS: "26.0", freeDisk: 4 * gib},
			wantBad: "disk", hint: "10 GiB"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var bad []check
			for _, c := range hostChecks(tt.host) {
				if !c.ok {
					bad = append(bad, c)
				}
			}
			switch {
			case tt.wantBad == "" && len(bad) != 0:
				t.Errorf("failed checks %+v, want none", bad)
			case tt.wantBad != "" && (len(bad) != 1 || bad[0].name != tt.wantBad || !strings.Contains(bad[0].detail, tt.hint)):
				t.Errorf("failed checks %+v, want only %s with %q", bad, tt.wantBad, tt.hint)
			}
		})
	}
}

func TestReadHostInfo(t *testing.T) {
	t.Parallel()

	answers := map[string]string{
		"sysctl -n machdep.cpu.brand_string": "Apple M4 Pro\n",
		"sw_vers -productVersion":            "26.6.2\n",
	}
	run := func(name string, args ...string) (string, error) {
		return answers[strings.Join(append([]string{name}, args...), " ")], nil
	}
	h := readHostInfo(run, t.TempDir())
	if h.arch != runtime.GOARCH || h.chip != "Apple M4 Pro" || h.macOS != "26.6.2" || h.freeDisk == 0 {
		t.Errorf("readHostInfo = %+v, want this binary's arch, the trimmed answers and a non-zero free disk", h)
	}
}

func TestPreflight(t *testing.T) {
	t.Parallel()

	good := hostInfo{arch: "arm64", chip: "Apple M4 Pro", macOS: "26.6.2", freeDisk: 31 << 30}
	var out strings.Builder
	if !preflight(&out, good) || !strings.Contains(out.String(), "Apple M4 Pro") {
		t.Errorf("preflight on a good Mac: false or no report:\n%s", out.String())
	}
	bad := good
	bad.chip = "Apple M1"
	out.Reset()
	if preflight(&out, bad) || !strings.Contains(out.String(), "M3 or later") {
		t.Errorf("preflight on an M1: true or no reason:\n%s", out.String())
	}
}

func TestFindLimactl(t *testing.T) {
	t.Parallel()

	onPath := func(string) (string, error) { return "/opt/homebrew/bin/limactl", nil }
	notOnPath := func(string) (string, error) { return "", exec.ErrNotFound }

	empty := t.TempDir()
	withPinned := t.TempDir()
	pinned := filepath.Join(withPinned, ".local", "share", "sbox", "lima", "bin", "limactl")
	if err := os.MkdirAll(filepath.Dir(pinned), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pinned, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name     string
		home     string
		lookPath func(string) (string, error)
		want     string
	}{
		{name: "sbox's own copy wins", home: withPinned, lookPath: onPath, want: pinned},
		{name: "then PATH", home: empty, lookPath: onPath, want: "/opt/homebrew/bin/limactl"},
		{name: "none", home: empty, lookPath: notOnPath, want: ""},
	}
	for _, tt := range tests {
		if got := findLimactl(tt.home, tt.lookPath); got != tt.want {
			t.Errorf("%s: findLimactl = %q, want %q", tt.name, got, tt.want)
		}
	}
}
