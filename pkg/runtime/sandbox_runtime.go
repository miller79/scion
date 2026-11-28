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
	"encoding/json"
	"fmt"
	"path"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// sandboxDockerDir is where an inner Docker engine keeps its state.
const sandboxDockerDir = "/var/lib/docker"

// SandboxKindEnvVar tells the agent image which sandbox it runs under, so its
// entrypoint starts an inner Docker engine only under a sandbox and picks the
// matching engine flags without guessing from the kernel.
const SandboxKindEnvVar = "SCION_SANDBOX_KIND"

// gvisorInnerEngineCaps is the capability set an inner Docker engine needs
// under gVisor: the list from gVisor's Docker-in-gVisor tutorial, without
// SYS_PTRACE (debuggers only). These are granted inside gVisor's user-space
// kernel only, never on the host, and are only ever added together with the
// sandbox runtime.
var gvisorInnerEngineCaps = []string{
	"AUDIT_WRITE", "CHOWN", "DAC_OVERRIDE", "FOWNER", "FSETID", "KILL", "MKNOD",
	"NET_ADMIN", "NET_BIND_SERVICE", "NET_RAW", "SETFCAP", "SETGID", "SETPCAP",
	"SETUID", "SYS_ADMIN", "SYS_CHROOT",
}

// sandboxIsGVisor reports whether the run uses a gVisor sandbox runtime.
func sandboxIsGVisor(cfg RunConfig) bool {
	return cfg.Sandbox != nil && cfg.Sandbox.Kind == config.SandboxKindGVisor
}

// runtimeManagedInterception reports whether the runtime installs the
// metadata redirect from a setup container sharing the agent's network
// namespace. It does not under gVisor, whose own network stack never sees
// those host-side rules; sciontool installs the redirect inside the sandbox
// instead (it needs iptables-legacy in the image: gVisor implements the
// legacy xtables interface, not nftables).
//
// That in-sandbox redirect only covers the agent's own traffic, and the agent
// is root inside the sandbox through its inner Docker engine: inner
// containers' traffic is forwarded, not redirected, and the agent can remove
// the rules. The host must block the metadata server for sandbox agents (see
// the operator runbook); the in-sandbox redirect keeps the agent's tools on
// Scion's metadata server.
func runtimeManagedInterception(cfg RunConfig) bool {
	return cfg.MetadataInterception && !sandboxIsGVisor(cfg)
}

// InitFirewallRequested reports whether the agent container would see
// SCION_INIT_FIREWALL set to true, from the same sources the run arguments
// use.
func InitFirewallRequested(cfg RunConfig) bool {
	val, ok := effectiveInitFirewall(cfg)
	if !ok {
		return false
	}
	on, err := CloudRunInitFirewallRequested(val)
	return err == nil && on
}

// appendSandboxRuntimeArgs adds the operator-declared sandbox runtime and the
// flags its kind needs for an inner container engine. The flags are added
// only together with --runtime and only for the matching kind: under the
// default runtime they would give the agent host-level privileges, so
// DockerRuntime.Run first checks the runtime really is that sandbox
// (verifySandboxRuntime).
//
// gVisor: the inner-engine capabilities (gvisorInnerEngineCaps) and an
// unconfined engine-level seccomp profile (both confined to gVisor's
// user-space kernel, which installs its own seccomp filter on the host), plus
// an executable tmpfs for /var/lib/docker (Docker's default --tmpfs is
// noexec, which fails every inner container). Sysbox needs nothing beyond
// --runtime. Both get SCION_SANDBOX_KIND.
func appendSandboxRuntimeArgs(args []string, cfg RunConfig) []string {
	s := cfg.Sandbox
	if s == nil || s.Name == "" {
		return args
	}
	args = append(args, "--runtime", s.Name, "-e", SandboxKindEnvVar+"="+s.Kind)
	if s.Kind == config.SandboxKindGVisor {
		for _, c := range gvisorInnerEngineCaps {
			args = append(args, "--cap-add", c)
		}
		args = append(args,
			"--security-opt", "seccomp=unconfined",
			"--tmpfs", sandboxDockerDir+":rw,exec,size="+s.StorageSize(),
		)
	}
	return args
}

