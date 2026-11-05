package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// workspace is a named set of permissions in the user's own file: what
// every project in it gets (the grants), and what one may ask for in its
// own sbox.yaml (allow). Projects belong by folder (paths), never by
// their own say. See docs/workspaces.md.
type workspace struct {
	Paths    []string          `yaml:"paths,omitempty"`    // folders whose projects are in this workspace
	Egress   []string          `yaml:"egress,omitempty"`   // granted to every project here
	Mounts   []mount           `yaml:"mounts,omitempty"`   // granted
	Secrets  map[string]string `yaml:"secrets,omitempty"`  // granted
	Services []int             `yaml:"services,omitempty"` // granted
	Mount    string            `yaml:"mount,omitempty"`    // live or live-ro: every project here is mounted so (docs/live-mount.md)
	Allow    struct {
		Live     bool     `yaml:"live,omitempty"`     // a project may ask for dev.mount: live or live-ro
		Egress   []string `yaml:"egress,omitempty"`   // patterns a project may ask for, or *
		Services []string `yaml:"services,omitempty"` // ports, or *
		Mounts   []string `yaml:"mounts,omitempty"`   // Mac folders or files a project may mount read-only
		RW       []string `yaml:"rw,omitempty"`       // ... or read-write
		Secrets  []string `yaml:"secrets,omitempty"`  // Mac variables a project may read
	} `yaml:"allow,omitempty"`
}

var _workspaceName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// check validates a workspace and normalizes its egress entries, as
// config.check does for the top-level keys.
func (w *workspace) check(name string) error {
	if !_workspaceName.MatchString(name) {
		return fmt.Errorf("workspaces: %q: want lowercase letters, digits and dashes", name)
	}
	where := "workspaces." + name
	macPath := func(key, p string) error {
		if !strings.HasPrefix(p, "/") && !strings.HasPrefix(p, "~/") {
			return fmt.Errorf("%s.%s: %q: want an absolute path or one starting with ~/", where, key, p)
		}
		return nil
	}
	for _, p := range w.Paths {
		if err := macPath("paths", p); err != nil {
			return err
		}
	}
	grants := config{Egress: w.Egress, Mounts: w.Mounts, Secrets: w.Secrets, Services: w.Services}
	if err := grants.check(); err != nil {
		return fmt.Errorf("%s: %w", where, err)
	}
	if w.Mount != "" && w.Mount != "live" && w.Mount != "live-ro" {
		return fmt.Errorf("%s.mount: %q: want live or live-ro (copy is the default)", where, w.Mount)
	}
	for i, e := range w.Allow.Egress {
		if e == "*" {
			continue
		}
		e = strings.ToLower(e)
		if !_egressPattern.MatchString(e) {
			return fmt.Errorf("%s.allow.egress: %q: want a host name or *.domain, with :port or :*, or *", where, w.Allow.Egress[i])
		}
		if !strings.Contains(e, ":") {
			e += ":443"
		}
		w.Allow.Egress[i] = e
	}
	for _, s := range w.Allow.Services {
		if n, err := strconv.Atoi(s); s != "*" && (err != nil || n < 1 || n > 65535) {
			return fmt.Errorf("%s.allow.services: %q: want a port or *", where, s)
		}
	}
	for _, list := range []struct {
		key   string
		paths []string
	}{{"allow.mounts", w.Allow.Mounts}, {"allow.rw", w.Allow.RW}} {
		for _, p := range list.paths {
			if err := macPath(list.key, p); err != nil {
				return err
			}
		}
	}
	for _, v := range w.Allow.Secrets {
		if !_envName.MatchString(v) {
			return fmt.Errorf("%s.allow.secrets: %q: want a variable name", where, v)
		}
	}
	return nil
}

// macPath expands ~/ against home and resolves symlinks, so two spellings
// of a folder compare equal; a path that doesn't exist is only cleaned.
func macPath(p, home string) string {
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		p = filepath.Join(home, rest)
	}
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	if real, err := filepath.EvalSymlinks(p); err == nil {
		return real
	}
	return filepath.Clean(p)
}

// under reports whether p is dir or inside it.
func under(p, dir string) bool {
	return p == dir || strings.HasPrefix(p, strings.TrimSuffix(dir, "/")+"/")
}

