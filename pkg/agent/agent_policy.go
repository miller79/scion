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
	"fmt"
	"log/slog"
	"maps"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/brokercontrol"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// The operator's agent policy (config.V1AgentPolicy, the global settings
// file's agent_policy block) is enforced by Start on the final RunConfig,
// immediately before Runtime.Run (enforceAgentPolicy). Every value it judges
// has been fully resolved by then (settings, profile, harness config,
// template, the agent's stored config, inline config and the Hub request
// all merged), and Run is the only place any runtime creates an agent
// container, so no source can get around it. The image is also checked
// before it is pulled (checkAgentPolicyImage).

// ErrAgentPolicy is wrapped by every agent policy refusal.
var ErrAgentPolicy = errors.New("refused by agent policy")

// AgentPolicyError is a start refused by the agent policy. Its message names
// the agent config field and the policy setting, and repeats only the
// requester's own value, never a resolved host path or the policy's allow
// lists, so it is safe to return to the requester.
//
// It wraps ErrAgentPolicy and a runtime.ResourceLimitsError carrying the
// same message: the Runtime Broker answers a ResourceLimitsError on every
// start path (create, start, restart and the asynchronous launch) with a
// 400 validation_error / container_config refusal and its public message,
// instead of a generic 500, and an agent policy refusal is the same kind of
// error: the requested container configuration is not allowed here.
type AgentPolicyError struct {
	// Field is the agent config field refused (e.g. "image",
	// "volumes[0].source", "resources.limits.cpu").
	Field string
	// Setting is the policy setting that refused it (e.g.
	// "agent_policy.allowed_image_prefixes").
	Setting string
	msg     string
}

// NewAgentPolicyError returns a refusal of field by the policy setting. The
// formatted reason must follow PublicMessage's rules.
func NewAgentPolicyError(field, setting, format string, args ...any) *AgentPolicyError {
	return &AgentPolicyError{
		Field:   field,
		Setting: setting,
		msg:     fmt.Sprintf("agent policy: %s: %s (%s)", field, fmt.Sprintf(format, args...), setting),
	}
}

func (e *AgentPolicyError) Error() string { return e.msg }

// PublicMessage returns the text the requester may see.
func (e *AgentPolicyError) PublicMessage() string { return e.msg }

// Unwrap exposes ErrAgentPolicy and the broker-facing ResourceLimitsError.
func (e *AgentPolicyError) Unwrap() []error {
	return []error{ErrAgentPolicy, runtime.NewResourceLimitsError("%s", e.msg)}
}

// loadAgentPolicy loads the agent policy for a start. A variable so tests
// can substitute a policy.
var loadAgentPolicy = config.LoadAgentPolicy

// loadStartAgentPolicy loads the agent policy, failing the start closed
// when it cannot be loaded or is invalid. The detail goes to the log; the
// requester sees a fixed message.
func loadStartAgentPolicy(agentName string) (*config.V1AgentPolicy, error) {
	p, err := loadAgentPolicy()
	if err != nil {
		slog.Warn("Agent policy could not be loaded; refusing the start", "agent", agentName, "error", err)
		return nil, NewAgentPolicyError("agent", "agent_policy", "the host's agent policy could not be loaded; ask the operator to check the agent_policy settings")
	}
	return p, nil
}

// refuseAgentPolicy logs a refusal at WARN and returns it.
func refuseAgentPolicy(agentName string, e *AgentPolicyError) error {
	slog.Warn("Agent policy refused agent start", "agent", agentName, "field", e.Field, "setting", e.Setting)
	return e
}

// normalizeImageReference returns ref fully qualified the way Docker and
// containerd read it: a reference whose first component is not a registry
// host (no "." or ":" and not "localhost") is on docker.io, and a
// single-component docker.io repository is under library/.
func normalizeImageReference(ref string) string {
	ref = strings.TrimSpace(ref)
	first, rest, hasSlash := strings.Cut(ref, "/")
	if hasSlash && (strings.ContainsAny(first, ".:") || first == "localhost") {
		// Registry host names are case-insensitive.
		first = strings.ToLower(first)
		if first == "index.docker.io" {
			first = "docker.io"
		}
		ref = first + "/" + rest
		if first == "docker.io" && !strings.Contains(rest, "/") {
			ref = "docker.io/library/" + rest
		}
		return ref
	}
	if !hasSlash {
		return "docker.io/library/" + ref
	}
	return "docker.io/" + ref
}

