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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizeImageReference(t *testing.T) {
	for in, want := range map[string]string{
		"postgres":                            "docker.io/library/postgres",
		"postgres:18-alpine":                  "docker.io/library/postgres:18-alpine",
		"library/postgres:18":                 "docker.io/library/postgres:18",
		"someorg/tool:1":                      "docker.io/someorg/tool:1",
		"docker.io/postgres":                  "docker.io/library/postgres",
		"index.docker.io/someorg/tool":        "docker.io/someorg/tool",
		"Registry.Example.com/approved/img:1": "registry.example.com/approved/img:1",
		"localhost/img:dev":                   "localhost/img:dev",
		"localhost:5000/img":                  "localhost:5000/img",
		"ghcr.io/org/img@sha256:abc":          "ghcr.io/org/img@sha256:abc",
	} {
		assert.Equal(t, want, normalizeImageReference(in), in)
	}
}

func TestCheckAgentPolicyImage(t *testing.T) {
	p := &config.V1AgentPolicy{AllowedImagePrefixes: []string{
		"registry.example.com/approved/",
		"ghcr.io/org/agent",
		"scion-claude",
		"quay.example.com",
		"docker.io/hubns/",
		"otherns/",
	}}
	allowed := map[string]string{
		"registry.example.com/approved/scion-claude:latest": "registry.example.com/approved/scion-claude:latest",
		"registry.example.com/approved/sub/img@sha256:abc":  "registry.example.com/approved/sub/img@sha256:abc",
		"ghcr.io/org/agent":                "ghcr.io/org/agent",
		"ghcr.io/org/agent:v1":             "ghcr.io/org/agent:v1",
		"ghcr.io/org/agent/sub:v1":         "ghcr.io/org/agent/sub:v1",
		"ghcr.io/org/agent@sha256:abc":     "ghcr.io/org/agent@sha256:abc",
		"scion-claude:latest":              "docker.io/library/scion-claude:latest",
		"docker.io/library/scion-claude":   "docker.io/library/scion-claude",
		"quay.example.com/anything/at:all": "quay.example.com/anything/at:all",
		// Docker Hub namespace prefixes.
		"docker.io/hubns/agent:1": "docker.io/hubns/agent:1",
		"hubns/agent:1":           "docker.io/hubns/agent:1",
		"otherns/agent":           "docker.io/otherns/agent",
	}
	for in, want := range allowed {
		got, err := checkAgentPolicyImage(p, "a", in)
		if assert.NoError(t, err, in) {
			assert.Equal(t, want, got, in)
		}
	}
	refused := []string{
		"docker.io/library/postgres:18-alpine",
		"postgres:18-alpine",
		"registry.example.com/approved-evil/img",           // not under approved/
		"registry.example.com/approvedimg",                 // not under approved/
		"ghcr.io/org/agent-evil:v1",                        // boundary: agent vs agent-evil
		"ghcr.io/org/agentx",                               // boundary
		"scion-claude-evil:latest",                         // boundary on a short name
		"evil.example.com/registry.example.com/approved/x", // prefix only matches at the start
		"someorg/scion-claude",                             // docker.io/someorg/... is not docker.io/library/...
		"quay.example.com.evil.net/x",                      // host boundary
		"hubns-evil/agent",                                 // namespace boundary
		"docker.io/library/hubns",                          // a repository named hubns is not the hubns namespace
		"",
	}
	for _, in := range refused {
		_, err := checkAgentPolicyImage(p, "a", in)
		require.Error(t, err, in)
		assert.ErrorIs(t, err, ErrAgentPolicy, in)
		assert.Contains(t, err.Error(), "agent_policy.allowed_image_prefixes", in)
	}
	// No prefix list: the image is untouched.
	got, err := checkAgentPolicyImage(&config.V1AgentPolicy{}, "a", "postgres")
	require.NoError(t, err)
	assert.Equal(t, "postgres", got)
}

