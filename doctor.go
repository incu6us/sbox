package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// check is one doctor result.
type check struct {
	name   string
	ok     bool
	detail string
}

// doctor checks the whole chain, host to microVM, and prints one line per
// check. It reports false when any check fails.
func doctor(ctx context.Context, out io.Writer) (bool, error) {
	var checks []check
	defer func() { printChecks(out, checks) }()

	ver, err := exec.CommandContext(ctx, "limactl", "--version").Output()
	if err != nil {
		checks = append(checks, check{name: "limactl", detail: "not installed: brew install lima"})
		return false, nil
	}
	checks = append(checks, check{name: "limactl", ok: true, detail: strings.TrimSpace(string(ver))})

	vm, err := vmStatus(ctx)
	switch {
	case err != nil:
		return false, err
	case vm == nil:
		checks = append(checks, check{name: "vm", detail: "VM " + _vmName + " not found: run sbox setup"})
		return false, nil
	case vm.Status != "Running":
		checks = append(checks, check{name: "vm", detail: vm.Status + ": limactl start " + _vmName})
		return false, nil
	}
	checks = append(checks,
		check{name: "vm", ok: true, detail: _vmName + " running"},
		check{name: "nested virtualization", ok: vm.Config.NestedVirtualization, detail: fmt.Sprint(vm.Config.NestedVirtualization)},
	)

	var buf bytes.Buffer
	if err := vmScript(ctx, &buf, _doctorScript, false, nil); err != nil {
		return false, fmt.Errorf("doctor script: %w: %s", err, strings.TrimSpace(buf.String()))
	}
	checks = append(checks, judge(parseKV(buf.String()))...)

	for _, c := range checks {
		if !c.ok {
			return false, nil
		}
	}
	return true, nil
}

// parseKV reads the "SBOX key=value" lines the VM scripts print.
func parseKV(s string) map[string]string {
	kv := make(map[string]string)
	sc := bufio.NewScanner(strings.NewReader(s))
	for sc.Scan() {
		line, ok := strings.CutPrefix(strings.TrimSpace(sc.Text()), "SBOX ")
		if !ok {
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			kv[k] = v
		}
	}
	return kv
}

// judge turns the VM-side report into checks against the pinned versions.
func judge(kv map[string]string) []check {
	if kv["provisioned"] != "yes" {
		return []check{{name: "provisioned", detail: "/opt/sbox missing: run sbox setup"}}
	}
	want := func(name, key, expected, hint string) check {
		got := kv[key]
		c := check{name: name, ok: got == expected, detail: got}
		if !c.ok {
			c.detail = fmt.Sprintf("got %q, want %q: %s", got, expected, hint)
		}
		return c
	}
	checks := []check{
		want("kvm", "kvm", "present", "the VM needs nested virtualization: recreate it with sbox setup"),
		want("firecracker", "firecracker", _firecrackerVersion, "run sbox setup"),
		want("firecracker checksum", "firecracker-sha", pinnedSHA("FIRECRACKER"), "run sbox setup"),
		want("kernel checksum", "kernel-sha", pinnedSHA("KERNEL"), "run sbox setup"),
		want("rootfs", "rootfs", "present", "run sbox setup"),
		want("runner", "runner", "present", "run sbox setup"),
	}
	if _, booted := kv["boot-exit"]; !booted {
		return checks
	}
	return append(checks,
		want("microVM boot", "boot-exit", "0", "see the Firecracker log"),
		check{name: "boot time", ok: true, detail: kv["boot-ms"] + " ms"},
		want("guest node", "node", _nodeVersion, "run sbox setup"),
		want("guest uv", "uv", _uvVersion, "run sbox setup"),
		check{name: "guest python", ok: kv["python"] != "", detail: kv["python"]},
		check{name: "guest strace", ok: kv["strace"] != "", detail: kv["strace"]},
		want("guest reaches VM side", "net-host", "ok", "tap networking is broken"),
		want("guest cut off from internet", "net-internet", "blocked", "the sandbox can reach the internet"),
	)
}

func printChecks(out io.Writer, checks []check) {
	for _, c := range checks {
		mark := "ok  "
		if !c.ok {
			mark = "FAIL"
		}
		_, _ = fmt.Fprintf(out, "%s  %-28s %s\n", mark, c.name, c.detail)
	}
}