// workspaceFor is the workspace a project folder belongs to: the one whose
// paths include the longest folder containing it, or default. override,
// from -workspace, is the user's choice for one run; it must name a
// workspace (or default).
func (c config) workspaceFor(dir, override, home string) (string, error) {
	if override != "" {
		if _, ok := c.Workspaces[override]; !ok && override != "default" {
			return "", fmt.Errorf("-workspace %s: no such workspace in your config", override)
		}
		return override, nil
	}
	name, _ := c.workspaceMatch(dir, home)
	return name, nil
}

// workspaceMatch is the workspace whose paths hold the longest folder
// containing dir, with that folder; default and "" when none does.
func (c config) workspaceMatch(dir, home string) (name, path string) {
	if dir == "" {
		return "default", ""
	}
	dir = macPath(dir, home)
	name = "default"
	for _, n := range slices.Sorted(maps.Keys(c.Workspaces)) { // a tie goes to the first name
		for _, p := range c.Workspaces[n].Paths {
			if p = macPath(p, home); under(dir, p) && len(p) > len(path) {
				name, path = n, p
			}
		}
	}
	return name, path
}

// grants is a workspace's grants as a config layer.
func (w workspace) grants() config {
	c := config{Egress: w.Egress, Mounts: w.Mounts, Secrets: w.Secrets, Services: w.Services}
	c.Dev.Mount = w.Mount
	return c
}

// runConfig is sbox run's config: the user's file and -config, as
// toolConfig reads them, with the target folder's workspace grants (or
// override's) on top. A run reads no project file, so nothing is asked.
func runConfig(userFile, configFile, dir, override, home string) (config, string, error) {
	cfg, err := toolConfig(userFile, configFile)
	if err != nil {
		return config{}, "", err
	}
	ws, err := cfg.workspaceFor(dir, override, home)
	if err != nil {
		return config{}, "", err
	}
	cfg.merge(cfg.Workspaces[ws].grants())
	cfg, err = cfg.finish()
	return cfg, ws, err
}

// allowsEgress reports whether an egress request (host:port, the host a
// name or *.domain, the port a number or *) is inside one of the
// patterns: the same entry, a wildcard over its host (*.a.com covers
// x.a.com and *.b.a.com, not a.com), a * port over any port, or *.
func allowsEgress(patterns []string, req string) bool {
	host, port, _ := strings.Cut(req, ":")
	for _, p := range patterns {
		if p == "*" {
			return true
		}
		ph, pp, _ := strings.Cut(p, ":")
		if pp != "*" && pp != port {
			continue
		}
		if ph == host || strings.HasPrefix(ph, "*.") && strings.HasSuffix(host, ph[1:]) && len(host) > len(ph)-1 {
			return true
		}
	}
	return false
}

// allowsService reports whether port, or *, is listed.
func allowsService(allow []string, port int) bool {
	return slices.Contains(allow, "*") || slices.Contains(allow, strconv.Itoa(port))
}

// allowsSecret reports whether a secret's source (env:VAR) is a Mac
// variable listed: what matters is what's read, not the guest's name.
func allowsSecret(allow []string, ref string) bool {
	return slices.Contains(allow, strings.TrimPrefix(ref, "env:"))
}

// allowsMount reports whether a mount's Mac path is one of the allowed
// paths or inside one, from rw alone when the mount is rw; all of them
// with ~ expanded and symlinks resolved.
func allowsMount(ro, rw []string, host string, writable bool, home string) bool {
	host = macPath(host, home)
	lists := [][]string{rw}
	if !writable {
		lists = append(lists, ro)
	}
	for _, list := range lists {
		for _, p := range list {
			if under(host, macPath(p, home)) {
				return true
			}
		}
	}
	return false
}

// _macRuns are what a tool on the Mac runs or reads as configuration, from the
// home: a guest that can write ~/.gitconfig sets core.fsmonitor, and it runs at
// your next git command anywhere. ponytail: a list, and it can't be complete; a
// read-write link is for data.
var _macRuns = []string{".ssh", ".gnupg", ".aws", ".config", ".claude", "Library", ".gitconfig",
	".zshrc", ".zshenv", ".zprofile", ".zlogin", ".bashrc", ".bash_profile", ".profile", ".npmrc"}