// normalizeImagePrefix normalizes an allowed_image_prefixes entry the same
// way as an image reference. A bare registry host ("registry.example.com")
// or a host with a trailing "/" is kept as a host prefix.
func normalizeImagePrefix(prefix string) string {
	prefix = strings.TrimSpace(prefix)
	host, rest, hasSlash := strings.Cut(prefix, "/")
	isHost := strings.ContainsAny(host, ".:") || host == "localhost"
	if isHost && (!hasSlash || rest == "") {
		host = strings.ToLower(host)
		if host == "index.docker.io" {
			host = "docker.io"
		}
		return host + "/"
	}
	if strings.HasSuffix(prefix, "/") {
		// A namespace prefix ("myorg/", "docker.io/myorg/"): only the
		// registry is implied, never library/, which applies to a
		// single-component repository name only.
		if isHost {
			host = strings.ToLower(host)
			if host == "index.docker.io" {
				host = "docker.io"
			}
			return host + "/" + rest
		}
		return "docker.io/" + prefix
	}
	return normalizeImageReference(prefix)
}

// imageMatchesPrefix reports whether the normalized image starts with the
// normalized prefix at a reference boundary: a prefix ending in "/" matches
// anything under it; any other prefix matches the reference itself or one
// continued by "/", ":" or "@" (so registry/path never matches
// registry/path-evil).
func imageMatchesPrefix(image, prefix string) bool {
	if strings.HasSuffix(prefix, "/") {
		return strings.HasPrefix(image, prefix)
	}
	if !strings.HasPrefix(image, prefix) {
		return false
	}
	if len(image) == len(prefix) {
		return true
	}
	switch image[len(prefix)] {
	case '/', ':', '@':
		return true
	}
	return false
}

// checkAgentPolicyImage checks image against allowed_image_prefixes and
// returns the reference to use: the fully qualified form when a prefix list
// is set, so the image checked is the image pulled and run whatever the
// runtime's short-name resolution, else image unchanged.
func checkAgentPolicyImage(p *config.V1AgentPolicy, agentName, image string) (string, error) {
	if p == nil || len(p.AllowedImagePrefixes) == 0 {
		return image, nil
	}
	if strings.TrimSpace(image) == "" {
		return "", refuseAgentPolicy(agentName, NewAgentPolicyError("image", "agent_policy.allowed_image_prefixes", "no image is set"))
	}
	normalized := normalizeImageReference(image)
	for _, prefix := range p.AllowedImagePrefixes {
		if imageMatchesPrefix(normalized, normalizeImagePrefix(prefix)) {
			return normalized, nil
		}
	}
	return "", refuseAgentPolicy(agentName, NewAgentPolicyError("image", "agent_policy.allowed_image_prefixes",
		"image %s is not from an allowed registry or repository", quotePolicyValue(image)))
}

// maxEchoedPolicyValue is how many characters of a requester's value an
// agent policy error repeats.
const maxEchoedPolicyValue = 128

func quotePolicyValue(v string) string {
	if len(v) > maxEchoedPolicyValue {
		v = v[:maxEchoedPolicyValue] + "..."
	}
	return strconv.Quote(v)
}

// policyVolumeType returns a volume's type for allowed_volume_types: the
// type exactly as written ("local" for the empty default). It is not
// normalized, because the runtimes compare it exactly ("GCS" is not a gcs
// volume to them), and allowed_volume_types only names the lower-case types.
func policyVolumeType(v api.VolumeMount) string {
	if v.Type == "" {
		return "local"
	}
	return v.Type
}

// bindRuntimes are the runtimes that bind every configured volume other
// than one of type exactly "gcs" as a host path (buildCommonRunArgs), so a
// "nfs", "cloudrun-volume", "bind" or "GCS" volume is a host bind there.
var bindRuntimes = map[string]bool{"docker": true, "podman": true, "container": true, "apple-container": true}

// nonHostVolumeTypes are the types whose source is not a host path on the
// runtimes that support them.
var nonHostVolumeTypes = map[string]bool{"gcs": true, "cloudrun-volume": true, "gke-shared-volume": true}