func TestCheckAgentPolicyVolumes(t *testing.T) {
	root := t.TempDir()
	allowedDir := filepath.Join(root, "allowed")
	require.NoError(t, os.MkdirAll(filepath.Join(allowedDir, "sub"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "allowed-evil"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "secret"), 0o755))
	// A symlink inside the allowed tree that points out of it.
	require.NoError(t, os.Symlink(filepath.Join(root, "secret"), filepath.Join(allowedDir, "escape")))
	// A symlink inside the allowed tree that stays in it.
	require.NoError(t, os.Symlink(filepath.Join(allowedDir, "sub"), filepath.Join(allowedDir, "inside")))
	t.Setenv("POLICY_TEST_DIR", allowedDir)

	allowlist := &config.V1AgentPolicy{HostVolumes: "allowlist", AllowedHostVolumeSources: []string{allowedDir}}
	vol := func(src string) []api.VolumeMount { return []api.VolumeMount{{Source: src, Target: "/x"}} }

	for _, src := range []string{
		allowedDir,
		filepath.Join(allowedDir, "sub"),
		filepath.Join(allowedDir, "not-yet-created"),
		filepath.Join(allowedDir, "inside"),
		"$POLICY_TEST_DIR/sub",
	} {
		assert.NoError(t, checkAgentPolicyVolumes(allowlist, "a", "docker", vol(src)), src)
	}
	for _, src := range []string{
		"/var/run/docker.sock",
		"/etc",
		filepath.Join(root, "allowed-evil"), // element boundary
		allowedDir + "/../secret",           // traversal (not pre-cleaned)
		allowedDir + "/sub/../../secret",
		allowedDir + "/escape/..", // ".." after a symlink: the link target's parent
		allowedDir + "/escape",    // symlink out of the tree
		allowedDir + "/escape/x",
		"$POLICY_TEST_DIR/../secret",
		"named-volume", // not a host path
		"relative/path",
		"",
	} {
		err := checkAgentPolicyVolumes(allowlist, "a", "docker", vol(src))
		require.Error(t, err, src)
		assert.ErrorIs(t, err, ErrAgentPolicy, src)
		assert.Contains(t, err.Error(), "agent_policy.allowed_host_volume_sources", src)
		// The error repeats the configured value only, never the resolved
		// host path or the allow list.
		assert.Contains(t, err.Error(), quotePolicyValue(src), src)
		if strings.Contains(src, "escape") {
			assert.NotContains(t, err.Error(), filepath.Join(root, "secret"), src)
		}
	}

	deny := &config.V1AgentPolicy{HostVolumes: "deny"}
	err := checkAgentPolicyVolumes(deny, "a", "docker", vol(allowedDir))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "agent_policy.host_volumes")
	// deny leaves non-host volume types alone.
	assert.NoError(t, checkAgentPolicyVolumes(deny, "a", "docker", []api.VolumeMount{{Type: "gcs", Bucket: "b", Target: "/b"}}))

	types := &config.V1AgentPolicy{AllowedVolumeTypes: []string{"local"}}
	assert.NoError(t, checkAgentPolicyVolumes(types, "a", "docker", vol("/anything")))
	for _, typ := range []string{"nfs", "NFS", "gcs"} {
		err := checkAgentPolicyVolumes(types, "a", "docker", []api.VolumeMount{{Type: typ, Source: "/export", Server: "h", Target: "/n"}})
		require.Error(t, err, typ)
		assert.Contains(t, err.Error(), "agent_policy.allowed_volume_types")
	}

	// allow (the default) with no type list judges nothing.
	assert.NoError(t, checkAgentPolicyVolumes(&config.V1AgentPolicy{}, "a", "docker", vol("/var/run/docker.sock")))
	assert.NoError(t, checkAgentPolicyVolumes(nil, "a", "docker", vol("/var/run/docker.sock")))
}

