package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// _starter is what sbox init writes: Claude Code in the guest, its API
// host reachable, and every other key commented with an example.
const _starter = `# sbox.yaml: how sbox dev runs this project. Lists here are added to the
# ones in ~/.config/sbox/config.yaml; sbox config shows the result.

dev:
  command: claude              # what sbox dev runs; leave out for a shell
  # permissions: skip          # skip (the VM is the boundary) or ask
  # env: {NODE_ENV: development}
  # setup: [npm ci]            # once per home, and when this list changes
  # start: ["npm run dev &"]   # every fresh boot; & runs it in the background
  # vcpus: 2
  # memory_mib: 2048

egress:                        # the only hosts the guest can reach
  - api.anthropic.com
  # - registry.npmjs.org
  # - "*.github.com"
  # - github.com:22            # raw TCP: git over SSH

# secrets:                     # read from your Mac's environment at start
#   CLAUDE_CODE_OAUTH_TOKEN: env:CLAUDE_CODE_OAUTH_TOKEN   # claude setup-token prints one
#   GITHUB_TOKEN: env:GITHUB_SBOX_TOKEN

# ports: [3000]                # the guest's dev server on your Mac's localhost:3000
# services: [5432]             # your Mac's localhost:5432 as host.sbox:5432 in the guest

# permissions:                 # Claude Code's rules in the guest
#   deny: ["Bash(rm -rf *)"]
# mcp:
#   servers:
#     docs: {url: "https://mcp.example.com/mcp"}   # its host must be in egress
#   deny: ["mcp__docs__delete"]

# timeouts: {setup: 10m}
`

// initMain implements `sbox init [dir]`: a starter sbox.yaml, never over
// an existing one.
func initMain(args []string, stdout, stderr io.Writer) int {
	dir, ok := oneDir(args)
	if !ok {
		_, _ = fmt.Fprintln(stderr, "usage: sbox init [dir]")
		return exitUsage
	}
	file := filepath.Join(dir, "sbox.yaml")
	f, err := os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if errors.Is(err, fs.ErrExist) {
		_, _ = fmt.Fprintf(stderr, "sbox: %s exists; sbox config shows what it sets\n", file)
		return exitUsage
	}
	if err == nil {
		_, err = io.WriteString(f, _starter)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "sbox:", err)
		return exitFail
	}
	_, _ = fmt.Fprintln(stdout, "wrote", file)
	return exitOK
}

// configMain implements `sbox config [dir]`: the effective config for a
// project, with each secret's source and whether it is set on this Mac,
// never its value.
func configMain(args []string, stdout, stderr io.Writer, host devHost) int {
	flags := flag.NewFlagSet("sbox config", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&host.projectConfig, "config", "", "")
	err := flags.Parse(args)
	dir, ok := oneDir(flags.Args())
	if err != nil || !ok {
		_, _ = fmt.Fprintln(stderr, "usage: sbox config [-config <file>] [dir]")
		return exitUsage
	}
	cfg, err := host.loadConfig(dir)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "sbox:", err)
		return exitUsage
	}
	secrets := cfg.Secrets
	cfg.Secrets = nil
	var b strings.Builder
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := errors.Join(enc.Encode(cfg), enc.Close()); err != nil {
		_, _ = fmt.Fprintln(stderr, "sbox:", err)
		return exitFail
	}
	if len(secrets) > 0 {
		lookup := host.lookupEnv
		if lookup == nil {
			lookup = os.LookupEnv
		}
		b.WriteString("secrets:\n")
		for _, k := range slices.Sorted(maps.Keys(secrets)) {
			state := "set"
			if _, ok := lookup(strings.TrimPrefix(secrets[k], "env:")); !ok {
				state = "unset on this Mac"
			}
			fmt.Fprintf(&b, "  %s: %s # %s\n", k, secrets[k], state)
		}
	}
	b.WriteString("\n# what a session gets:\n")
	for _, line := range devPreflight(cfg, secrets, readMCPJSON(dir)) {
		b.WriteString("# " + line + "\n")
	}
	if note := gitignoreNote(dir, syncRules(append(gitignoreRules(dir), cfg.Dev.Ignore...))); note != "" {
		b.WriteString("# " + note + "\n")
	}
	_, _ = io.WriteString(stdout, b.String())
	return exitOK
}

// oneDir reads an optional folder argument, "." when absent.
func oneDir(args []string) (string, bool) {
	switch len(args) {
	case 0:
		return ".", true
	case 1:
		return args[0], true
	}
	return "", false
}