// isHostPathVolume reports whether host_volumes judges v: every volume whose
// type is not exactly a non-host type, and on the bind runtimes every
// volume not of type exactly "gcs". An nfs volume names a server's export,
// but the bind runtimes bind its source from the host, so it is judged
// everywhere.
func isHostPathVolume(v api.VolumeMount, runtimeName string) bool {
	if bindRuntimes[runtimeName] {
		return v.Type != "gcs"
	}
	return !nonHostVolumeTypes[v.Type]
}

// pathAtOrUnder reports whether the clean absolute path p is base or below
// it, per path element.
func pathAtOrUnder(p, base string) bool {
	if p == base {
		return true
	}
	prefix := base
	if !strings.HasSuffix(prefix, string(filepath.Separator)) {
		prefix += string(filepath.Separator)
	}
	return strings.HasPrefix(p, prefix)
}

// checkAgentPolicyVolumes applies allowed_volume_types and host_volumes to
// the volumes taken from the agent's merged config (configured volumes:
// settings, profiles, harness configs, templates, the agent's own and the
// inline config). Scion's own mounts (workspace, repository root, agent
// home, shared directories, auth files, git config) are not in vols.
func checkAgentPolicyVolumes(p *config.V1AgentPolicy, agentName, runtimeName string, vols []api.VolumeMount) error {
	if p == nil {
		return nil
	}
	mode := p.HostVolumeMode()
	if len(p.AllowedVolumeTypes) == 0 && mode == config.AgentPolicyHostVolumesAllow {
		return nil
	}
	allowed, allowedPhysical := policyAllowedHostPrefixes(p)
	for i, v := range vols {
		typ := policyVolumeType(v)
		if len(p.AllowedVolumeTypes) > 0 && !slices.Contains(p.AllowedVolumeTypes, typ) {
			return refuseAgentPolicy(agentName, NewAgentPolicyError(fmt.Sprintf("volumes[%d].type", i), "agent_policy.allowed_volume_types",
				"volume type %s (target %s) is not allowed", quotePolicyValue(typ), quotePolicyValue(v.Target)))
		}
		if !isHostPathVolume(v, runtimeName) {
			continue
		}
		field := fmt.Sprintf("volumes[%d].source", i)
		switch mode {
		case config.AgentPolicyHostVolumesDeny:
			return refuseAgentPolicy(agentName, NewAgentPolicyError(field, "agent_policy.host_volumes",
				"host volume %s (target %s) is not allowed: host volumes are denied", quotePolicyValue(v.Source), quotePolicyValue(v.Target)))
		case config.AgentPolicyHostVolumesAllowlist:
			if !hostVolumeSourceAllowed(v.Source, allowed, allowedPhysical) {
				return refuseAgentPolicy(agentName, NewAgentPolicyError(field, "agent_policy.allowed_host_volume_sources",
					"host volume %s (target %s) is not under an allowed source", quotePolicyValue(v.Source), quotePolicyValue(v.Target)))
			}
		}
	}
	return nil
}

// policyAllowedHostPrefixes returns allowed_host_volume_sources clean, and
// each with its symlinks resolved (the clean form when it cannot be
// resolved), in matching order; nil unless host_volumes is allowlist.
func policyAllowedHostPrefixes(p *config.V1AgentPolicy) (allowed, allowedPhysical []string) {
	if p.HostVolumeMode() != config.AgentPolicyHostVolumesAllowlist {
		return nil, nil
	}
	for _, a := range p.AllowedHostVolumeSources {
		c, err := config.CleanAgentPolicyHostPrefix(a)
		if err != nil {
			continue // Validate already refused an invalid policy.
		}
		allowed = append(allowed, c)
		if r, err := brokercontrol.ResolvePhysical(c); err == nil {
			allowedPhysical = append(allowedPhysical, r)
		} else {
			allowedPhysical = append(allowedPhysical, c)
		}
	}
	return allowed, allowedPhysical
}

// agentPolicyWorkspace describes the workspace a start mounts, for the
// workspace check (checkAgentPolicyWorkspace).
type agentPolicyWorkspace struct {
	// Requested is the validated, symlink-free workspace when it came from
	// opts.Workspace or the persisted explicit workspace (a requester's
	// config.workspace, or a worktree the broker placed there); empty when
	// Scion derived it from the project. Judged only while RunConfig still
	// mounts exactly this path: a storage backend that replaced it with its
	// own mount computed that path itself.
	Requested string
	// ScionRoots are the broker-known roots a workspace may always lie in:
	// the Scion global directory (whose own control-path floor limits it to
	// agent-data subtrees), the project's directory and the directory that
	// holds it. Never derived from settings a project can write.
	ScionRoots []string
}

