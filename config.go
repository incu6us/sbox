package main

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// config is what sbox's YAML files set. Two files, fixed names, no
// includes: the user's (~/.config/sbox/config.yaml) and, for sbox dev, the
// project's (sbox.yaml in its folder). Lists append; a key sbox doesn't
// know is an error. Keys arrive with the steps that need them.
type config struct {
	Ports    []int    `yaml:"ports,omitempty"`    // guest ports forwarded to the Mac's localhost
	Services []int    `yaml:"services,omitempty"` // Mac ports the guest reaches at host.sbox
	Egress   []string `yaml:"egress,omitempty"`   // hosts the guest may reach: name or *.domain, :port or :* (443 when left out)
	Image    struct {
		Packages []string `yaml:"packages,omitempty"` // Ubuntu packages added to the guest image by sbox setup
	} `yaml:"image,omitempty"`
	Dev struct {
		Env         map[string]string `yaml:"env,omitempty"`         // variables in every login: hooks, terminals, the agent
		Setup       []string          `yaml:"setup,omitempty"`       // steps run once per home, and again when the list changes
		Start       []string          `yaml:"start,omitempty"`       // steps run at every fresh boot; a trailing & runs one in the background
		Command     string            `yaml:"command,omitempty"`     // run by sbox dev when no command is given; a login shell when unset
		Permissions string            `yaml:"permissions,omitempty"` // skip (the default: the VM is the boundary) or ask
		VCPUs       int               `yaml:"vcpus,omitempty"`       // the session microVM's size; 2 and 1024 when unset
		MemoryMiB   int               `yaml:"memory_mib,omitempty"`
		Ignore      []string          `yaml:"ignore,omitempty"`      // names or root paths neither copied in nor synced, besides node_modules, .gitignore and the like; ! takes one back
		PauseAfter  time.Duration     `yaml:"pause_after,omitempty"` // pause the session after this long with no terminal or forwarded connection; off when unset
	} `yaml:"dev,omitempty"`
	Mounts  []mount           `yaml:"mounts,omitempty"`  // Mac files and folders copied into the guest at each boot
	Secrets map[string]string `yaml:"secrets,omitempty"` // guest variable → env:HOST_VARIABLE, read when a session starts
	// Claude Code's own rules in the guest, written to ~/.claude/settings.json.
	Permissions struct {
		Allow []string `yaml:"allow,omitempty"`
		Deny  []string `yaml:"deny,omitempty"`
	} `yaml:"permissions,omitempty"`
	MCP struct {
		Servers map[string]mcpServer `yaml:"servers,omitempty"` // merged into the guest's ~/.claude.json
		Allow   []string             `yaml:"allow,omitempty"`
		Deny    []string             `yaml:"deny,omitempty"`
	} `yaml:"mcp,omitempty"`
	Timeouts struct {
		Setup    time.Duration `yaml:"setup,omitempty"`    // each dev.setup and dev.start step; 10m when unset
		Run      time.Duration `yaml:"run,omitempty"`      // sbox run's command; 1m when unset
		Entry    time.Duration `yaml:"entry,omitempty"`    // each of sbox scan's entry points; 20s when unset
		Prefetch time.Duration `yaml:"prefetch,omitempty"` // sbox scan's dependency download; 3m when unset
	} `yaml:"timeouts,omitempty"`
	VM struct {
		VCPUs     int `yaml:"vcpus,omitempty"` // the microVM's size for run, scan, and dev unless dev sets its own; 2 and 1024 when unset
		MemoryMiB int `yaml:"memory_mib,omitempty"`
	} `yaml:"vm,omitempty"`
	Prefetch struct {
		Enabled    *bool    `yaml:"enabled,omitempty"`    // sbox scan downloads dependencies first; true when unset
		Registries []string `yaml:"registries,omitempty"` // hosts added to the prefetch proxy's three public registries
	} `yaml:"prefetch,omitempty"`
	// sbox scan's calibration. Each override that weakens a verdict is named
	// in the report header and in JSON under config.overrides.
	Rules struct {
		ExpectedHosts   []string          `yaml:"expected_hosts,omitempty"`   // lookups of these drop to INFO, marked expected
		Severity        map[string]string `yaml:"severity,omitempty"`         // rule → severity; not the canary rules
		CredentialPaths []string          `yaml:"credential_paths,omitempty"` // guest paths (~/ or /) added to the credential list
		BlockOn         string            `yaml:"block_on,omitempty"`         // BLOCK from this severity; CRITICAL when unset
		ReviewOn        string            `yaml:"review_on,omitempty"`        // REVIEW from this severity; MEDIUM when unset
	} `yaml:"rules,omitempty"`
	Push   []string `yaml:"push,omitempty"` // host/owner/repo sbox run -push may push the folder's branch to
	Report struct {
		Format    string `yaml:"format,omitempty"`     // text or json; -json wins
		MaxListed int    `yaml:"max_listed,omitempty"` // per category; 15 when unset
		FailOn    string `yaml:"fail_on,omitempty"`    // review (the default: non-zero at REVIEW or BLOCK) or block
	} `yaml:"report,omitempty"`
}