// engineRuntime is one entry of `docker info --format '{{json .Runtimes}}'`.
type engineRuntime struct {
	Path        string `json:"path"`
	RuntimeType string `json:"runtimeType"`
}

// sandboxRuntimeMatchesKind reports whether a registered engine runtime is
// the sandbox kind declared for it: its binary (or containerd shim type) must
// be gVisor's runsc or Sysbox's sysbox-runc. An alias that points at runc, or
// at a different sandbox, does not match, so the gVisor flags can never be
// combined with a host kernel runtime by a naming mistake.
func sandboxRuntimeMatchesKind(rt engineRuntime, kind string) bool {
	bin := path.Base(rt.Path)
	switch kind {
	case config.SandboxKindGVisor:
		return bin == "runsc" || rt.RuntimeType == "io.containerd.runsc.v1"
	case config.SandboxKindSysbox:
		return bin == "sysbox-runc"
	}
	return false
}

// checkSandboxRuntimeInfo checks the engine's runtime table (the JSON of
// `docker info --format '{{json .Runtimes}}'`) for the declared sandbox.
func checkSandboxRuntimeInfo(infoJSON string, s *config.V1SandboxRuntime) error {
	var rts map[string]engineRuntime
	if err := json.Unmarshal([]byte(strings.TrimSpace(infoJSON)), &rts); err != nil {
		return fmt.Errorf("sandbox runtime %q: cannot read the engine's runtimes: %w", s.Name, err)
	}
	rt, ok := rts[s.Name]
	if !ok {
		return fmt.Errorf("sandbox runtime %q is not registered with the container engine", s.Name)
	}
	if !sandboxRuntimeMatchesKind(rt, s.Kind) {
		return fmt.Errorf("sandbox runtime %q (path %q, type %q) is not a %s runtime; refusing to add sandbox-only privileges",
			s.Name, rt.Path, rt.RuntimeType, s.Kind)
	}
	return nil
}

// checkEngineSandboxRuntime checks the engine's runtime table, through the
// engine CLI, for the declared sandbox.
func checkEngineSandboxRuntime(ctx context.Context, command string, s *config.V1SandboxRuntime) error {
	out, err := runSimpleCommand(ctx, command, "info", "--format", "{{json .Runtimes}}")
	if err != nil {
		return fmt.Errorf("sandbox runtime %q: cannot list the engine's runtimes: %w", s.Name, err)
	}
	return checkSandboxRuntimeInfo(out, s)
}

// CheckSandboxRuntime reports an error unless rt is the Docker runtime and its
// engine's runtime named by s really is the declared sandbox kind. The agent
// start calls it before launching, so a mismatch is refused as agent policy;
// DockerRuntime.Run checks again (verifySandboxRuntime) before adding the
// sandbox-only flags.
func CheckSandboxRuntime(ctx context.Context, rt Runtime, s *config.V1SandboxRuntime) error {
	if s == nil {
		return nil
	}
	d, ok := rt.(*DockerRuntime)
	if !ok {
		return fmt.Errorf("sandbox runtime %q: only the Docker runtime is supported", s.Name)
	}
	return checkEngineSandboxRuntime(ctx, d.Command, s)
}

// verifySandboxRuntime refuses the launch unless the engine's runtime named by
// cfg.Sandbox really is the declared sandbox kind.
func verifySandboxRuntime(ctx context.Context, command string, cfg RunConfig) error {
	if cfg.Sandbox == nil || cfg.Sandbox.Name == "" {
		return nil
	}
	if err := checkEngineSandboxRuntime(ctx, command, cfg.Sandbox); err != nil {
		return newLaunchFailure(api.LaunchCodeAgentPolicyRefused, api.LaunchStepLaunching, "", err)
	}
	return nil
}
