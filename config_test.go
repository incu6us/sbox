package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestLoadConfig(t *testing.T) {
	t.Parallel()
	write := func(t *testing.T, body string) string {
		p := filepath.Join(t.TempDir(), "sbox.yaml")
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	missing := filepath.Join(t.TempDir(), "none.yaml")

	t.Run("none", func(t *testing.T) {
		t.Parallel()
		cfg, err := loadConfig(missing, missing)
		if err != nil || len(cfg.Ports)+len(cfg.Services) != 0 {
			t.Errorf("loadConfig with no files = %+v, %v; want empty", cfg, err)
		}
	})

	t.Run("lists append, duplicates once", func(t *testing.T) {
		t.Parallel()
		user := write(t, "services: [5432]\nports: [8080]\n")
		project := write(t, "# the app\nports: [3000, 8080]\nservices: [6379]\n")
		cfg, err := loadConfig(user, project)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(cfg.Ports, []int{3000, 8080}) || !slices.Equal(cfg.Services, []int{5432, 6379}) {
			t.Errorf("loadConfig = %+v, want ports [3000 8080], services [5432 6379]", cfg)
		}
	})

	for _, tt := range []struct {
		name, body, wantErr string
	}{
		{"unknown key", "prots: [3000]\n", "prots"},
		{"privileged port", "ports: [80]\n", "1024"},
		{"port out of range", "ports: [70000]\n", "65535"},
		{"service out of range", "services: [0]\n", "1 to 65535"},
		{"not yaml", "ports: [3000\n", "sbox.yaml"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			project := write(t, tt.body)
			_, err := loadConfig(missing, project)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) || !strings.Contains(err.Error(), project) {
				t.Errorf("err = %v, want one naming the file and %q", err, tt.wantErr)
			}
		})
	}
}