// mount is a Mac file or folder copied into the guest's home at each
// boot, so the Mac's version wins; nothing comes back (sbox sync is for
// the project).
type mount struct {
	Host  string `yaml:"host"`           // absolute, or ~/...
	Guest string `yaml:"guest"`          // under /home/runner, outside the project
	Mode  string `yaml:"mode,omitempty"` // ro, the only mode
}

// mountSources resolves each mount's host path against home and checks
// it exists, following a symlink at the top (a dotfile linked from a
// dotfiles repository is the usual case).
func (c config) mountSources(home string) ([]string, error) {
	var srcs []string
	for _, m := range c.Mounts {
		p := m.Host
		if rest, ok := strings.CutPrefix(p, "~/"); ok {
			p = filepath.Join(home, rest)
		}
		real, err := filepath.EvalSymlinks(p)
		if err != nil {
			return nil, fmt.Errorf("mounts: %s: no such file or folder on the Mac", m.Host)
		}
		srcs = append(srcs, real)
	}
	return srcs, nil
}

// mcpServer is an MCP server for Claude Code in the guest: a command it
// starts there, or a remote URL (whose host must be in egress).
type mcpServer struct {
	Command string            `yaml:"command,omitempty" json:"command,omitempty"`
	Args    []string          `yaml:"args,omitempty" json:"args,omitempty"`
	Env     map[string]string `yaml:"env,omitempty" json:"env,omitempty"`
	URL     string            `yaml:"url,omitempty" json:"url,omitempty"`
	Type    string            `yaml:"-" json:"type,omitempty"`
}

var (
	_registryHost = regexp.MustCompile(`^[a-z0-9-]+(\.[a-z0-9-]+)+$`)
	_pushRepo     = regexp.MustCompile(`^[a-z0-9-]+(\.[a-z0-9-]+)+/[a-z0-9_.-]+/[a-z0-9_.-]+$`)
	_envName      = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	_serverName   = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
)

// bootKeys is what a session takes from the config when it boots, one
// canonical value per key, so a running session can be compared with the
// files now. Secrets are compared by name and source, never by value.
// dev.command, timeouts and image are left out: the first applies at every
// attach, the others when they are used.
func (c config) bootKeys() map[string]string {
	keys := map[string]any{
		"egress": c.Egress, "ports": c.Ports, "services": c.Services, "secrets": c.Secrets, "mounts": c.Mounts,
		"dev.env": c.Dev.Env, "dev.setup": c.Dev.Setup, "dev.start": c.Dev.Start,
		"dev.vcpus": c.Dev.VCPUs, "dev.memory_mib": c.Dev.MemoryMiB, "dev.permissions": c.Dev.Permissions, "dev.pause_after": c.Dev.PauseAfter,
		"permissions": c.Permissions, "mcp": c.MCP,
	}
	out := make(map[string]string, len(keys))
	for k, v := range keys {
		data, _ := json.Marshal(v) // maps, slices and strings: never fails; map keys come out sorted
		out[k] = string(data)
	}
	return out
}