// The bind runtimes bind every volume not of type exactly "gcs" as a host
// path, so a nfs, bind, cloudrun-volume or upper-case GCS volume is judged
// as a host volume there.
func TestCheckAgentPolicyVolumes_TypeSpellings(t *testing.T) {
	deny := &config.V1AgentPolicy{HostVolumes: "deny"}
	for _, typ := range []string{"", "local", "nfs", "NFS", "GCS", " gcs", "bind", "cloudrun-volume", "gke-shared-volume"} {
		v := []api.VolumeMount{{Type: typ, Source: "/etc", Server: "s", Bucket: "b", Target: "/x"}}
		err := checkAgentPolicyVolumes(deny, "a", "docker", v)
		require.Error(t, err, "docker type %q", typ)
		assert.Contains(t, err.Error(), "agent_policy.host_volumes")
	}
	assert.NoError(t, checkAgentPolicyVolumes(deny, "a", "docker", []api.VolumeMount{{Type: "gcs", Bucket: "b", Target: "/b"}}))
	// Elsewhere only the non-host types are exempt, by exact spelling.
	for _, typ := range []string{"gcs", "cloudrun-volume", "gke-shared-volume"} {
		assert.NoError(t, checkAgentPolicyVolumes(deny, "a", "kubernetes", []api.VolumeMount{{Type: typ, VolumeName: "v", Bucket: "b", Target: "/b"}}), typ)
	}
	for _, typ := range []string{"", "nfs", "GCS"} {
		assert.Error(t, checkAgentPolicyVolumes(deny, "a", "cloudrun", []api.VolumeMount{{Type: typ, Source: "/etc", Target: "/x"}}), typ)
	}
	// allowed_volume_types compares the type exactly as the runtimes do.
	types := &config.V1AgentPolicy{AllowedVolumeTypes: []string{"local", "gcs"}}
	err := checkAgentPolicyVolumes(types, "a", "docker", []api.VolumeMount{{Type: "GCS", Bucket: "b", Source: "/etc", Target: "/x"}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "agent_policy.allowed_volume_types")
}

func TestCheckAgentPolicyWorkspace(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "repo")
	projectDir := filepath.Join(project, ".scion")
	allowedSrc := filepath.Join(root, "data")
	for _, d := range []string{projectDir, filepath.Join(project, "sub"), allowedSrc, filepath.Join(root, "outside")} {
		require.NoError(t, os.MkdirAll(d, 0o755))
	}
	ws := agentPolicyWorkspace{ScionRoots: []string{projectDir, project}}
	deny := &config.V1AgentPolicy{HostVolumes: "deny"}
	allowlist := &config.V1AgentPolicy{HostVolumes: "allowlist", AllowedHostVolumeSources: []string{allowedSrc}}

	judge := func(p *config.V1AgentPolicy, requested, mounted, repoRoot string, own bool) error {
		w := ws
		w.Requested = requested
		return checkAgentPolicyWorkspace(p, "a", &runtime.RunConfig{Workspace: mounted, RepoRoot: repoRoot, AgentOwnWorkspace: own}, w)
	}
	outside := filepath.Join(root, "outside")
	// A requested workspace outside the project is refused under deny and
	// allowlist, and allowed under an allowed source with allowlist.
	err := judge(deny, outside, outside, "", false)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrAgentPolicy)
	assert.Contains(t, err.Error(), "agent_policy.host_volumes")
	err = judge(allowlist, outside, outside, "", false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "agent_policy.allowed_host_volume_sources")
	assert.NoError(t, judge(allowlist, allowedSrc, allowedSrc, "", false))
	assert.Error(t, judge(deny, "/etc", "/etc", "", false))
	// Scion's own workspaces are never refused.
	assert.NoError(t, judge(deny, filepath.Join(project, "sub"), filepath.Join(project, "sub"), project, false))
	assert.NoError(t, judge(deny, outside, outside, "", true), "the agent's own workspace")
	assert.NoError(t, judge(deny, "", outside, "", false), "a workspace Scion derived")
	assert.NoError(t, judge(deny, outside, filepath.Join(root, "nfs-mount"), "", false), "replaced by a storage backend")
	// The repository root mounted with a requested workspace is judged too.
	err = judge(deny, filepath.Join(project, "sub"), filepath.Join(project, "sub"), outside, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "repository root")
	// allow judges nothing.
	assert.NoError(t, judge(&config.V1AgentPolicy{}, "/etc", "/etc", "", false))
}

func TestAgentPolicyScionRoots(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	global := filepath.Join(home, ".scion")
	roots := agentPolicyScionRoots(filepath.Join(home, "repo", ".scion"))
	assert.Contains(t, roots, filepath.Join(home, "repo"))
	assert.Contains(t, roots, filepath.Join(home, "repo", ".scion"))
	// The global project's parent (the home directory) is never a root.
	roots = agentPolicyScionRoots(global)
	assert.NotContains(t, roots, home)
}

