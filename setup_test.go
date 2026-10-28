package main

import "testing"

func TestProvisionEnv(t *testing.T) {
	t.Parallel()
	var cfg config
	env := provisionEnv(cfg)
	if env["FIRECRACKER_SHA256"] != pinnedSHA("FIRECRACKER") || env["CLAUDE_URL"] == "" {
		t.Errorf("env %v lacks the pinned artifacts", env)
	}
	if v, ok := env["GUEST_EXTRA_PACKAGES"]; !ok || v != "" {
		t.Errorf("GUEST_EXTRA_PACKAGES = %q, %v; want set and empty, so a stale value can't leak in", v, ok)
	}
	cfg.Image.Packages = []string{"g++", "golang-go"}
	if got := provisionEnv(cfg)["GUEST_EXTRA_PACKAGES"]; got != "g++,golang-go" {
		t.Errorf("GUEST_EXTRA_PACKAGES = %q, want the packages joined for mmdebstrap's --include", got)
	}
}
