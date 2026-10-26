package main

import (
	"maps"
	"slices"
	"strings"
	"testing"
)

func TestFindVM(t *testing.T) {
	t.Parallel()

	out := []byte(`{"name":"default","status":"Running","config":{"nestedVirtualization":false}}
{"name":"sbox","status":"Stopped","config":{"nestedVirtualization":true}}
`)
	tests := []struct {
		give     string
		wantNil  bool
		wantStat string
	}{
		{give: "sbox", wantStat: "Stopped"},
		{give: "missing", wantNil: true},
	}

	for _, tt := range tests {
		t.Run(tt.give, func(t *testing.T) {
			t.Parallel()
			vm, err := findVM(out, tt.give)
			if err != nil {
				t.Fatal(err)
			}
			if (vm == nil) != tt.wantNil {
				t.Fatalf("vm = %+v, want nil %v", vm, tt.wantNil)
			}
			if vm != nil && (vm.Status != tt.wantStat || !vm.Config.NestedVirtualization) {
				t.Errorf("vm = %+v, want status %s with nested virtualization", vm, tt.wantStat)
			}
		})
	}
}

func TestJudge(t *testing.T) {
	t.Parallel()

	healthy := func() map[string]string {
		return parseKV(`noise from the shell
SBOX provisioned=yes
SBOX kvm=present
SBOX firecracker=`+_firecrackerVersion+`
SBOX firecracker-sha=`+pinnedSHA("FIRECRACKER")+`
SBOX kernel-sha=`+pinnedSHA("KERNEL")+`
SBOX rootfs=present
SBOX runner=present
SBOX boot-exit=0
SBOX boot-ms=2600
  SBOX node=`+_nodeVersion+`
SBOX uv=`+_uvVersion+`
SBOX python=3.12.3
SBOX strace=6.8`, "SBOX ")
	}

	tests := []struct {
		name     string
		change   map[string]string
		wantFail string // check that must fail; "" means all pass
	}{
		{name: "healthy"},
		{name: "tampered kernel", change: map[string]string{"kernel-sha": "abc"}, wantFail: "kernel checksum"},
		{name: "no kvm", change: map[string]string{"kvm": "missing"}, wantFail: "kvm"},
		{name: "boot failed", change: map[string]string{"boot-exit": "1", "node": ""}, wantFail: "microVM boot"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			kv := healthy()
			maps.Copy(kv, tt.change)
			var failed []string
			for _, c := range judge(kv) {
				if !c.ok {
					failed = append(failed, c.name)
				}
			}
			switch {
			case tt.wantFail == "" && len(failed) > 0:
				t.Errorf("failed checks %v, want none", failed)
			case tt.wantFail != "" && !slices.Contains(failed, tt.wantFail):
				t.Errorf("failed checks %v, want %q among them", failed, tt.wantFail)
			}
		})
	}
}

func TestJudgeUnprovisioned(t *testing.T) {
	t.Parallel()
	if got := judge(parseKV("SBOX provisioned=no", "SBOX ")); len(got) != 1 || got[0].ok {
		t.Errorf("judge = %+v, want a single failing provisioned check", got)
	}
}

func TestJudgeProbe(t *testing.T) {
	t.Parallel()

	healthy := "│ PROBE net-host=ok\n│ PROBE net-internet=blocked\n│ PROBE uid=1000\n│ PROBE rootfs=readonly\n"
	tests := []struct {
		name     string
		give     string
		wantFail string
	}{
		{name: "isolated", give: healthy},
		{name: "internet reachable", give: strings.Replace(healthy, "blocked", "reachable", 1), wantFail: "run: cut off from internet"},
		{name: "runs as root", give: strings.Replace(healthy, "uid=1000", "uid=0", 1), wantFail: "run: unprivileged guest user"},
		{name: "no output", give: "", wantFail: "run: guest reaches VM side"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var fails []string
			for _, c := range judgeProbe(parseKV(tt.give, "PROBE ")) {
				if !c.ok {
					fails = append(fails, c.name)
				}
			}
			if (tt.wantFail == "") != (len(fails) == 0) || (tt.wantFail != "" && !slices.Contains(fails, tt.wantFail)) {
				t.Errorf("failed checks %v, want %q", fails, tt.wantFail)
			}
		})
	}
}
