// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package runtime

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
)

func TestAppendSandboxRuntimeArgs(t *testing.T) {
	base := []string{"run", "-t"}
	tests := []struct {
		name    string
		sandbox *config.V1SandboxRuntime
		want    []string
	}{
		{"none", nil, []string{"run", "-t"}},
		{"empty name", &config.V1SandboxRuntime{Kind: config.SandboxKindGVisor}, []string{"run", "-t"}},
		{"gvisor default size", &config.V1SandboxRuntime{Name: "runsc-dind", Kind: config.SandboxKindGVisor},
			[]string{"run", "-t", "--runtime", "runsc-dind", "-e", "SCION_SANDBOX_KIND=gvisor", "--cap-add", "AUDIT_WRITE", "--cap-add", "CHOWN", "--cap-add", "DAC_OVERRIDE", "--cap-add", "FOWNER", "--cap-add", "FSETID", "--cap-add", "KILL", "--cap-add", "MKNOD", "--cap-add", "NET_ADMIN", "--cap-add", "NET_BIND_SERVICE", "--cap-add", "NET_RAW", "--cap-add", "SETFCAP", "--cap-add", "SETGID", "--cap-add", "SETPCAP", "--cap-add", "SETUID", "--cap-add", "SYS_ADMIN", "--cap-add", "SYS_CHROOT", "--security-opt", "seccomp=unconfined", "--tmpfs", "/var/lib/docker:rw,exec,size=6g"}},
		{"gvisor size", &config.V1SandboxRuntime{Name: "runsc", Kind: config.SandboxKindGVisor, DockerStorageSize: "2g"},
			[]string{"run", "-t", "--runtime", "runsc", "-e", "SCION_SANDBOX_KIND=gvisor", "--cap-add", "AUDIT_WRITE", "--cap-add", "CHOWN", "--cap-add", "DAC_OVERRIDE", "--cap-add", "FOWNER", "--cap-add", "FSETID", "--cap-add", "KILL", "--cap-add", "MKNOD", "--cap-add", "NET_ADMIN", "--cap-add", "NET_BIND_SERVICE", "--cap-add", "NET_RAW", "--cap-add", "SETFCAP", "--cap-add", "SETGID", "--cap-add", "SETPCAP", "--cap-add", "SETUID", "--cap-add", "SYS_ADMIN", "--cap-add", "SYS_CHROOT", "--security-opt", "seccomp=unconfined", "--tmpfs", "/var/lib/docker:rw,exec,size=2g"}},
		{"sysbox runtime only", &config.V1SandboxRuntime{Name: "sysbox-runc", Kind: config.SandboxKindSysbox},
			[]string{"run", "-t", "--runtime", "sysbox-runc", "-e", "SCION_SANDBOX_KIND=sysbox"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := appendSandboxRuntimeArgs(append([]string(nil), base...), RunConfig{Sandbox: tt.sandbox})
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("appendSandboxRuntimeArgs() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBuildCommonRunArgs_MetadataInterceptionInSandboxUnderGVisor(t *testing.T) {
	hasEnv := func(args []string, kv string) bool {
		for i := 0; i+1 < len(args); i++ {
			if args[i] == "-e" && args[i+1] == kv {
				return true
			}
		}
		return false
	}
	required := MetadataInterceptionEnvVar + "=" + MetadataInterceptionRequired
	runtimeSource := MetadataInterceptionSourceEnvVar + "=" + MetadataInterceptionSourceRuntime
	cfg := RunConfig{Name: "a", Image: "img", RuntimeName: "docker", UnixUsername: "scion", Harness: &harness.Generic{}, MetadataInterception: true}

	args, err := buildCommonRunArgs(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !hasEnv(args, required) || !hasEnv(args, runtimeSource) || !runtimeManagedInterception(cfg) {
		t.Fatalf("default runtime: want runtime-managed interception (%s, %s) in %q", required, runtimeSource, args)
	}

	cfg.Sandbox = &config.V1SandboxRuntime{Name: "runsc", Kind: config.SandboxKindGVisor}
	if args, err = buildCommonRunArgs(cfg); err != nil {
		t.Fatal(err)
	}
	if !hasEnv(args, required) {
		t.Fatalf("gVisor: interception must stay required (%s) in %q", required, args)
	}
	if hasEnv(args, runtimeSource) || runtimeManagedInterception(cfg) {
		t.Fatalf("gVisor: want in-sandbox interception (no %s, no setup container) in %q", runtimeSource, args)
	}

	cfg.Sandbox = &config.V1SandboxRuntime{Name: "sysbox-runc", Kind: config.SandboxKindSysbox}
	if args, err = buildCommonRunArgs(cfg); err != nil {
		t.Fatal(err)
	}
	if !hasEnv(args, runtimeSource) || !runtimeManagedInterception(cfg) {
		t.Fatalf("sysbox: want runtime-managed interception in %q", args)
	}
}

func TestBuildCommonRunArgs_NetRawDropKeptExceptUnderGVisor(t *testing.T) {
	hasNetRawDrop := func(args []string) bool {
		for i := 0; i+1 < len(args); i++ {
			if args[i] == "--cap-drop" && args[i+1] == "NET_RAW" {
				return true
			}
		}
		return false
	}
	cfg := RunConfig{Name: "a", Image: "img", RuntimeName: "docker", UnixUsername: "scion", Harness: &harness.Generic{}}
	args, err := buildCommonRunArgs(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !hasNetRawDrop(args) {
		t.Fatalf("default runtime: want --cap-drop NET_RAW in %q", args)
	}
	cfg.Sandbox = &config.V1SandboxRuntime{Name: "runsc", Kind: config.SandboxKindGVisor}
	if args, err = buildCommonRunArgs(cfg); err != nil {
		t.Fatal(err)
	}
	if hasNetRawDrop(args) {
		t.Fatalf("gVisor sandbox: want no --cap-drop NET_RAW in %q", args)
	}
	cfg.Sandbox = &config.V1SandboxRuntime{Name: "sysbox-runc", Kind: config.SandboxKindSysbox}
	if args, err = buildCommonRunArgs(cfg); err != nil {
		t.Fatal(err)
	}
	if !hasNetRawDrop(args) {
		t.Fatalf("sysbox sandbox: want --cap-drop NET_RAW in %q", args)
	}
}

func TestCheckSandboxRuntimeInfo(t *testing.T) {
	info := `{"runc":{"path":"runc"},"runsc-dind":{"path":"/usr/local/bin/runsc","runtimeArgs":["--net-raw"]},` +
		`"shim-runsc":{"runtimeType":"io.containerd.runsc.v1"},"sneaky":{"path":"/usr/bin/runc"},` +
		`"sysbox-runc":{"path":"/usr/bin/sysbox-runc"},"youki":{"path":"/usr/bin/youki"}}`
	gv := func(n string) *config.V1SandboxRuntime {
		return &config.V1SandboxRuntime{Name: n, Kind: config.SandboxKindGVisor}
	}
	sb := func(n string) *config.V1SandboxRuntime {
		return &config.V1SandboxRuntime{Name: n, Kind: config.SandboxKindSysbox}
	}
	tests := []struct {
		name    string
		info    string
		s       *config.V1SandboxRuntime
		wantErr string
	}{
		{"runsc path", info, gv("runsc-dind"), ""},
		{"runsc shim type", info, gv("shim-runsc"), ""},
		{"sysbox path", info, sb("sysbox-runc"), ""},
		{"alias to runc refused", info, gv("sneaky"), "is not a gvisor runtime"},
		{"youki as gvisor refused", info, gv("youki"), "is not a gvisor runtime"},
		{"sysbox named as gvisor refused", info, gv("sysbox-runc"), "is not a gvisor runtime"},
		{"runsc named as sysbox refused", info, sb("runsc-dind"), "is not a sysbox runtime"},
		{"unregistered", info, gv("runsc"), "not registered"},
		{"bad json", "not json", gv("runsc-dind"), "cannot read"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkSandboxRuntimeInfo(tt.info, tt.s)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("checkSandboxRuntimeInfo() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("checkSandboxRuntimeInfo() = %v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestInitFirewallRequested(t *testing.T) {
	for env, want := range map[string]bool{"SCION_INIT_FIREWALL=true": true, "SCION_INIT_FIREWALL=false": false, "OTHER=1": false} {
		if got := InitFirewallRequested(RunConfig{Env: []string{env}}); got != want {
			t.Errorf("InitFirewallRequested(%q) = %v, want %v", env, got, want)
		}
	}
}

func TestCheckSandboxRuntimeNonDocker(t *testing.T) {
	s := &config.V1SandboxRuntime{Name: "runsc", Kind: config.SandboxKindGVisor}
	if err := CheckSandboxRuntime(context.Background(), &PodmanRuntime{}, s); err == nil || !strings.Contains(err.Error(), "only the Docker runtime") {
		t.Fatalf("CheckSandboxRuntime(podman) = %v, want Docker-only refusal", err)
	}
	if err := CheckSandboxRuntime(context.Background(), &PodmanRuntime{}, nil); err != nil {
		t.Fatalf("CheckSandboxRuntime(nil sandbox) = %v, want nil", err)
	}
}
