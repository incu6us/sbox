package main

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

var (
	//go:embed scripts/provision.sh
	_provisionScript string
	//go:embed scripts/doctor.sh
	_doctorScript string
	//go:embed scripts/sbox-run.sh
	_runScript string
	//go:embed scripts/sbox-dns.py
	_dnsScript string
	//go:embed scripts/sbox-proxy.py
	_proxyScript string
)

const _runScriptPath = "/opt/sbox/bin/sbox-run"

// _vmTools are installed into the VM by setup, after provisioning.
var _vmTools = []struct{ path, body string }{
	{_runScriptPath, _runScript},
	{"/opt/sbox/bin/sbox-dns.py", _dnsScript},
	{"/opt/sbox/bin/sbox-proxy.py", _proxyScript},
}

// artifact is a pinned download, checked against its SHA-256 before use.
type artifact struct {
	env    string // prefix of the URL and SHA256 variables passed to provision.sh
	url    string
	sha256 string
}

// Pinned versions. Firecracker, Node and uv sums are the ones each project
// publishes. Firecracker's CI publishes none for its guest kernels: that sum
// was taken from the first download, on 2026-10-26. Claude Code's is the
// SHA-256 of the npm tarball whose sha512 integrity matches the registry's.
const (
	_firecrackerVersion = "v1.17.0"
	_nodeVersion        = "v24.21.0"
	_uvVersion          = "0.13.0"
	_claudeVersion      = "2.1.296"
)

var _artifacts = []artifact{
	{
		env:    "FIRECRACKER",
		url:    "https://github.com/firecracker-microvm/firecracker/releases/download/v1.17.0/firecracker-v1.17.0-aarch64.tgz",
		sha256: "e351ebe4f7a16b5873bbd51005d2e6767103cff4d5ebc829df2d3f95a93e2256",
	},
	{
		env:    "KERNEL",
		url:    "https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/v1.15/aarch64/vmlinux-6.1.155",
		sha256: "e3544b10603acbf3db492cb52e000d22ba202cb4b63b9add027565683e11c591",
	},
	{
		env:    "NODE",
		url:    "https://nodejs.org/dist/v24.21.0/node-v24.21.0-linux-arm64.tar.xz",
		sha256: "6ad1325edbdb5649c379b75a237147a666c95d4f9ae8d340fef2d1575d289ad2",
	},
	{
		env:    "UV",
		url:    "https://github.com/astral-sh/uv/releases/download/0.13.0/uv-aarch64-unknown-linux-gnu.tar.gz",
		sha256: "3ccfb6af6e242433eb552f7d9676abd5412c6595c497e990d9c8cb7b5bd4d2c3",
	},
	{
		// The linux-arm64 platform package: just the native claude binary, no install scripts.
		env:    "CLAUDE",
		url:    "https://registry.npmjs.org/@anthropic-ai/claude-code-linux-arm64/-/claude-code-linux-arm64-2.1.296.tgz",
		sha256: "56abbe1cfc85e5b37afeef5ae8b4856ddc32bbe443b82689a3b5f99992045cd1",
	},
}

// _limaArtifact is Lima itself, installed on the Mac by setup when no
// limactl is found. Its sum is the one in the release's SHA256SUMS.
var _limaArtifact = artifact{
	env:    "LIMA",
	url:    "https://github.com/lima-vm/lima/releases/download/v2.2.1/lima-2.2.1-Darwin-arm64.tar.gz",
	sha256: "9e9eacce88f37e185c346bad73aa6136f738d8cdf8c3bb23cd42b071824bc66e",
}

func pinnedSHA(env string) string {
	for _, a := range _artifacts {
		if a.env == env {
			return a.sha256
		}
	}
	return ""
}

// provisionEnv is provision.sh's input: the pinned artifacts and the
// packages the user's config adds to the guest image.
func provisionEnv(cfg config) map[string]string {
	env := map[string]string{"GUEST_EXTRA_PACKAGES": strings.Join(cfg.Image.Packages, ",")}
	for _, a := range _artifacts {
		env[a.env+"_URL"] = a.url
		env[a.env+"_SHA256"] = a.sha256
	}
	return env
}

// setup creates and starts the VM if needed, provisions it with the
// image packages from userConfig, and runs the doctor to prove the result
// works.
func setup(ctx context.Context, out io.Writer, userConfig string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintln(out, "==> host")
	if !preflight(out, readHostInfo(hostCommand, home)) {
		return errors.New("this Mac can't run sbox: see the checks above")
	}
	if findLimactl(home, exec.LookPath) == "" {
		_, _ = fmt.Fprintln(out, "==> install Lima into", filepath.Join("~", _limaDir))
		client := &http.Client{Timeout: 10 * time.Minute}
		if err := installLima(ctx, client, _limaArtifact, home); err != nil {
			return fmt.Errorf("install Lima: %w", err)
		}
	}

	vm, err := vmStatus(ctx)
	if err != nil {
		return err
	}
	if vm == nil {
		_, _ = fmt.Fprintln(out, "==> create VM", _vmName)
		if err := createVM(ctx, out); err != nil {
			return err
		}
	}
	if vm == nil || vm.Status != "Running" {
		_, _ = fmt.Fprintln(out, "==> start VM", _vmName)
		if err := startVM(ctx, out); err != nil {
			return err
		}
	}
	if vm != nil && !vm.Config.NestedVirtualization {
		return fmt.Errorf("VM %q exists without nested virtualization; delete it (limactl delete %s) and rerun setup", _vmName, _vmName)
	}

	cfg, err := loadConfig(userConfig, "")
	if err != nil {
		return err
	}
	if err := vmScript(ctx, out, _provisionScript, true, provisionEnv(cfg)); err != nil {
		return fmt.Errorf("provision: %w", err)
	}
	for _, tool := range _vmTools {
		install := fmt.Sprintf("cat >%[1]s.tmp && chmod 0755 %[1]s.tmp && mv %[1]s.tmp %[1]s", tool.path)
		if err := vmScriptInput(ctx, out, install, tool.body); err != nil {
			return fmt.Errorf("install %s: %w", tool.path, err)
		}
	}

	_, _ = fmt.Fprintln(out)
	ok, err := doctor(ctx, out)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("setup finished but doctor checks failed")
	}
	return nil
}
