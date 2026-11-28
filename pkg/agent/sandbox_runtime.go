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
	"log/slog"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

// sandboxWorkloadHeadroom is the memory a gVisor agent should have beyond its
// /var/lib/docker tmpfs for the harness, build tools and inner containers.
const sandboxWorkloadHeadroom = int64(2) << 30

// checkSandboxMemory checks a gVisor agent's final memory limit against its
// /var/lib/docker tmpfs, which lives in memory and counts against the limit:
// a limit at or below the tmpfs size is refused (filling the image store
// would OOM-kill the agent), a limit with less than sandboxWorkloadHeadroom
// beyond it, or no limit at all, is logged.
func checkSandboxMemory(agentName string, s *config.V1SandboxRuntime, res *api.ResourceSpec) error {
	if s == nil || s.Kind != config.SandboxKindGVisor {
		return nil
	}
	tmpfs := s.StorageSizeBytes()
	if res == nil || res.Limits.Memory == "" {
		slog.Warn("gVisor sandbox has no memory limit; its in-memory Docker image store and the workload are unbounded",
			"agent", agentName, "docker_storage_size", s.StorageSize())
		return nil
	}
	mem, err := util.ParseMemory(res.Limits.Memory)
	if err != nil {
		return nil // the runtime reports the invalid limit
	}
	if mem <= tmpfs {
		return refuseAgentPolicy(agentName, NewAgentPolicyError("resources.limits.memory", "sandbox_runtime.docker_storage_size",
			"the memory limit %s does not exceed the sandbox's in-memory Docker storage (%s); raise the limit or lower docker_storage_size",
			res.Limits.Memory, s.StorageSize()))
	}
	if mem-tmpfs < sandboxWorkloadHeadroom {
		slog.Warn("gVisor sandbox leaves less than 2 GiB of memory beyond its in-memory Docker storage",
			"agent", agentName, "memory_limit", res.Limits.Memory, "docker_storage_size", s.StorageSize())
	}
	return nil
}

// loadProfileSandboxRuntime is a seam for tests.
var loadProfileSandboxRuntime = config.LoadProfileSandboxRuntime

// resolveStartSandboxRuntime returns the sandbox runtime this start uses: the
// profile's sandbox_runtime from the global settings, when the
// agent_policy.sandbox_runtime experiment is on and the runtime is Docker or
// Podman. An invalid block refuses the start. With the experiment off, or on
// another runtime, the block is ignored with a warning and the agent starts
// on the default runtime (it then has no inner container engine).
func resolveStartSandboxRuntime(ctx context.Context, agentName, profile, runtimeName string) (*config.V1SandboxRuntime, error) {
	s, err := loadProfileSandboxRuntime(profile)
	if err != nil {
		slog.Warn("Sandbox runtime could not be loaded; refusing the start", "agent", agentName, "profile", profile, "error", err)
		return nil, refuseAgentPolicy(agentName, NewAgentPolicyError("profile", "profiles."+profile+".sandbox_runtime",
			"the profile's sandbox_runtime is invalid; ask the operator to check it"))
	}
	if s == nil {
		return nil, nil
	}
	if !api.HubAgentDefaultsFromContext(ctx).ExperimentEnabled(experiments.AgentSandboxRuntime) {
		slog.Warn("Profile declares sandbox_runtime but the experiment is off; starting on the default runtime",
			"agent", agentName, "profile", profile, "experiment", experiments.AgentSandboxRuntime)
		return nil, nil
	}
	if runtimeName != "docker" && runtimeName != "podman" {
		slog.Warn("sandbox_runtime applies to Docker and Podman only; ignoring it",
			"agent", agentName, "profile", profile, "runtime", runtimeName)
		return nil, nil
	}
	slog.Info("Start: sandbox runtime", "agent", agentName, "profile", profile, "runtime", s.Name, "kind", s.Kind)
	return s, nil
}