// agentPolicyScionRoots returns the broker-known roots a requested
// workspace may lie in (agentPolicyWorkspace.ScionRoots): the Scion global
// directory, the project directory, and, for a project directory named
// .scion that is not the global directory, the directory holding it (the
// project's checkout). Settings are deliberately not consulted: a project's
// settings (even in-repo content) could otherwise widen them.
func agentPolicyScionRoots(projectDir string) []string {
	var roots []string
	globalDir, err := config.GetGlobalDir()
	if err == nil && globalDir != "" {
		roots = append(roots, globalDir)
	}
	if projectDir == "" || !filepath.IsAbs(projectDir) {
		return roots
	}
	projectDir = filepath.Clean(projectDir)
	roots = append(roots, projectDir)
	isGlobal := globalDir != "" && filepath.Clean(globalDir) == projectDir
	if filepath.Base(projectDir) == config.DotScion && !isGlobal && !config.IsGlobalProjectDir(projectDir) {
		roots = append(roots, filepath.Dir(projectDir))
	}
	return roots
}

// checkAgentPolicyWorkspace applies host_volumes to a requested workspace
// and to the repository root mounted with it: under deny or allowlist, a
// workspace that is not the agent's own, not inside a Scion root and (for
// allowlist) not inside an allowed source is refused. Scion's own workspaces
// (the agent's own directory, worktrees and checkouts of the project) lie
// inside the Scion roots and are never refused.
func checkAgentPolicyWorkspace(p *config.V1AgentPolicy, agentName string, cfg *runtime.RunConfig, ws agentPolicyWorkspace) error {
	if p == nil || p.HostVolumeMode() == config.AgentPolicyHostVolumesAllow {
		return nil
	}
	if ws.Requested == "" || cfg.Workspace != ws.Requested || cfg.AgentOwnWorkspace {
		return nil
	}
	allowed, allowedPhysical := policyAllowedHostPrefixes(p)
	var roots, rootsPhysical []string
	for _, r := range ws.ScionRoots {
		if r == "" || !filepath.IsAbs(r) {
			continue
		}
		c := filepath.Clean(r)
		roots = append(roots, c)
		if rp, err := brokercontrol.ResolvePhysical(c); err == nil {
			rootsPhysical = append(rootsPhysical, rp)
		} else {
			rootsPhysical = append(rootsPhysical, c)
		}
	}
	judge := func(field, path string) error {
		if path == "" {
			return nil
		}
		if hostVolumeSourceAllowed(path, roots, rootsPhysical) || hostVolumeSourceAllowed(path, allowed, allowedPhysical) {
			return nil
		}
		setting := "agent_policy.host_volumes"
		if p.HostVolumeMode() == config.AgentPolicyHostVolumesAllowlist {
			setting = "agent_policy.allowed_host_volume_sources"
		}
		return refuseAgentPolicy(agentName, NewAgentPolicyError(field, setting,
			"the requested workspace is outside the project and is not an allowed host source"))
	}
	if err := judge("workspace", cfg.Workspace); err != nil {
		return err
	}
	return judge("workspace (repository root)", cfg.RepoRoot)
}

// hostVolumeSourceAllowed reports whether a configured host volume source,
// expanded as the runtime expands it, is at or under an allowed prefix both
// lexically (".." applied to the spelling, which is what a remote node
// sees) and physically (every symlink on this host resolved, which is what
// a local bind mounts). A named volume, a relative path and a source that
// cannot be resolved are refused.
func hostVolumeSourceAllowed(src string, allowed, allowedPhysical []string) bool {
	expanded := runtime.ExpandHostVolumeSource(src)
	if expanded == "" || !filepath.IsAbs(expanded) {
		return false
	}
	lexical := filepath.Clean(expanded)
	physical, err := brokercontrol.ResolvePhysical(expanded)
	if err != nil {
		return false
	}
	for i := range allowed {
		if pathAtOrUnder(lexical, allowed[i]) && pathAtOrUnder(physical, allowedPhysical[i]) {
			return true
		}
	}
	return false
}

// k8sDefaultRequestFor is the request a Kubernetes container gets for a
// resource with neither a request nor a limit (runtime
// k8sDefaultResourceRequests). When the policy fills a missing limit on
// Kubernetes, the request is filled with this value (at most the maximum),
// so the API server does not default the request to the filled limit.
var k8sDefaultRequestFor = map[string]string{"cpu": "250m", "memory": "512Mi"}

