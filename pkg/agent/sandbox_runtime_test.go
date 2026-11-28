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

package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

func TestCheckSandboxMemory(t *testing.T) {
	gv := &config.V1SandboxRuntime{Name: "runsc", Kind: config.SandboxKindGVisor, DockerStorageSize: "6g"}
	mem := func(m string) *api.ResourceSpec { return &api.ResourceSpec{Limits: api.ResourceList{Memory: m}} }
	tests := []struct {
		name    string
		s       *config.V1SandboxRuntime
		res     *api.ResourceSpec
		wantErr bool
	}{
		{"no sandbox", nil, mem("1Gi"), false},
		{"sysbox ignored", &config.V1SandboxRuntime{Name: "sysbox-runc", Kind: config.SandboxKindSysbox}, mem("1Gi"), false},
		{"no limit warns only", gv, nil, false},
		{"empty limit warns only", gv, mem(""), false},
		{"below tmpfs refused", gv, mem("4Gi"), true},
		{"equal to tmpfs refused", gv, mem("6Gi"), true},
		{"small headroom warns only", gv, mem("7Gi"), false},
		{"enough headroom", gv, mem("8Gi"), false},
		{"default size 6g, 8Gi ok", &config.V1SandboxRuntime{Name: "runsc", Kind: config.SandboxKindGVisor}, mem("8Gi"), false},
		{"invalid limit left to runtime", gv, mem("lots"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkSandboxMemory("a", tt.s, tt.res)
			if (err != nil) != tt.wantErr {
				t.Fatalf("checkSandboxMemory() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestResolveStartSandboxRuntime(t *testing.T) {
	gv := &config.V1SandboxRuntime{Name: "runsc", Kind: config.SandboxKindGVisor}
	on := api.ContextWithHubAgentDefaults(context.Background(), &api.HubAgentDefaults{Experiments: []string{experiments.AgentSandboxRuntime}})
	off := context.Background()
	tests := []struct {
		name    string
		ctx     context.Context
		loaded  *config.V1SandboxRuntime
		loadErr error
		runtime string
		want    bool
		wantErr bool
	}{
		{"off ignores valid block", off, gv, nil, "docker", false, false},
		{"off ignores invalid block", off, nil, errors.New("bad"), "docker", false, false},
		{"on uses block on docker", on, gv, nil, "docker", true, false},
		{"on refuses invalid block", on, nil, errors.New("bad"), "docker", false, true},
		{"on ignores podman", on, gv, nil, "podman", false, false},
		{"on ignores kubernetes", on, gv, nil, "kubernetes", false, false},
		{"on no block", on, nil, nil, "docker", false, false},
	}
	orig := loadProfileSandboxRuntime
	t.Cleanup(func() { loadProfileSandboxRuntime = orig })
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			loadProfileSandboxRuntime = func(string) (*config.V1SandboxRuntime, error) { return tt.loaded, tt.loadErr }
			got, err := resolveStartSandboxRuntime(tt.ctx, "a", "build", tt.runtime)
			if (err != nil) != tt.wantErr || (got != nil) != tt.want {
				t.Fatalf("resolveStartSandboxRuntime() = %+v, %v; want sandbox %v, err %v", got, err, tt.want, tt.wantErr)
			}
		})
	}
}

func TestHubEndpointIsLoopback(t *testing.T) {
	for ep, want := range map[string]bool{
		"http://localhost:8080": true, "http://127.0.0.1:8080": true, "http://[::1]:8080": true,
		"http://host.docker.internal:8080": false, "https://hub.example.com": false, "": false,
	} {
		if got := hubEndpointIsLoopback(map[string]string{"SCION_HUB_ENDPOINT": ep}); got != want {
			t.Errorf("hubEndpointIsLoopback(%q) = %v, want %v", ep, got, want)
		}
	}
	if !hubEndpointIsLoopback(map[string]string{"SCION_HUB_URL": "http://localhost:9810"}) {
		t.Error("SCION_HUB_URL loopback not detected")
	}
}

func TestCheckSandboxInitFirewall(t *testing.T) {
	gv := &config.V1SandboxRuntime{Name: "runsc", Kind: config.SandboxKindGVisor}
	sb := &config.V1SandboxRuntime{Name: "sysbox-runc", Kind: config.SandboxKindSysbox}
	fw := []string{"SCION_INIT_FIREWALL=true"}
	tests := []struct {
		name    string
		cfg     runtime.RunConfig
		wantErr bool
	}{
		{"gvisor with firewall refused", runtime.RunConfig{Sandbox: gv, Env: fw}, true},
		{"gvisor without firewall", runtime.RunConfig{Sandbox: gv}, false},
		{"gvisor firewall false", runtime.RunConfig{Sandbox: gv, Env: []string{"SCION_INIT_FIREWALL=false"}}, false},
		{"sysbox with firewall refused", runtime.RunConfig{Sandbox: sb, Env: fw}, true},
		{"no sandbox with firewall", runtime.RunConfig{Env: fw}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := tt.cfg
			if err := checkSandboxInitFirewall("a", "build", &cfg); (err != nil) != tt.wantErr {
				t.Fatalf("checkSandboxInitFirewall() = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
