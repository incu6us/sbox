package main

import (
	"fmt"
	"regexp"
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