// applyAgentPolicyResources checks the agent's CPU and memory requests and
// limits (the common resource spec and the Kubernetes resources map, which
// overrides it on Kubernetes) against max_resources, and returns the spec to
// run with: a copy with each missing limit set to the maximum. spec is
// never modified.
func applyAgentPolicyResources(p *config.V1AgentPolicy, agentName, runtimeName string, spec *api.ResourceSpec, k8s *api.KubernetesConfig) (*api.ResourceSpec, error) {
	if p == nil || p.MaxResources == nil || (p.MaxResources.CPU == "" && p.MaxResources.Memory == "") {
		return spec, nil
	}
	out := &api.ResourceSpec{}
	if spec != nil {
		*out = *spec
	}
	var k8sRes *api.K8sResources
	if k8s != nil {
		k8sRes = k8s.Resources
	}
	type resourceRule struct {
		name       string
		max        string
		maxOf      func(string) (float64, error)
		valueOf    func(string) (float64, error)
		minValueOf func(string) (float64, error)
		// minLimit is the smallest limit every runtime reads as a limit
		// (a 0 limit is "unlimited" to Docker).
		minLimit float64
		limit    *string
		request  *string
	}
	rules := []resourceRule{
		{"cpu", p.MaxResources.CPU, config.AgentPolicyCPUMax, config.AgentPolicyCPUValue, config.AgentPolicyCPUMinValue, 0.001, &out.Limits.CPU, &out.Requests.CPU},
		{"memory", p.MaxResources.Memory, config.AgentPolicyMemoryMax, config.AgentPolicyMemoryValue, config.AgentPolicyMemoryMinValue, 1, &out.Limits.Memory, &out.Requests.Memory},
	}
	for _, r := range rules {
		if r.max == "" {
			continue
		}
		maxVal, err := r.maxOf(r.max)
		if err != nil {
			// Validate refuses an invalid policy before this runs.
			return nil, refuseAgentPolicy(agentName, NewAgentPolicyError("resources", "agent_policy.max_resources."+r.name, "the policy maximum is invalid"))
		}
		check := func(field, value string) error {
			if value == "" {
				return nil
			}
			v, err := r.valueOf(value)
			if err != nil {
				return refuseAgentPolicy(agentName, NewAgentPolicyError(field, "agent_policy.max_resources."+r.name,
					"%s is not a valid quantity", quotePolicyValue(value)))
			}
			if v > maxVal {
				return refuseAgentPolicy(agentName, NewAgentPolicyError(field, "agent_policy.max_resources."+r.name,
					"%s is above the maximum %s", quotePolicyValue(value), r.max))
			}
			// A zero limit is "unlimited" to Docker (--cpus 0, --memory 0),
			// so every runtime's reading of a limit must be a real limit;
			// a negative value is never meaningful.
			minV, _ := r.minValueOf(value)
			isLimit := strings.Contains(field, "limits.")
			if minV < 0 || (isLimit && !(minV >= r.minLimit)) {
				return refuseAgentPolicy(agentName, NewAgentPolicyError(field, "agent_policy.max_resources."+r.name,
					"%s must be a positive quantity at most %s", quotePolicyValue(value), r.max))
			}
			return nil
		}
		if err := check("resources.limits."+r.name, *r.limit); err != nil {
			return nil, err
		}
		if err := check("resources.requests."+r.name, *r.request); err != nil {
			return nil, err
		}
		var k8sLimit, k8sRequest string
		var hasK8sLimit, hasK8sRequest bool
		if k8sRes != nil {
			k8sLimit, hasK8sLimit = k8sRes.Limits[r.name]
			k8sRequest, hasK8sRequest = k8sRes.Requests[r.name]
		}
		if hasK8sLimit {
			if err := check("kubernetes.resources.limits."+r.name, k8sLimit); err != nil {
				return nil, err
			}
		}
		if hasK8sRequest {
			if err := check("kubernetes.resources.requests."+r.name, k8sRequest); err != nil {
				return nil, err
			}
		}
		// Only the Kubernetes runtime reads the kubernetes.resources map;
		// every other runtime runs with the common spec alone, so there the
		// map is checked above but never stands in for a missing limit.
		if runtimeName != "kubernetes" {
			hasK8sLimit, hasK8sRequest = false, false
		}
		// No limit is not "unlimited": the maximum becomes the limit.
		if *r.limit == "" && (!hasK8sLimit || k8sLimit == "") {
			*r.limit = r.max
			if runtimeName == "kubernetes" && *r.request == "" && (!hasK8sRequest || k8sRequest == "") {
				req := k8sDefaultRequestFor[r.name]
				if v, err := r.valueOf(req); err != nil || v > maxVal {
					req = r.max
				}
				*r.request = req
			}
		}
	}
	return out, nil
}

