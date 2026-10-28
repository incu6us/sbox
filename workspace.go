package main

import (
	"fmt"
	"maps"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
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
	Allow    struct {
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
	dir = macPath(dir, home)
	best, longest := "default", -1
	for _, name := range slices.Sorted(maps.Keys(c.Workspaces)) { // a tie goes to the first name
		for _, p := range c.Workspaces[name].Paths {
			if p = macPath(p, home); under(dir, p) && len(p) > longest {
				best, longest = name, len(p)
			}
		}
	}
	return best, nil
}
