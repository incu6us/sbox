package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"slices"

	"gopkg.in/yaml.v3"
)

// config is what sbox's YAML files set. Two files, fixed names, no
// includes: the user's (~/.config/sbox/config.yaml) and, for sbox dev, the
// project's (sbox.yaml in its folder). Lists append; a key sbox doesn't
// know is an error. Keys arrive with the steps that need them.
type config struct {
	Ports    []int `yaml:"ports"`    // guest ports forwarded to the Mac's localhost
	Services []int `yaml:"services"` // Mac ports the guest reaches at host.sbox
}

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
	}
	slices.Sort(cfg.Ports)
	slices.Sort(cfg.Services)
	cfg.Ports, cfg.Services = slices.Compact(cfg.Ports), slices.Compact(cfg.Services)
	return cfg, nil
}

func (c config) check() error {
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