// Env names the agent policy sets in the agent's environment.
const (
	policyEnvTelemetryEnabled      = "SCION_TELEMETRY_ENABLED"
	policyEnvTelemetryCloudEnabled = "SCION_TELEMETRY_CLOUD_ENABLED"
	policyEnvMaxTurns              = "SCION_MAX_TURNS"
	policyEnvMaxModelCalls         = "SCION_MAX_MODEL_CALLS"
	policyEnvMaxDuration           = "SCION_MAX_DURATION"
)

// envEntryValue returns the value of the last KEY=VALUE entry for key, and
// whether one exists. A bare KEY entry (copied from the host's env by the
// container CLI) counts as present with an unknown value.
func envEntryValue(env []string, key string) (string, bool) {
	val, found := "", false
	for _, e := range env {
		k, v, hasEq := strings.Cut(e, "=")
		if k != key {
			continue
		}
		found = true
		if hasEq {
			val = v
		} else {
			val = ""
		}
	}
	return val, found
}

// setEnvEntry returns env with every entry for key (KEY=VALUE or bare KEY)
// removed and KEY=value appended, so exactly one value reaches the agent
// whatever order a runtime applies duplicates in.
func setEnvEntry(env []string, key, value string) []string {
	out := make([]string, 0, len(env)+1)
	for _, e := range env {
		k, _, _ := strings.Cut(e, "=")
		if k != key {
			out = append(out, e)
		}
	}
	return append(out, key+"="+value)
}

// pinRunEnv sets key=value in cfg.Env and keeps the template fallback layer
// (FallbackEnv, applied last by every runtime but only for names nothing
// else set) from ever setting key.
func pinRunEnv(cfg *runtime.RunConfig, key, value string) {
	cfg.Env = setEnvEntry(cfg.Env, key, value)
	if _, ok := cfg.FallbackEnv[key]; ok {
		cfg.FallbackEnv = maps.Clone(cfg.FallbackEnv)
		delete(cfg.FallbackEnv, key)
	}
	if !slices.Contains(cfg.SuppressedFallbackKeys, key) {
		cfg.SuppressedFallbackKeys = append(slices.Clip(cfg.SuppressedFallbackKeys), key)
	}
}

// policyTelemetryDestinationEnv are the env names that decide where, and
// whether, sciontool exports telemetry off the host, or whether its pipeline
// starts at all (pkg/sciontool/telemetry LoadConfig). Any of them set by the
// requester can switch export off, redirect it, or make the pipeline refuse
// to start (cloud export asked for with no destination, GCP with no
// project, an exporter that cannot be built, a receiver port in use), so
// under require_telemetry they come from the operator alone.
var policyTelemetryDestinationEnv = []string{
	policyEnvTelemetryCloudEnabled,
	"SCION_TELEMETRY_CLOUD_PROVIDER",
	"SCION_OTEL_ENDPOINT",
	"SCION_OTEL_PROTOCOL",
	"SCION_OTEL_HEADERS",
	"SCION_OTEL_INSECURE",
	"SCION_OTEL_SKIP_TLS_VERIFY",
	"SCION_OTEL_CA_FILE",
	"SCION_OTEL_CERT_FILE",
	"SCION_OTEL_KEY_FILE",
	"SCION_GCP_PROJECT_ID",
	"SCION_OTEL_GRPC_PORT",
	"SCION_OTEL_HTTP_PORT",
}

// policyEnvTelemetryGCPCredentials is the telemetry service account key
// path. A key file there switches sciontool to GCP-native export to the
// key's project, so a requester's value is removed under require_telemetry.
// It is removed rather than pinned: the runtimes set it themselves, after
// this point, from the broker's provisioned telemetry credential, and on the
// flat-env runtimes a value already in RunConfig.Env would win over theirs.
const policyEnvTelemetryGCPCredentials = "SCION_OTEL_GCP_CREDENTIALS"