func TestApplyAgentPolicyResources(t *testing.T) {
	p := &config.V1AgentPolicy{MaxResources: &config.V1AgentPolicyResources{CPU: "8", Memory: "8Gi"}}

	// Missing limits become the maximum; the input is not modified.
	in := &api.ResourceSpec{Requests: api.ResourceList{CPU: "1"}}
	out, err := applyAgentPolicyResources(p, "a", "docker", in, nil)
	require.NoError(t, err)
	assert.Equal(t, "8", out.Limits.CPU)
	assert.Equal(t, "8Gi", out.Limits.Memory)
	assert.Equal(t, "1", out.Requests.CPU)
	assert.Equal(t, "", out.Requests.Memory, "docker: no request is added")
	assert.Equal(t, "", in.Limits.CPU, "the input spec must not be modified")

	out, err = applyAgentPolicyResources(p, "a", "docker", nil, nil)
	require.NoError(t, err)
	assert.Equal(t, api.ResourceList{CPU: "8", Memory: "8Gi"}, out.Limits)

	// On Kubernetes a filled limit gets the default request, so the API
	// server does not default the request to the filled limit.
	out, err = applyAgentPolicyResources(p, "a", "kubernetes", nil, nil)
	require.NoError(t, err)
	assert.Equal(t, api.ResourceList{CPU: "250m", Memory: "512Mi"}, out.Requests)
	small := &config.V1AgentPolicy{MaxResources: &config.V1AgentPolicyResources{CPU: "100m"}}
	out, err = applyAgentPolicyResources(small, "a", "kubernetes", nil, nil)
	require.NoError(t, err)
	assert.Equal(t, "100m", out.Requests.CPU, "the filled request never exceeds the maximum")

	// Values within the maximum pass unchanged.
	ok := &api.ResourceSpec{Limits: api.ResourceList{CPU: "8000m", Memory: "8192Mi"}, Requests: api.ResourceList{CPU: "2", Memory: "1Gi"}}
	out, err = applyAgentPolicyResources(p, "a", "docker", ok, nil)
	require.NoError(t, err)
	assert.Equal(t, *ok, *out)

	for name, spec := range map[string]*api.ResourceSpec{
		"limits.cpu":      {Limits: api.ResourceList{CPU: "12"}},
		"limits.memory":   {Limits: api.ResourceList{Memory: "64Gi"}},
		"requests.cpu":    {Requests: api.ResourceList{CPU: "9"}},
		"requests.memory": {Requests: api.ResourceList{Memory: "9Gi"}},
		"docker spelling": {Limits: api.ResourceList{Memory: "64g"}},
		"ambiguous m":     {Limits: api.ResourceList{Memory: "9000m"}}, // 9 GB to docker
		"unparsable":      {Limits: api.ResourceList{CPU: "lots"}},
		// Docker reads a 0 limit as unlimited.
		"zero cpu limit":        {Limits: api.ResourceList{CPU: "0"}},
		"zero memory limit":     {Limits: api.ResourceList{Memory: "0"}},
		"negative cpu limit":    {Limits: api.ResourceList{CPU: "-1"}},
		"negative memory limit": {Limits: api.ResourceList{Memory: "-8Gi"}},
		"sub-byte memory limit": {Limits: api.ResourceList{Memory: "0.1"}},
		"negative request":      {Requests: api.ResourceList{CPU: "-1"}},
	} {
		_, err := applyAgentPolicyResources(p, "a", "docker", spec, nil)
		require.Error(t, err, name)
		assert.ErrorIs(t, err, ErrAgentPolicy, name)
		assert.Contains(t, err.Error(), "agent_policy.max_resources", name)
	}

	// The Kubernetes resources map wins over the spec on Kubernetes, so it
	// is checked too, and its limit counts as a limit.
	k8s := &api.KubernetesConfig{Resources: &api.K8sResources{Limits: map[string]string{"cpu": "16"}}}
	_, err = applyAgentPolicyResources(p, "a", "kubernetes", nil, k8s)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "kubernetes.resources.limits.cpu")
	k8s = &api.KubernetesConfig{Resources: &api.K8sResources{Limits: map[string]string{"cpu": "4"}}}
	out, err = applyAgentPolicyResources(p, "a", "kubernetes", nil, k8s)
	require.NoError(t, err)
	assert.Equal(t, "", out.Limits.CPU, "a kubernetes limit within the maximum is kept as the limit")
	assert.Equal(t, "8Gi", out.Limits.Memory)

	// No max_resources: the spec is returned as is.
	out, err = applyAgentPolicyResources(&config.V1AgentPolicy{}, "a", "docker", in, nil)
	require.NoError(t, err)
	assert.Same(t, in, out)
}

