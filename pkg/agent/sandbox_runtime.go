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
	"net/url"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
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

// checkSandboxInitFirewall refuses the opt-in init firewall
// (SCION_INIT_FIREWALL) on a sandbox agent. init-firewall.sh resets the NAT
// table and sets FORWARD to DROP, cutting off every inner container; under
// gVisor it runs inside the sandbox, where the operator's firewall allowlist
// is not passed; and the agent is root in its sandbox through its inner
// Docker engine, so it can remove the rules anyway. Egress for a sandbox
// profile belongs on the host (see the operator runbook).
func checkSandboxInitFirewall(agentName, profile string, cfg *runtime.RunConfig) error {
	if cfg == nil || cfg.Sandbox == nil || !runtime.InitFirewallRequested(*cfg) {
		return nil
	}
	return refuseAgentPolicy(agentName, NewAgentPolicyError("SCION_INIT_FIREWALL", "profiles."+profile+".sandbox_runtime",
		"the init firewall is not supported with a sandbox runtime; enforce egress on the host instead"))
}

// hubEndpointIsLoopback reports whether the agent's hub endpoint names the
// loopback interface. Inside a gVisor sandbox (always bridge networking)
// loopback is the sandbox itself, so the agent could never reach the hub.
func hubEndpointIsLoopback(env map[string]string) bool {
	for _, k := range []string{"SCION_HUB_ENDPOINT", "SCION_HUB_URL"} {
		ep := env[k]
		if ep == "" {
			continue
		}
		u, err := url.Parse(ep)
		if err != nil {
			continue
		}
		switch u.Hostname() {
		case "localhost", "127.0.0.1", "::1":
			return true
		}
	}
	return false
}

// loadProfileSandboxRuntime is a seam for tests.
var loadProfileSandboxRuntime = config.LoadProfileSandboxRuntime

// resolveStartSandboxRuntime returns the sandbox runtime this start uses: the
// profile's sandbox_runtime from the global settings, when the
// agent_policy.sandbox_runtime experiment is on and the runtime is Docker.
// With the experiment off the block, valid or not, is ignored with a warning
// (exactly upstream behaviour). With it on, an invalid block refuses the
// start, and a non-Docker runtime ignores the block with a warning (the
// agent then starts on the default runtime with no inner container engine).
// Podman is not supported yet: its runtime names resolve through
// containers.conf, which Scion cannot verify before granting the gVisor
// flags.
func resolveStartSandboxRuntime(ctx context.Context, agentName, profile, runtimeName string) (*config.V1SandboxRuntime, error) {
	s, err := loadProfileSandboxRuntime(profile)
	if !api.HubAgentDefaultsFromContext(ctx).ExperimentEnabled(experiments.AgentSandboxRuntime) {
		if err != nil || s != nil {
			slog.Warn("Profile declares sandbox_runtime but the experiment is off; starting on the default runtime",
				"agent", agentName, "profile", profile, "experiment", experiments.AgentSandboxRuntime, "error", err)
		}
		return nil, nil
	}
	if err != nil {
		slog.Warn("Sandbox runtime could not be loaded; refusing the start", "agent", agentName, "profile", profile, "error", err)
		return nil, refuseAgentPolicy(agentName, NewAgentPolicyError("profile", "profiles."+profile+".sandbox_runtime",
			"the profile's sandbox_runtime is invalid; ask the operator to check it"))
	}
	if s == nil {
		return nil, nil
	}
	if runtimeName != "docker" {
		slog.Warn("sandbox_runtime applies to the Docker runtime only; ignoring it",
			"agent", agentName, "profile", profile, "runtime", runtimeName)
		return nil, nil
	}
	slog.Info("Start: sandbox runtime", "agent", agentName, "profile", profile, "runtime", s.Name, "kind", s.Kind)
	return s, nil
}