// removeRunEnv drops every entry for key from cfg.Env and keeps the template
// fallback layer from setting it.
func removeRunEnv(cfg *runtime.RunConfig, key string) {
	out := make([]string, 0, len(cfg.Env))
	for _, e := range cfg.Env {
		if k, _, _ := strings.Cut(e, "="); k != key {
			out = append(out, e)
		}
	}
	cfg.Env = out
	if _, ok := cfg.FallbackEnv[key]; ok {
		cfg.FallbackEnv = maps.Clone(cfg.FallbackEnv)
		delete(cfg.FallbackEnv, key)
	}
	if !slices.Contains(cfg.SuppressedFallbackKeys, key) {
		cfg.SuppressedFallbackKeys = append(slices.Clip(cfg.SuppressedFallbackKeys), key)
	}
}

// loadOperatorTelemetrySettings loads the settings require_telemetry takes
// the operator's telemetry from: the global settings file (plus the hub
// settings overlay), never a project's settings file or the SCION_*
// environment. A variable so tests can replace it.
var loadOperatorTelemetrySettings = config.LoadGlobalSettingsWithOverlayFor

// agentPolicyOperatorTelemetryEnv returns the telemetry env of the operator's
// tiers alone: the global settings telemetry with the hub default over it,
// as Start merges them, without the project settings or the agent's own
// config. A global settings file that cannot be loaded contributes nothing
// (the agent then keeps local telemetry unless the hub default sets a
// destination).
func agentPolicyOperatorTelemetryEnv(ctx context.Context, agentName string) map[string]string {
	var op *api.ScionConfig
	if hub := cloneTelemetryConfig(api.HubAgentDefaultsFromContext(ctx).TelemetryDefault()); hub != nil {
		op = &api.ScionConfig{Telemetry: hub}
	}
	gs, _, err := loadOperatorTelemetrySettings(ctx)
	if err != nil {
		slog.Warn("Agent policy could not load the global telemetry settings; using the hub default only",
			"agent", agentName, "setting", "agent_policy.require_telemetry", "error", err)
	} else if gs != nil && gs.Telemetry != nil {
		op = config.MergeScionConfig(&api.ScionConfig{Telemetry: config.ConvertV1TelemetryToAPI(gs.Telemetry)}, op)
	}
	if op == nil {
		return nil
	}
	return config.TelemetryConfigToEnv(op.Telemetry)
}

// applyAgentPolicyTelemetry forces telemetry on: the runtime's harness
// telemetry env (RunConfig.TelemetryEnabled) and sciontool's own switch.
//
// The export destination is left to the operator: every
// policyTelemetryDestinationEnv name is pinned to its value in operatorEnv
// (agentPolicyOperatorTelemetryEnv: the global settings telemetry and the
// hub default, never a project's settings or the agent's own config or env),
// or to empty, which sciontool reads as unset, when the operator sets none;
// the pin also keeps an env-type secret from setting it. A requested
// SCION_OTEL_GCP_CREDENTIALS is removed (policyEnvTelemetryGCPCredentials).
// SCION_TELEMETRY_CLOUD_ENABLED is never pinned to true, even when the
// operator config says so: sciontool refuses to start the pipeline when
// cloud export is asked for explicitly with no destination, while left unset
// it exports when a destination is configured and runs locally otherwise.
// An operator's explicit false is kept.
func applyAgentPolicyTelemetry(p *config.V1AgentPolicy, agentName string, cfg *runtime.RunConfig, operatorEnv map[string]string) {
	if p == nil || !p.RequireTelemetry {
		return
	}
	enabled, _ := envEntryValue(cfg.Env, policyEnvTelemetryEnabled)
	if !cfg.TelemetryEnabled || enabled != "true" {
		slog.Warn("Agent policy forced telemetry on", "agent", agentName, "setting", "agent_policy.require_telemetry")
	}
	cfg.TelemetryEnabled = true
	pinRunEnv(cfg, policyEnvTelemetryEnabled, "true")
	for _, key := range policyTelemetryDestinationEnv {
		value := operatorEnv[key]
		// The operator's own value already in the env (merged from the
		// operator tiers) is not a request: no warning for it.
		cur, set := envEntryValue(cfg.Env, key)
		requested := set && cur != value
		if key == policyEnvTelemetryCloudEnabled && value != "false" {
			value = ""
		}
		if requested {
			slog.Warn("Agent policy replaced a requested telemetry destination setting with the operator's",
				"agent", agentName, "setting", "agent_policy.require_telemetry", "env", key)
		}
		pinRunEnv(cfg, key, value)
	}
	if _, set := envEntryValue(cfg.Env, policyEnvTelemetryGCPCredentials); set {
		slog.Warn("Agent policy removed a requested telemetry credential setting",
			"agent", agentName, "setting", "agent_policy.require_telemetry", "env", policyEnvTelemetryGCPCredentials)
	}
	removeRunEnv(cfg, policyEnvTelemetryGCPCredentials)
}