// checkLinkPaths refuses what no allowance or approval may pass: a read-write
// link that is, holds or is inside what could run code on the Mac, and any link
// overlapping the project, which would reach past its hide and protect. All by
// resolved path, compared as APFS does; ponytail: case alone, no Unicode
// normalisation.
func (c config) checkLinkPaths(home, project string, getenv func(string) string) error {
	fold := func(p string) string { return strings.ToLower(macPath(p, home)) }
	overlap := func(a, b string) bool { return under(a, b) || under(b, a) }
	runs := map[string]string{}
	for _, name := range _macRuns {
		runs[fold(filepath.Join(home, name))] = "~/" + name
	}
	if dir := getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		runs[fold(dir)] = "$CLAUDE_CONFIG_DIR"
	}
	for _, dir := range filepath.SplitList(getenv("PATH")) {
		if filepath.IsAbs(dir) {
			runs[fold(dir)] = "$PATH's " + dir
		}
	}
	for _, l := range c.links() {
		p := fold(l.Host)
		if l.Mode == "rw" && under(fold(home), p) {
			return fmt.Errorf("mounts: %s: a read-write link can't be your home folder, or above it", l.Host)
		}
		if overlap(p, fold(project)) {
			return fmt.Errorf("mounts: %s: a link can't hold the project or be inside it, as it would reach past hide and protect; dev.mount links the project", l.Host)
		}
		if l.Mode != "rw" {
			continue
		}
		for _, at := range slices.Sorted(maps.Keys(runs)) {
			if overlap(p, at) {
				return fmt.Errorf("mounts: %s: a read-write link can't reach %s: a tool on the Mac reads or runs it, so the guest could run code there; link it read-only, or link a folder of data", l.Host, runs[at])
			}
		}
	}
	return nil
}

// request is one thing a project's sbox.yaml asks for that reaches past
// its VM: as approvals and BLOCKED lines name it (entry), and as the
// workspace's allow list would take it (list, value).
type request struct {
	key, entry  string // egress, service, mount, secret; "evil.example:443", "~/.ssh → /home/runner/.ssh"
	list, value string // allow.<list>: [value]
}

// id is how approvals remember a request.
func (r request) id() string { return r.key + " " + r.entry }

// blocked is the request's BLOCKED line in workspace ws, with the key to
// add.
func (r request) blocked(ws string) string {
	value := "[" + r.value + "]"
	if r.list == "live" {
		value = r.value
	}
	return fmt.Sprintf("BLOCKED %-7s %s  not allowed in workspace %s → workspaces.%s.allow.%s: %s", r.key, r.entry, ws, ws, r.list, value)
}

// blockedError is a project asking for more than its workspace allows:
// the BLOCKED lines, and the config without those requests.
type blockedError struct {
	dir, ws string
	misses  []request
	cfg     config
	shown   bool // the lines were printed already, with the question
}

func (e *blockedError) Error() string {
	return fmt.Sprintf("%s asks for more than workspace %s allows", e.dir, e.ws)
}

// lines are the BLOCKED lines.
func (e *blockedError) lines() []string {
	var out []string
	for _, r := range e.misses {
		out = append(out, r.blocked(e.ws))
	}
	return out
}