// changedKeys lists, sorted, the keys whose value differs between two
// bootKeys results.
func changedKeys(was, now map[string]string) []string {
	var changed []string
	for k, v := range now {
		if was[k] != v {
			changed = append(changed, k)
		}
	}
	slices.Sort(changed)
	return changed
}

// devCommand is what sbox dev runs when given no command: "" for a login
// shell.
func (c config) devCommand() string { return c.Dev.Command }

// claudeSettings is what scripts/sbox-claude.py merges into the runner's
// Claude Code files: the permission rules and mode, and the MCP servers.
func (c config) claudeSettings() []byte {
	perms := map[string]any{
		"allow":       append(slices.Clone(c.Permissions.Allow), c.MCP.Allow...),
		"deny":        append(slices.Clone(c.Permissions.Deny), c.MCP.Deny...),
		"defaultMode": "default",
	}
	settings := map[string]any{"permissions": perms}
	if c.Dev.Permissions != "ask" {
		perms["defaultMode"] = "bypassPermissions"
		settings["skipDangerousModePermissionPrompt"] = true
	}
	servers := make(map[string]mcpServer, len(c.MCP.Servers))
	for name, s := range c.MCP.Servers {
		if s.URL != "" {
			s.Type = "http"
		}
		servers[name] = s
	}
	data, _ := json.Marshal(map[string]any{"settings": settings, "mcpServers": servers}) // maps of strings: never fails
	return data
}

// secretValues reads each secret's host variable through lookup. A
// missing one is an error, as is a value that can't be one line of the
// guest's environment file.
func (c config) secretValues(lookup func(string) (string, bool)) (map[string]string, error) {
	values := make(map[string]string, len(c.Secrets))
	for _, k := range slices.Sorted(maps.Keys(c.Secrets)) {
		host := strings.TrimPrefix(c.Secrets[k], "env:")
		v, ok := lookup(host)
		switch {
		case !ok:
			return nil, fmt.Errorf("secrets: %s: host variable %s is unset", k, host)
		case strings.ContainsAny(v, "\n\r\x00"):
			return nil, fmt.Errorf("secrets: %s: host variable %s is more than one line", k, host)
		}
		values[k] = v
	}
	return values, nil
}

// toolConfig is sbox run's and sbox scan's config: the user's file, and
// the -config file when given, which must exist. A file inside the target
// is never read: it would grant itself permissions.
func toolConfig(userFile, configFile string) (config, error) {
	if configFile != "" {
		if _, err := os.Stat(configFile); err != nil {
			return config{}, fmt.Errorf("-config: %w", err)
		}
	}
	return loadConfig(userFile, configFile)
}

// runTimeout, entryTimeout and prefetchTimeout are sbox run's and sbox
// scan's limits.
func (c config) runTimeout() time.Duration { return cmp.Or(c.Timeouts.Run, time.Minute) }

func (c config) entryTimeout() time.Duration { return cmp.Or(c.Timeouts.Entry, 20*time.Second) }

func (c config) prefetchTimeout() time.Duration { return cmp.Or(c.Timeouts.Prefetch, 3*time.Minute) }

// prefetchOn reports whether sbox scan downloads dependencies first.
func (c config) prefetchOn() bool { return c.Prefetch.Enabled == nil || *c.Prefetch.Enabled }

// runJob is sbox run's job for command.
func (c config) runJob(command []string) guestJob {
	return guestJob{command: command, egress: c.Egress, vcpus: c.VM.VCPUs, memoryMiB: c.VM.MemoryMiB, registries: c.Prefetch.Registries}
}

// setupTimeout is how long each setup or start step may take.
func (c config) setupTimeout() time.Duration {
	if c.Timeouts.Setup == 0 {
		return 10 * time.Minute
	}
	return c.Timeouts.Setup
}

// _packageName is a Debian package name: nothing mmdebstrap could read as
// a second package, an option or a file.
var _packageName = regexp.MustCompile(`^[a-z0-9][a-z0-9+.-]+$`)

var _egressPattern = regexp.MustCompile(`^(\*\.)?[a-z0-9-]+(\.[a-z0-9-]+)*(:([0-9]{1,5}|\*))?$`)

