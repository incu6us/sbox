package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

const (
	_vmName   = "sbox"
	_vmCPUs   = "4"
	_vmMemory = "6" // GiB
	_vmDisk   = "30"
)

// _limaDir is where sbox setup installs its own Lima, under the home folder.
var _limaDir = filepath.Join(".local", "share", "sbox", "lima")

// findLimactl picks the limactl to run: the copy sbox setup installed,
// else one on PATH, else "" when there is none.
func findLimactl(home string, lookPath func(string) (string, error)) string {
	own := filepath.Join(home, _limaDir, "bin", "limactl")
	if info, err := os.Stat(own); err == nil && info.Mode().IsRegular() {
		return own
	}
	if p, err := lookPath("limactl"); err == nil {
		return p
	}
	return ""
}

// limactlBin is findLimactl for this user. It is looked up on every call,
// since setup installs Lima partway through. When there is none it
// returns "limactl", so the error names the missing program.
func limactlBin() string {
	home, _ := os.UserHomeDir() // "" only finds PATH's copy
	if p := findLimactl(home, exec.LookPath); p != "" {
		return p
	}
	return "limactl"
}

// vmInfo is the part of `limactl list --json` that sbox needs.
type vmInfo struct {
	Name   string
	Status string
	Config struct {
		NestedVirtualization bool
	}
}

// vmStatus returns the sbox VM, or nil when it doesn't exist.
func vmStatus(ctx context.Context) (*vmInfo, error) {
	out, err := exec.CommandContext(ctx, limactlBin(), "list", "--json").Output()
	if err != nil {
		return nil, fmt.Errorf("limactl list: %w", err)
	}
	return findVM(out, _vmName)
}

// findVM picks one instance from limactl's JSON-lines output.
func findVM(out []byte, name string) (*vmInfo, error) {
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		var vm vmInfo
		if err := json.Unmarshal(sc.Bytes(), &vm); err != nil {
			return nil, fmt.Errorf("limactl list: %w", err)
		}
		if vm.Name == name {
			return &vm, nil
		}
	}
	return nil, sc.Err()
}

// createVM makes the sbox VM: vz with nested virtualization for KVM, and
// no host mounts, so nothing in it can reach the user's files.
func createVM(ctx context.Context, out io.Writer) error {
	return limactl(ctx, out, "create", "--tty=false", "--name="+_vmName,
		"--vm-type=vz", "--set=.nestedVirtualization=true", "--mount-none", "--containerd=none",
		"--cpus="+_vmCPUs, "--memory="+_vmMemory, "--disk="+_vmDisk,
		"template:ubuntu-24.04")
}

func startVM(ctx context.Context, out io.Writer) error {
	return limactl(ctx, out, "start", "--tty=false", _vmName)
}

func limactl(ctx context.Context, out io.Writer, args ...string) error {
	cmd := exec.CommandContext(ctx, limactlBin(), args...)
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("limactl %s: %w", args[0], err)
	}
	return nil
}

// vmScript runs a bash script inside the VM, fed on stdin, with env set.
func vmScript(ctx context.Context, out io.Writer, script string, root bool, env map[string]string) error {
	args := []string{"shell", "--workdir", "/", _vmName}
	if root {
		args = append(args, "sudo")
	}
	args = append(args, "env")
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, k+"="+env[k])
	}
	args = append(args, "bash", "-s")

	cmd := exec.CommandContext(ctx, limactlBin(), args...)
	cmd.Stdin = strings.NewReader(script)
	cmd.Stdout, cmd.Stderr = out, out
	return cmd.Run()
}

// vmScriptInput runs a one-line root command in the VM with input on stdin.
func vmScriptInput(ctx context.Context, out io.Writer, command, input string) error {
	cmd := exec.CommandContext(ctx, limactlBin(), "shell", "--workdir", "/", _vmName, "sudo", "sh", "-c", command)
	cmd.Stdin = strings.NewReader(input)
	cmd.Stdout, cmd.Stderr = out, out
	return cmd.Run()
}

// sboxRun runs sbox-run in the VM as root, with input on stdin. It is the
// real sboxRunFunc.
func sboxRun(ctx context.Context, in io.Reader, out, errOut io.Writer, args ...string) error {
	cmd := exec.CommandContext(ctx, limactlBin(), append([]string{"shell", "--workdir", "/", _vmName, "sudo", _runScriptPath}, args...)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = in, out, errOut
	return cmd.Run()
}
