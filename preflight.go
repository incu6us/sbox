package main

import (
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

// _minFreeDisk is what setup needs on the host: Lima's VM disk is a sparse
// file, so the real use is the Ubuntu image, the downloads and the guest
// image, a few GiB, with room to grow.
const _minFreeDisk = 10 << 30

// hostInfo is what the preflight looks at on the Mac.
type hostInfo struct {
	arch     string // GOARCH of this binary
	chip     string // sysctl -n machdep.cpu.brand_string
	macOS    string // sw_vers -productVersion
	freeDisk uint64 // bytes free in the home directory's file system
}

var _appleChip = regexp.MustCompile(`^Apple M(\d+)\b`)

// hostChecks judges the Mac before setup downloads anything. Firecracker
// needs KVM inside the Lima VM, which needs nested virtualization: Apple
// M3 or later on macOS 15 or later. The VM's own /dev/kvm check in doctor
// stays the authority; this turns a late, confusing failure into an early
// sentence.
func hostChecks(h hostInfo) []check {
	var checks []check
	if h.arch != "arm64" {
		checks = append(checks, check{name: "arch", detail: h.arch + ": sbox needs an Apple Silicon Mac and an arm64 build"})
	} else {
		c := check{name: "chip", detail: h.chip}
		if m := _appleChip.FindStringSubmatch(h.chip); m != nil {
			gen, _ := strconv.Atoi(m[1]) // \d+ always parses
			c.ok = gen >= 3
		}
		if !c.ok {
			c.detail = fmt.Sprintf("%q: nested virtualization needs an Apple M3 or later", h.chip)
		}
		checks = append(checks, c)
	}

	major, _, _ := strings.Cut(h.macOS, ".")
	v, err := strconv.Atoi(major)
	mac := check{name: "macOS", ok: err == nil && v >= 15, detail: h.macOS}
	if !mac.ok {
		mac.detail = fmt.Sprintf("%q: nested virtualization needs macOS 15 or later", h.macOS)
	}

	disk := check{name: "disk", ok: h.freeDisk >= _minFreeDisk, detail: fmt.Sprintf("%d GiB free", h.freeDisk>>30)}
	if !disk.ok {
		disk.detail = fmt.Sprintf("%d GiB free in your home folder: setup needs %d GiB", h.freeDisk>>30, _minFreeDisk>>30)
	}
	return append(checks, mac, disk)
}

// commandOutput runs a host command and returns its output. Tests pass a fake.
type commandOutput func(name string, args ...string) (string, error)

func hostCommand(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).Output()
	return string(out), err
}

// readHostInfo gathers what hostChecks judges. A command that fails leaves
// its field empty, which the checks then report.
func readHostInfo(run commandOutput, home string) hostInfo {
	h := hostInfo{arch: runtime.GOARCH}
	if out, err := run("sysctl", "-n", "machdep.cpu.brand_string"); err == nil {
		h.chip = strings.TrimSpace(out)
	}
	if out, err := run("sw_vers", "-productVersion"); err == nil {
		h.macOS = strings.TrimSpace(out)
	}
	var fs syscall.Statfs_t
	if syscall.Statfs(home, &fs) == nil {
		h.freeDisk = fs.Bavail * uint64(fs.Bsize)
	}
	return h
}

// preflight prints the host checks and reports whether they all passed.
func preflight(out io.Writer, h hostInfo) bool {
	checks := hostChecks(h)
	printChecks(out, checks)
	return !failed(checks)
}