// layered builds a project's config through its workspace: the user's
// file (global grants), the workspace's grants, then the project's own
// file, whose egress, services, mounts and secrets each stay only if
// already granted, allowed by the workspace, or approved (by request id).
// It returns that config, the workspace's name and what was left out.
func layered(userFile, projectFile, dir, override, home string, approved map[string]bool) (config, string, []request, error) {
	user, err := readConfigFile(userFile, true)
	if err != nil {
		return config{}, "", nil, err
	}
	project, err := readConfigFile(projectFile, false)
	if err != nil {
		return config{}, "", nil, err
	}
	ws, err := user.workspaceFor(dir, override, home)
	if err != nil {
		return config{}, "", nil, err
	}
	w := user.Workspaces[ws]
	grants := w.grants()

	// What the project may have: the grants, and the workspace's allow.
	egress := slices.Concat(user.Egress, w.Egress, w.Allow.Egress)
	services := slices.Clone(w.Allow.Services)
	for _, p := range slices.Concat(user.Services, w.Services) {
		services = append(services, strconv.Itoa(p))
	}
	ro, rw := slices.Clone(w.Allow.Mounts), slices.Clone(w.Allow.RW)
	for _, m := range slices.Concat(user.Mounts, w.Mounts) {
		if m.Mode == "rw" {
			rw = append(rw, m.Host)
		} else {
			ro = append(ro, m.Host)
		}
	}
	vars := slices.Clone(w.Allow.Secrets)
	for ref := range maps.Values(user.Secrets) {
		vars = append(vars, strings.TrimPrefix(ref, "env:"))
	}
	for ref := range maps.Values(w.Secrets) {
		vars = append(vars, strings.TrimPrefix(ref, "env:"))
	}

	var misses []request
	keep := func(r request, ok bool) bool {
		if ok || approved[r.id()] {
			return true
		}
		misses = append(misses, r)
		return false
	}
	project.Egress = slices.DeleteFunc(project.Egress, func(e string) bool {
		return !keep(request{"egress", e, "egress", strings.TrimSuffix(e, ":443")}, allowsEgress(egress, e))
	})
	project.Services = slices.DeleteFunc(project.Services, func(p int) bool {
		return !keep(request{"service", strconv.Itoa(p), "services", strconv.Itoa(p)}, allowsService(services, p))
	})
	liveWrites := w.Allow.Live || w.Mount == "live" || user.Dev.Mount == "live"
	project.Mounts = slices.DeleteFunc(project.Mounts, func(m mount) bool {
		allowed := allowsMount(ro, rw, m.Host, m.Mode == "rw", home)
		r := request{"mount", m.Host + " → " + m.Guest, "mounts", m.Host}
		switch {
		case m.Live && m.Mode == "rw": // no review step, so allow.rw alone isn't enough: it was written for run -apply
			r.entry, r.list = r.entry+" (live, rw)", "rw"
			if allowed && !liveWrites {
				r.list, r.value = "live", "true"
			}
			allowed = allowed && liveWrites
		case m.Live:
			r.entry += " (live)"
		case m.Mode == "rw":
			r.entry, r.list = r.entry+" (rw)", "rw"
		}
		return !keep(r, allowed)
	})
	// A live mount puts the Mac's folder in reach of the guest's writes; a
	// read-only one, of its reads, past what copy mode leaves out.
	if ask := project.Dev.Mount; ask == "live" || ask == "live-ro" {
		granted := w.Mount == "live" || user.Dev.Mount == "live" ||
			ask == "live-ro" && (w.Mount == "live-ro" || user.Dev.Mount == "live-ro")
		if !keep(request{"mount", ask, "live", "true"}, w.Allow.Live || granted) {
			project.Dev.Mount = ""
		}
	}
	for _, name := range slices.Sorted(maps.Keys(project.Secrets)) {
		ref := project.Secrets[name]
		if !keep(request{"secret", name + " ← " + ref, "secrets", strings.TrimPrefix(ref, "env:")}, allowsSecret(vars, ref)) {
			delete(project.Secrets, name)
		}
	}

	var cfg config
	cfg.merge(user)
	cfg.merge(grants)
	cfg.merge(project)
	cfg, err = cfg.finish()
	return cfg, ws, misses, err
}

// approvedFile is where the requests the user allowed for one project are
// kept, next to its sync base; sbox clean removes it with the rest.
func approvedFile(stateDir, project string) string {
	return filepath.Join(stateDir, "projects", project, "approved.json")
}

// loadApproved reads a project's approvals: request id → the day it was
// approved. None when the file is missing or unreadable.
func loadApproved(stateDir, project string) map[string]string {
	approved := map[string]string{}
	if data, err := os.ReadFile(approvedFile(stateDir, project)); err == nil {
		_ = json.Unmarshal(data, &approved)
	}
	return approved
}

// saveApproved adds requests to a project's approvals, dated today.
func saveApproved(stateDir, project string, misses []request) error {
	approved := loadApproved(stateDir, project)
	for _, r := range misses {
		approved[r.id()] = time.Now().Format(time.DateOnly)
	}
	data, err := json.MarshalIndent(approved, "", "  ")
	if err != nil {
		return err
	}
	p := approvedFile(stateDir, project)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, append(data, '\n'), 0o644)
}

// askApprove shows the BLOCKED lines and asks on out whether to allow
// them for this project alone, reading the answer from in.
func askApprove(in io.Reader, out io.Writer) func(lines []string) bool {
	return func(lines []string) bool {
		for _, line := range lines {
			_, _ = fmt.Fprintln(out, line)
		}
		_, _ = fmt.Fprint(out, "sbox: allow these for this project alone, and remember? [y/N] ")
		answer, _ := bufio.NewReader(in).ReadString('\n')
		return strings.HasPrefix(strings.ToLower(strings.TrimSpace(answer)), "y")
	}
}
