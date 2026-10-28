package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
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
	Ports    []int    `yaml:"ports"`    // guest ports forwarded to the Mac's localhost
	Services []int    `yaml:"services"` // Mac ports the guest reaches at host.sbox
	Egress   []string `yaml:"egress"`   // hosts the guest may reach: name or *.domain, :port or :* (443 when left out)
	Image    struct {
		Packages []string `yaml:"packages"` // Ubuntu packages added to the guest image by sbox setup
	} `yaml:"image"`
	Dev struct {
		Env   map[string]string `yaml:"env"`   // variables in every login: hooks, terminals, the agent
		Setup []string          `yaml:"setup"` // steps run once per home, and again when the list changes
		Start []string          `yaml:"start"` // steps run at every fresh boot; a trailing & runs one in the background
	} `yaml:"dev"`
	Secrets  map[string]string `yaml:"secrets"` // guest variable → env:HOST_VARIABLE, read when a session starts
	Timeouts struct {
		Setup time.Duration `yaml:"setup"` // each dev.setup and dev.start step; 10m when unset
	} `yaml:"timeouts"`
}

var _envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

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