// loadConfig reads the user's file, then the project's; a missing file is
// an empty one.
func loadConfig(userFile, projectFile string) (config, error) {
	var cfg config
	for i, p := range []string{userFile, projectFile} {
		var c config
		data, err := os.ReadFile(p)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			continue
		case err != nil:
			return config{}, err
		}
		dec := yaml.NewDecoder(bytes.NewReader(data))
		dec.KnownFields(true)
		if err := dec.Decode(&c); err != nil && !errors.Is(err, io.EOF) {
			return config{}, fmt.Errorf("%s: %w", p, err)
		}
		if err := c.check(); err != nil {
			return config{}, fmt.Errorf("%s: %w", p, err)
		}
		if i == 1 && len(c.Image.Packages) > 0 {
			return config{}, fmt.Errorf("%s: image: the guest image is shared by every project; set it in ~/.config/sbox/config.yaml", p)
		}
		cfg.Image.Packages = append(cfg.Image.Packages, c.Image.Packages...)
		cfg.Mounts = append(cfg.Mounts, c.Mounts...)
		cfg.Dev.Ignore = append(cfg.Dev.Ignore, c.Dev.Ignore...)
		cfg.Dev.Setup = append(cfg.Dev.Setup, c.Dev.Setup...)
		cfg.Dev.Start = append(cfg.Dev.Start, c.Dev.Start...)
		if len(c.Dev.Env) > 0 && cfg.Dev.Env == nil {
			cfg.Dev.Env = make(map[string]string)
		}
		maps.Copy(cfg.Dev.Env, c.Dev.Env)
		if len(c.Secrets) > 0 && cfg.Secrets == nil {
			cfg.Secrets = make(map[string]string)
		}
		maps.Copy(cfg.Secrets, c.Secrets)
		if len(c.MCP.Servers) > 0 && cfg.MCP.Servers == nil {
			cfg.MCP.Servers = make(map[string]mcpServer)
		}
		maps.Copy(cfg.MCP.Servers, c.MCP.Servers)
		cfg.Permissions.Allow = append(cfg.Permissions.Allow, c.Permissions.Allow...)
		cfg.Permissions.Deny = append(cfg.Permissions.Deny, c.Permissions.Deny...)
		cfg.MCP.Allow = append(cfg.MCP.Allow, c.MCP.Allow...)
		cfg.MCP.Deny = append(cfg.MCP.Deny, c.MCP.Deny...)
		if c.Dev.Command != "" {
			cfg.Dev.Command = c.Dev.Command
		}
		if c.Dev.Permissions != "" {
			cfg.Dev.Permissions = c.Dev.Permissions
		}
		if c.Dev.PauseAfter != 0 {
			cfg.Dev.PauseAfter = c.Dev.PauseAfter
		}
		cfg.VM.VCPUs = cmp.Or(c.VM.VCPUs, cfg.VM.VCPUs)
		cfg.VM.MemoryMiB = cmp.Or(c.VM.MemoryMiB, cfg.VM.MemoryMiB)
		cfg.Timeouts.Run = cmp.Or(c.Timeouts.Run, cfg.Timeouts.Run)
		cfg.Timeouts.Entry = cmp.Or(c.Timeouts.Entry, cfg.Timeouts.Entry)
		cfg.Timeouts.Prefetch = cmp.Or(c.Timeouts.Prefetch, cfg.Timeouts.Prefetch)
		if c.Prefetch.Enabled != nil {
			cfg.Prefetch.Enabled = c.Prefetch.Enabled
		}
		cfg.Prefetch.Registries = append(cfg.Prefetch.Registries, c.Prefetch.Registries...)
		cfg.Rules.ExpectedHosts = append(cfg.Rules.ExpectedHosts, c.Rules.ExpectedHosts...)
		cfg.Rules.CredentialPaths = append(cfg.Rules.CredentialPaths, c.Rules.CredentialPaths...)
		if len(c.Rules.Severity) > 0 && cfg.Rules.Severity == nil {
			cfg.Rules.Severity = map[string]string{}
		}
		maps.Copy(cfg.Rules.Severity, c.Rules.Severity)
		cfg.Rules.BlockOn = cmp.Or(c.Rules.BlockOn, cfg.Rules.BlockOn)
		cfg.Rules.ReviewOn = cmp.Or(c.Rules.ReviewOn, cfg.Rules.ReviewOn)
		cfg.Report.Format = cmp.Or(c.Report.Format, cfg.Report.Format)
		cfg.Report.MaxListed = cmp.Or(c.Report.MaxListed, cfg.Report.MaxListed)
		cfg.Report.FailOn = cmp.Or(c.Report.FailOn, cfg.Report.FailOn)
		cfg.Push = append(cfg.Push, c.Push...)
		if c.Dev.VCPUs != 0 {
			cfg.Dev.VCPUs = c.Dev.VCPUs
		}
		if c.Dev.MemoryMiB != 0 {
			cfg.Dev.MemoryMiB = c.Dev.MemoryMiB
		}
		if c.Timeouts.Setup != 0 {
			cfg.Timeouts.Setup = c.Timeouts.Setup
		}
		cfg.Ports = append(cfg.Ports, c.Ports...)
		cfg.Services = append(cfg.Services, c.Services...)
		cfg.Egress = append(cfg.Egress, c.Egress...)
	}
	slices.Sort(cfg.Ports)
	slices.Sort(cfg.Services)
	slices.Sort(cfg.Egress)
	slices.Sort(cfg.Image.Packages)
	cfg.Ports, cfg.Services, cfg.Egress = slices.Compact(cfg.Ports), slices.Compact(cfg.Services), slices.Compact(cfg.Egress)
	cfg.Image.Packages = slices.Compact(cfg.Image.Packages)
	raw := make(map[string]string) // port → host
	for _, e := range cfg.rawEgress() {
		host, port, _ := strings.Cut(e, ":")
		if other, ok := raw[port]; ok {
			return config{}, fmt.Errorf("egress: %s and %s: raw TCP (a port other than 443) takes one host per port", other+":"+port, e)
		}
		raw[port] = host
	}
	return cfg, nil
}

