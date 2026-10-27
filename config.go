package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"

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
}

var _egressPattern = regexp.MustCompile(`^(\*\.)?[a-z0-9-]+(\.[a-z0-9-]+)*(:([0-9]{1,5}|\*))?$`)

// loadConfig reads the user's file, then the project's; a missing file is
// an empty one.
func loadConfig(userFile, projectFile string) (config, error) {
	var cfg config
	for _, p := range []string{userFile, projectFile} {
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
		cfg.Ports = append(cfg.Ports, c.Ports...)
		cfg.Services = append(cfg.Services, c.Services...)
		cfg.Egress = append(cfg.Egress, c.Egress...)
	}
	slices.Sort(cfg.Ports)
	slices.Sort(cfg.Services)
	slices.Sort(cfg.Egress)
	cfg.Ports, cfg.Services, cfg.Egress = slices.Compact(cfg.Ports), slices.Compact(cfg.Services), slices.Compact(cfg.Egress)
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