func TestApplyAgentPolicyTelemetry(t *testing.T) {
	p := &config.V1AgentPolicy{RequireTelemetry: true}
	cfg := &runtime.RunConfig{
		TelemetryEnabled: false,
		Env:              []string{"A=1", "SCION_TELEMETRY_ENABLED=false", "SCION_TELEMETRY_ENABLED", "SCION_TELEMETRY_CLOUD_ENABLED=false"},
		FallbackEnv:      map[string]string{"SCION_TELEMETRY_ENABLED": "false", "B": "2"},
	}
	fallback := cfg.FallbackEnv
	applyAgentPolicyTelemetry(p, "a", cfg)
	assert.True(t, cfg.TelemetryEnabled)
	assert.Equal(t, []string{"A=1", "SCION_TELEMETRY_ENABLED=true", "SCION_TELEMETRY_CLOUD_ENABLED=true"}, cfg.Env)
	assert.Equal(t, map[string]string{"B": "2"}, cfg.FallbackEnv)
	assert.Equal(t, "false", fallback["SCION_TELEMETRY_ENABLED"], "the caller's map must not be modified")
	assert.Contains(t, cfg.SuppressedFallbackKeys, "SCION_TELEMETRY_ENABLED")
	assert.Contains(t, cfg.SuppressedFallbackKeys, "SCION_TELEMETRY_CLOUD_ENABLED")

	// Not required: nothing changes.
	cfg = &runtime.RunConfig{Env: []string{"SCION_TELEMETRY_ENABLED=false"}}
	applyAgentPolicyTelemetry(&config.V1AgentPolicy{}, "a", cfg)
	assert.False(t, cfg.TelemetryEnabled)
	assert.Equal(t, []string{"SCION_TELEMETRY_ENABLED=false"}, cfg.Env)
}

func TestApplyAgentPolicyLimits(t *testing.T) {
	p := &config.V1AgentPolicy{MaxLimits: &config.V1AgentPolicyLimits{MaxTurns: 100, MaxModelCalls: 300, MaxDuration: "8h"}}
	cases := []struct {
		name                   string
		env                    []string
		turns, calls, duration string
	}{
		{"missing becomes the ceiling", nil, "100", "300", "8h"},
		{"unlimited becomes the ceiling", []string{"SCION_MAX_TURNS=0", "SCION_MAX_MODEL_CALLS=0", "SCION_MAX_DURATION=0"}, "100", "300", "8h"},
		{"larger is lowered", []string{"SCION_MAX_TURNS=1000", "SCION_MAX_MODEL_CALLS=301", "SCION_MAX_DURATION=24h"}, "100", "300", "8h"},
		{"smaller is kept", []string{"SCION_MAX_TURNS=10", "SCION_MAX_MODEL_CALLS=20", "SCION_MAX_DURATION=30m"}, "10", "20", "30m"},
		{"garbage becomes the ceiling", []string{"SCION_MAX_TURNS=lots", "SCION_MAX_MODEL_CALLS=-5", "SCION_MAX_DURATION=forever"}, "100", "300", "8h"},
		{"the last duplicate is judged and all are replaced", []string{"SCION_MAX_TURNS=10", "SCION_MAX_TURNS=0"}, "100", "300", "8h"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &runtime.RunConfig{Env: tc.env, FallbackEnv: map[string]string{"SCION_MAX_TURNS": "0"}}
			applyAgentPolicyLimits(p, "a", cfg)
			env := envListToMap(cfg.Env)
			assert.Equal(t, tc.turns, env["SCION_MAX_TURNS"])
			assert.Equal(t, tc.calls, env["SCION_MAX_MODEL_CALLS"])
			assert.Equal(t, tc.duration, env["SCION_MAX_DURATION"])
			n := 0
			for _, e := range cfg.Env {
				if strings.HasPrefix(e, "SCION_MAX_TURNS") {
					n++
				}
			}
			assert.Equal(t, 1, n, "exactly one SCION_MAX_TURNS entry")
			assert.NotContains(t, cfg.FallbackEnv, "SCION_MAX_TURNS")
			assert.Contains(t, cfg.SuppressedFallbackKeys, "SCION_MAX_DURATION")
		})
	}
	// A ceiling of 0 sets none.
	cfg := &runtime.RunConfig{Env: []string{"SCION_MAX_TURNS=0"}}
	applyAgentPolicyLimits(&config.V1AgentPolicy{MaxLimits: &config.V1AgentPolicyLimits{MaxModelCalls: 5}}, "a", cfg)
	assert.Equal(t, "0", envListToMap(cfg.Env)["SCION_MAX_TURNS"])
	assert.Equal(t, "5", envListToMap(cfg.Env)["SCION_MAX_MODEL_CALLS"])
}

