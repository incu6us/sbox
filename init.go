package main

import (
	"cmp"
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
	flags.StringVar(&host.workspace, "workspace", "", "")
	err := flags.Parse(args)
	dir, ok := oneDir(flags.Args())
	if err != nil || !ok {
		_, _ = fmt.Fprintln(stderr, "usage: sbox config [-config <file>] [-workspace <name>] [dir]")
		return exitUsage
	}
	cfg, err := host.loadConfig(dir)
	var blocked *blockedError
	if err != nil && !errors.As(err, &blocked) {
		return configFailed(stderr, err)
	}
	if blocked != nil {
		cfg = blocked.cfg // and the refused requests listed below
	}
	home := cmp.Or(host.home, os.Getenv("HOME"))
	ws, err := cfg.workspaceFor(dir, host.workspace, home)
	if err != nil {
		return configFailed(stderr, err)
	}
	var b strings.Builder
	switch _, matched := cfg.workspaceMatch(dir, home); {
	case host.workspace != "":
		fmt.Fprintf(&b, "# workspace %s (-workspace)\n", ws)
	case matched != "":
		fmt.Fprintf(&b, "# workspace %s (%s contains this folder)\n", ws, matched)
	default:
		b.WriteString("# workspace default (no workspace's paths contain this folder)\n")
	}
	project := cmp.Or(host.projectConfig, filepath.Join(dir, "sbox.yaml"))
	sources := keySources(host.userConfig, project, ws, cfg.Workspaces[ws])
	secrets := cfg.Secrets
	cfg.Secrets, cfg.Workspaces = nil, nil
	var doc yaml.Node
	if err := doc.Encode(cfg); err != nil {
		_, _ = fmt.Fprintln(stderr, "sbox:", err)
		return exitFail
	}
	for i := 0; i+1 < len(doc.Content); i += 2 {
		key, val := doc.Content[i], doc.Content[i+1]
		if val.Kind != yaml.MappingNode {
			key.LineComment = sources[key.Value]
			continue
		}
		for j := 0; j+1 < len(val.Content); j += 2 {
			val.Content[j].LineComment = sources[key.Value+"."+val.Content[j].Value]
		}
	}
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := errors.Join(enc.Encode(&doc), enc.Close()); err != nil {
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
			fmt.Fprintf(&b, "  %s: %s # %s (%s)\n", k, secrets[k], state, sources["secrets."+k])
		}
	}
	b.WriteString("\n# what a session gets:\n")
	for _, line := range devPreflight(cfg, secrets, readMCPJSON(dir)) {
		b.WriteString("# " + line + "\n")
	}
	if note := gitignoreNote(dir, syncRules(append(gitignoreRules(dir), cfg.Dev.Ignore...))); note != "" {
		b.WriteString("# " + note + "\n")
	}
	if blocked != nil {
		for _, line := range blocked.lines() {
			b.WriteString("# " + line + "\n")
		}
	}
	if id, err := projectID(dir); err == nil && host.stateDir != "" {
		approved := loadApproved(host.stateDir, id)
		for _, r := range slices.Sorted(maps.Keys(approved)) {
			fmt.Fprintf(&b, "# approved for this project on %s: %s\n", approved[r], r)
		}
	}
	_, _ = io.WriteString(stdout, b.String())
	return exitOK
}

// keySources names, per key two levels deep ("egress", "timeouts.run",
// "secrets.GITHUB_TOKEN"), where it came from: "user", "workspace <ws>"
// (its grants), "project", in that order, for sbox config's comments.
func keySources(userFile, projectFile, ws string, w workspace) map[string]string {
	sources := map[string]string{}
	add := func(key, label string) { sources[key] = strings.TrimPrefix(sources[key]+", "+label, ", ") }
	read := func(label, path string) {
		data, err := os.ReadFile(path)
		if err != nil {
			return
		}
		var top map[string]any
		if yaml.Unmarshal(data, &top) != nil {
			return
		}
		for k, v := range top {
			if k == "workspaces" {
				continue
			}
			add(k, label)
			if sub, ok := v.(map[string]any); ok {
				for s := range sub {
					add(k+"."+s, label)
				}
			}
		}
	}
	read("user", userFile)
	label := "workspace " + ws
	for key, n := range map[string]int{"egress": len(w.Egress), "mounts": len(w.Mounts), "services": len(w.Services), "secrets": len(w.Secrets)} {
		if n > 0 {
			add(key, label)
		}
	}
	for name := range w.Secrets {
		add("secrets."+name, label)
	}
	read("project", projectFile)
	return sources
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