// rawEgress lists the egress entries that are an exact host on a port
// other than 443. They don't go through the HTTPS proxy: the guest's
// lookup of that host is answered with the VM side of its tap, where a
// relay carries the connection to that host alone.
func (c config) rawEgress() []string {
	var raw []string
	for _, e := range c.Egress {
		host, port, _ := strings.Cut(e, ":")
		if !strings.HasPrefix(host, "*.") && port != "443" && port != "*" {
			raw = append(raw, e)
		}
	}
	return raw
}

// check validates a file's values, and normalizes egress entries to
// host:port.
func (c *config) check() error {
	for i, e := range c.Egress {
		e = strings.ToLower(e)
		if !_egressPattern.MatchString(e) {
			return fmt.Errorf("egress: %q: want a host name or *.domain, with :port or :* (no scheme or path)", c.Egress[i])
		}
		if !strings.Contains(e, ":") {
			e += ":443"
		}
		if _, port, _ := strings.Cut(e, ":"); port != "*" {
			if n, _ := strconv.Atoi(port); n < 1 || n > 65535 {
				return fmt.Errorf("egress: %q: ports are 1 to 65535", c.Egress[i])
			}
		}
		c.Egress[i] = e
	}
	for _, p := range c.Image.Packages {
		if !_packageName.MatchString(p) {
			return fmt.Errorf("image.packages: %q: want an Ubuntu package name", p)
		}
	}
	for k, v := range c.Dev.Env {
		if !_envName.MatchString(k) {
			return fmt.Errorf("dev.env: %q: want a variable name (letters, digits, _)", k)
		}
		if strings.ContainsAny(v, "\n\r\x00") {
			return fmt.Errorf("dev.env: %s: values are one line", k)
		}
	}
	for k, v := range c.Secrets {
		if !_envName.MatchString(k) {
			return fmt.Errorf("secrets: %q: want a variable name (letters, digits, _)", k)
		}
		if host, ok := strings.CutPrefix(v, "env:"); !ok || !_envName.MatchString(host) {
			return fmt.Errorf("secrets: %s: want env:<HOST_VARIABLE>; values are never written in the file", k) // not v: it may be the secret
		}
	}
	for _, steps := range [][]string{c.Dev.Setup, c.Dev.Start} {
		if slices.Contains(steps, "") {
			return errors.New("dev: a setup or start step is empty")
		}
	}
	for _, m := range c.Mounts {
		if m.Mode != "" && m.Mode != "ro" {
			return fmt.Errorf("mounts: %s: mode %q: mounts are copies in; the project comes back with sbox sync", m.Host, m.Mode)
		}
		if !filepath.IsAbs(m.Host) && !strings.HasPrefix(m.Host, "~/") {
			return fmt.Errorf("mounts: host %q: want an absolute path or one starting with ~/", m.Host)
		}
		guest := path.Clean(m.Guest)
		if !strings.HasPrefix(guest, "/home/runner/") || guest != m.Guest {
			return fmt.Errorf("mounts: guest %q: want a clean path under /home/runner/ (the rest of the guest is read-only)", m.Guest)
		}
		if guest == "/home/runner/work" || strings.HasPrefix(guest, "/home/runner/work/") {
			return fmt.Errorf("mounts: guest %q: that's the project; sbox sync carries files there", m.Guest)
		}
	}
	for _, rule := range c.Dev.Ignore {
		pat := strings.TrimPrefix(strings.TrimPrefix(rule, "!"), "/")
		_, err := path.Match(pat, "")
		if pat == "" || err != nil || path.IsAbs(pat) || path.Clean(pat) != pat || strings.HasPrefix(pat, "../") || pat == ".." {
			return fmt.Errorf("dev.ignore: %q: want a name or glob (*.log), or a clean path from the project root (build/cache, /dist), with ! to take one back", rule)
		}
	}
	if p := c.Dev.Permissions; p != "" && p != "skip" && p != "ask" {
		return fmt.Errorf("dev.permissions: %q: want skip or ask", p)
	}
	for _, rules := range [][]string{c.Permissions.Allow, c.Permissions.Deny, c.MCP.Allow, c.MCP.Deny} {
		if slices.Contains(rules, "") {
			return errors.New("permissions: a rule is empty")
		}
	}
	for name, s := range c.MCP.Servers {
		if !_serverName.MatchString(name) {
			return fmt.Errorf("mcp.servers: %q: want letters, digits, - and _", name)
		}
		if (s.Command == "") == (s.URL == "") {
			return fmt.Errorf("mcp.servers.%s: want a command or a url, not both", name)
		}
		if s.URL != "" && !strings.HasPrefix(s.URL, "https://") && !strings.HasPrefix(s.URL, "http://") {
			return fmt.Errorf("mcp.servers.%s: url: want http:// or https://", name)
		}
		for k, v := range s.Env {
			if strings.HasPrefix(v, "env:") {
				return fmt.Errorf("mcp.servers.%s: env: %s: put host variables under secrets; the server inherits them", name, k)
			}
		}
	}
	if c.Dev.VCPUs < 0 || c.Dev.VCPUs > 32 {
		return fmt.Errorf("dev.vcpus: %d: want 1 to 32", c.Dev.VCPUs)
	}
	if c.VM.VCPUs < 0 || c.VM.VCPUs > 32 {
		return fmt.Errorf("vm.vcpus: %d: want 1 to 32", c.VM.VCPUs)
	}
	if c.VM.MemoryMiB != 0 && (c.VM.MemoryMiB < 256 || c.VM.MemoryMiB > 65536) {
		return fmt.Errorf("vm.memory_mib: %d: want 256 to 65536", c.VM.MemoryMiB)
	}
	for name, d := range map[string]time.Duration{"run": c.Timeouts.Run, "entry": c.Timeouts.Entry, "prefetch": c.Timeouts.Prefetch} {
		if d != 0 && d < time.Second {
			return fmt.Errorf("timeouts.%s: %v: want 1s or more", name, d)
		}
	}
	for rule, sev := range c.Rules.Severity {
		sev = strings.ToUpper(sev)
		switch {
		case !slices.Contains(_ruleNames, rule):
			return fmt.Errorf("rules.severity: %q: no such rule", rule)
		case strings.HasPrefix(rule, "canary-"):
			return fmt.Errorf("rules.severity: %s: the canary rules can't be overridden", rule)
		case !slices.Contains(_severityNames[:], sev):
			return fmt.Errorf("rules.severity: %s: %q: want INFO, LOW, MEDIUM, HIGH or CRITICAL", rule, c.Rules.Severity[rule])
		}
		c.Rules.Severity[rule] = sev
	}
	for _, s := range []*string{&c.Rules.BlockOn, &c.Rules.ReviewOn} {
		if *s = strings.ToUpper(*s); *s != "" && !slices.Contains(_severityNames[1:], *s) {
			return fmt.Errorf("rules: %q: want LOW, MEDIUM, HIGH or CRITICAL", *s)
		}
	}
	if b, r := c.Rules.BlockOn, c.Rules.ReviewOn; b != "" && r != "" && slices.Index(_severityNames[:], r) > slices.Index(_severityNames[:], b) {
		return fmt.Errorf("rules: review_on %s is above block_on %s", r, b)
	}
	for i, p := range c.Rules.CredentialPaths {
		if rest, ok := strings.CutPrefix(p, "~/"); ok {
			p = _home + rest
		}
		if !strings.HasPrefix(p, "/") {
			return fmt.Errorf("rules.credential_paths: %q: want a guest path under ~/ or /", c.Rules.CredentialPaths[i])
		}
		c.Rules.CredentialPaths[i] = p
	}
	for i, h := range c.Rules.ExpectedHosts {
		if h = strings.ToLower(h); !_registryHost.MatchString(strings.TrimPrefix(h, "*.")) {
			return fmt.Errorf("rules.expected_hosts: %q: want a host name or *.domain", c.Rules.ExpectedHosts[i])
		}
		c.Rules.ExpectedHosts[i] = h
	}
	for i, r := range c.Push {
		if r = strings.ToLower(r); !_pushRepo.MatchString(r) {
			return fmt.Errorf("push: %q: want host/owner/repo, like github.com/org/repo", c.Push[i])
		}
		c.Push[i] = r
	}
	if f := c.Report.Format; f != "" && f != "text" && f != "json" {
		return fmt.Errorf("report.format: %q: want text or json", f)
	}
	if f := c.Report.FailOn; f != "" && f != "review" && f != "block" {
		return fmt.Errorf("report.fail_on: %q: want review or block", f)
	}
	if c.Report.MaxListed < 0 {
		return fmt.Errorf("report.max_listed: %d: want 1 or more", c.Report.MaxListed)
	}
	for i, r := range c.Prefetch.Registries {
		r = strings.ToLower(r)
		if !_registryHost.MatchString(r) {
			return fmt.Errorf("prefetch.registries: %q: want a host name (no scheme, port or path)", c.Prefetch.Registries[i])
		}
		c.Prefetch.Registries[i] = r
	}
	if c.Dev.MemoryMiB != 0 && (c.Dev.MemoryMiB < 256 || c.Dev.MemoryMiB > 65536) {
		return fmt.Errorf("dev.memory_mib: %d: want 256 to 65536", c.Dev.MemoryMiB)
	}
	if p := c.Dev.PauseAfter; p != 0 && p < time.Minute {
		return fmt.Errorf("dev.pause_after: %v: want a minute or more", p)
	}
	if c.Timeouts.Setup < 0 {
		return fmt.Errorf("timeouts.setup: %v: want a positive duration", c.Timeouts.Setup)
	}
	for _, p := range c.Ports {
		if p < 1024 || p > 65535 {
			return fmt.Errorf("ports: %d: forwarded ports are 1024 to 65535", p)
		}
	}
	for _, p := range c.Services {
		if p < 1 || p > 65535 {
			return fmt.Errorf("services: %d: ports are 1 to 65535", p)
		}
	}
	return nil
}