// An agent policy refusal reaches the Runtime Broker as the same typed
// error as a refused resource spec, which every start path answers with a
// 400 and the public message.
func TestAgentPolicyError_IsPublicResourceRefusal(t *testing.T) {
	err := error(NewAgentPolicyError("image", "agent_policy.allowed_image_prefixes", "image %q is not allowed", "x"))
	var re *runtime.ResourceLimitsError
	require.True(t, errors.As(err, &re))
	assert.Equal(t, err.Error(), re.PublicMessage())
	assert.ErrorIs(t, err, ErrAgentPolicy)
	var pe *AgentPolicyError
	require.True(t, errors.As(err, &pe))
	assert.Equal(t, "image", pe.Field)
}

// --- Start integration: the policy is applied to the final RunConfig. ---

const agentPolicyRunSettings = `schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
agent_policy:
  allowed_image_prefixes: [test-image]
  host_volumes: deny
  max_resources:
    cpu: "8"
    memory: 8Gi
  require_telemetry: true
  max_limits:
    max_turns: 50
    max_model_calls: 100
    max_duration: 2h
`

func startWithAgentPolicy(t *testing.T, settings string, inline *api.ScionConfig) (runtime.RunConfig, int, error) {
	t.Helper()
	return startWithAgentPolicyOpts(t, settings, inline, nil)
}

// startWithAgentPolicyOpts is startWithAgentPolicy with a hook that may
// change the start options (given the fixture's temp dir).
func startWithAgentPolicyOpts(t *testing.T, settings string, inline *api.ScionConfig, mutate func(tmpDir string, o *api.StartOptions)) (runtime.RunConfig, int, error) {
	t.Helper()
	f := newSharedDirStorageRunFixture(t)
	require.NoError(t, os.WriteFile(filepath.Join(f.globalScionDir, "settings.yaml"), []byte(settings), 0o644))
	var captured runtime.RunConfig
	ran, pulled := 0, []string{}
	mockRT := &runtime.MockRuntime{
		NameFunc: func() string { return "docker" },
		RunFunc: func(ctx context.Context, cfg runtime.RunConfig) (string, error) {
			captured = cfg
			ran++
			return "mock-id", nil
		},
		ImageExistsFunc: func(ctx context.Context, image string) (bool, error) { return false, nil },
		PullImageFunc: func(ctx context.Context, image string) error {
			pulled = append(pulled, image)
			return nil
		},
	}
	opts := api.StartOptions{
		Name:         "policy-agent",
		ProjectPath:  f.projectScionDir,
		NoAuth:       true,
		InlineConfig: inline,
	}
	if mutate != nil {
		mutate(f.tmpDir, &opts)
	}
	_, err := NewManager(mockRT).Start(context.Background(), opts)
	if errors.Is(err, ErrAgentPolicy) && strings.Contains(err.Error(), "allowed_image_prefixes") {
		assert.Empty(t, pulled, "a refused image must not be pulled")
	}
	return captured, ran, err
}

