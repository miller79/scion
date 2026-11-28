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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
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