// applyAgentPolicyLimits lowers the agent's run limits to max_limits: a
// missing, "no limit" (0 or a value sciontool cannot parse) or larger value
// becomes the ceiling. The limits reach the agent only as SCION_MAX_* env
// (sciontool init and its hooks), so the final env is what is clamped.
func applyAgentPolicyLimits(p *config.V1AgentPolicy, agentName string, cfg *runtime.RunConfig) {
	if p == nil || p.MaxLimits == nil {
		return
	}
	clampCount := func(key, setting string, ceiling int) {
		if ceiling <= 0 {
			return
		}
		cur, _ := envEntryValue(cfg.Env, key)
		n, err := strconv.Atoi(strings.TrimSpace(cur))
		if err == nil && n > 0 && n <= ceiling {
			pinRunEnv(cfg, key, strconv.Itoa(n))
			return
		}
		slog.Warn("Agent policy lowered an agent limit to its ceiling", "agent", agentName, "setting", setting, "ceiling", ceiling)
		pinRunEnv(cfg, key, strconv.Itoa(ceiling))
	}
	clampCount(policyEnvMaxTurns, "agent_policy.max_limits.max_turns", p.MaxLimits.MaxTurns)
	clampCount(policyEnvMaxModelCalls, "agent_policy.max_limits.max_model_calls", p.MaxLimits.MaxModelCalls)
	if p.MaxLimits.MaxDuration != "" {
		ceiling := api.ParseDuration(p.MaxLimits.MaxDuration)
		if ceiling > 0 {
			cur, _ := envEntryValue(cfg.Env, policyEnvMaxDuration)
			if d := api.ParseDuration(strings.TrimSpace(cur)); d > 0 && d <= ceiling {
				pinRunEnv(cfg, policyEnvMaxDuration, strings.TrimSpace(cur))
			} else {
				slog.Warn("Agent policy lowered an agent limit to its ceiling", "agent", agentName,
					"setting", "agent_policy.max_limits.max_duration", "ceiling", p.MaxLimits.MaxDuration)
				pinRunEnv(cfg, policyEnvMaxDuration, p.MaxLimits.MaxDuration)
			}
		}
	}
}

// enforceAgentPolicy applies the agent policy to the final RunConfig, just
// before Runtime.Run. configVolumes are the volumes cfg.Volumes took from the
// agent's merged config (Scion's own shared-directory volumes are appended
// after them and are not judged). Refusals come first, so a refused start
// changes nothing; then the image is pinned to its fully qualified form,
// missing resource limits are filled, telemetry is forced on (with the
// export destination from operatorTelemetryEnv, see
// applyAgentPolicyTelemetry) and run limits are lowered to their ceilings.
func enforceAgentPolicy(p *config.V1AgentPolicy, agentName, runtimeName string, cfg *runtime.RunConfig, configVolumes []api.VolumeMount, ws agentPolicyWorkspace, operatorTelemetryEnv map[string]string) error {
	if p == nil {
		return nil
	}
	image, err := checkAgentPolicyImage(p, agentName, cfg.Image)
	if err != nil {
		return err
	}
	if err := checkAgentPolicyVolumes(p, agentName, runtimeName, configVolumes); err != nil {
		return err
	}
	if err := checkAgentPolicyWorkspace(p, agentName, cfg, ws); err != nil {
		return err
	}
	resources, err := applyAgentPolicyResources(p, agentName, runtimeName, cfg.Resources, cfg.Kubernetes)
	if err != nil {
		return err
	}
	cfg.Image = image
	cfg.Resources = resources
	applyAgentPolicyTelemetry(p, agentName, cfg, operatorTelemetryEnv)
	applyAgentPolicyLimits(p, agentName, cfg)
	return nil
}