func TestStart_AgentPolicyAppliedToFinalRunConfig(t *testing.T) {
	disabled := false
	cfg, ran, err := startWithAgentPolicy(t, agentPolicyRunSettings, &api.ScionConfig{
		Telemetry:   &api.TelemetryConfig{Enabled: &disabled},
		MaxTurns:    api.IntPtr(0),
		MaxDuration: "0",
		Env:         map[string]string{"SCION_TELEMETRY_ENABLED": "false"},
	})
	require.NoError(t, err)
	require.Equal(t, 1, ran)
	assert.Equal(t, "docker.io/library/test-image:latest", cfg.Image)
	require.NotNil(t, cfg.Resources)
	// The built-in default limits.cpu (2) is within the maximum and kept;
	// the missing memory limit becomes the maximum.
	assert.Equal(t, "2", cfg.Resources.Limits.CPU)
	assert.Equal(t, "8Gi", cfg.Resources.Limits.Memory)
	assert.True(t, cfg.TelemetryEnabled)
	env := envListToMap(cfg.Env)
	assert.Equal(t, "true", env["SCION_TELEMETRY_ENABLED"])
	assert.Equal(t, "50", env["SCION_MAX_TURNS"])
	assert.Equal(t, "100", env["SCION_MAX_MODEL_CALLS"])
	assert.Equal(t, "2h", env["SCION_MAX_DURATION"])
}

func TestStart_AgentPolicyRefusals(t *testing.T) {
	cases := map[string]struct {
		inline  *api.ScionConfig
		setting string
	}{
		"inline image": {&api.ScionConfig{Image: "docker.io/library/postgres:18-alpine"}, "agent_policy.allowed_image_prefixes"},
		"host bind mount": {&api.ScionConfig{Volumes: []api.VolumeMount{{Source: "/var/run/docker.sock", Target: "/x"}}},
			"agent_policy.host_volumes"},
		"resources above max": {&api.ScionConfig{Resources: &api.ResourceSpec{Limits: api.ResourceList{CPU: "12", Memory: "64Gi"}}},
			"agent_policy.max_resources.cpu"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, ran, err := startWithAgentPolicy(t, agentPolicyRunSettings, tc.inline)
			require.Error(t, err)
			assert.Equal(t, 0, ran, "the container must not be created")
			assert.ErrorIs(t, err, ErrAgentPolicy)
			assert.Contains(t, err.Error(), tc.setting)
			var re *runtime.ResourceLimitsError
			assert.True(t, errors.As(err, &re), "the broker must see a public refusal")
		})
	}
}

func TestStart_NoAgentPolicyUnchanged(t *testing.T) {
	cfg, ran, err := startWithAgentPolicy(t, `schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
`, &api.ScionConfig{Image: "postgres:18-alpine", Volumes: []api.VolumeMount{{Source: "/var/run/docker.sock", Target: "/x"}}})
	require.NoError(t, err)
	require.Equal(t, 1, ran)
	assert.Equal(t, "postgres:18-alpine", cfg.Image)
	require.Len(t, cfg.Volumes, 1)
	assert.Equal(t, "/var/run/docker.sock", cfg.Volumes[0].Source)
}

func TestStart_InvalidAgentPolicyFailsClosed(t *testing.T) {
	_, ran, err := startWithAgentPolicy(t, `schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
agent_policy:
  host_volumes: sometimes
`, nil)
	require.Error(t, err)
	assert.Equal(t, 0, ran)
	assert.ErrorIs(t, err, ErrAgentPolicy)
}

// A requested workspace (config.workspace) is a host mount the requester
// chose: under host_volumes deny it must lie in the project, the agent's
// own directory or the Scion global directory.
func TestStart_AgentPolicyRequestedWorkspace(t *testing.T) {
	outside := func(tmp string, o *api.StartOptions) {
		ws := filepath.Join(tmp, "elsewhere")
		require.NoError(t, os.MkdirAll(ws, 0o755))
		o.Workspace = ws
	}
	base := "schema_version: \"1\"\nactive_profile: local\nprofiles:\n  local:\n    runtime: docker\n"
	deny := base + "agent_policy:\n  host_volumes: deny\n"
	_, ran, err := startWithAgentPolicyOpts(t, deny, nil, outside)
	require.Error(t, err)
	assert.Equal(t, 0, ran)
	assert.ErrorIs(t, err, ErrAgentPolicy)
	assert.Contains(t, err.Error(), "agent_policy.host_volumes")

	// The same start with no policy runs.
	_, ran, err = startWithAgentPolicyOpts(t, base, nil, outside)
	require.NoError(t, err)
	assert.Equal(t, 1, ran)

	// A start without a requested workspace (Scion places it) runs under deny.
	_, ran, err = startWithAgentPolicyOpts(t, deny, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, ran)
}
