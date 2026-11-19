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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/telemetry"
	"github.com/stretchr/testify/require"
)

// startSciontoolPipeline starts sciontool's telemetry pipeline under env, the
// way sciontool init does in the agent container, and stops it again.
func startSciontoolPipeline(t *testing.T, env []string) error {
	t.Helper()
	t.Setenv("HOME", t.TempDir()) // no well-known GCP credentials file
	// Nothing from the test process's own environment.
	for _, k := range []string{
		telemetry.EnvEnabled, telemetry.EnvCloudEnabled, telemetry.EnvCloudProvider,
		telemetry.EnvEndpoint, telemetry.EnvProtocol, telemetry.EnvProjectID,
		telemetry.EnvGCPCredentials, telemetry.EnvInsecure, telemetry.EnvSkipTLSVerify,
		telemetry.EnvCAFile, telemetry.EnvCertFile, telemetry.EnvKeyFile,
	} {
		t.Setenv(k, "")
	}
	for k, v := range envListToMap(env) {
		t.Setenv(k, v)
	}
	t.Setenv(telemetry.EnvGRPCPort, "0")
	t.Setenv(telemetry.EnvHTTPPort, "0")
	t.Setenv("SCION_USAGE_SOURCE", "")
	// LoadConfig turns cloud export off under go test unless the test has
	// sandboxed the environment, as done above: this decides from env.
	t.Cleanup(telemetry.SetTelemetryTestSandboxed())
	p := telemetry.New()
	require.NotNil(t, p, "telemetry must be enabled")
	err := p.Start(context.Background())
	stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = p.Stop(stopCtx)
	return err
}

// require_telemetry on a host with no cloud destination (local-only
// telemetry, the default) must leave sciontool's pipeline able to start:
// pinning cloud export on with nowhere to send it makes the pipeline refuse
// to start, so the agent would run with no telemetry at all.
func TestAgentPolicyTelemetry_SciontoolPipelineStartsWithoutDestination(t *testing.T) {
	p := &config.V1AgentPolicy{RequireTelemetry: true}
	for name, env := range map[string][]string{
		"nothing requested":             nil,
		"requester turned cloud off":    {"SCION_TELEMETRY_ENABLED=false", "SCION_TELEMETRY_CLOUD_ENABLED=false"},
		"requester turned cloud on":     {"SCION_TELEMETRY_CLOUD_ENABLED=true"},
		"requester gcp without project": {"SCION_TELEMETRY_CLOUD_PROVIDER=gcp"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := &runtime.RunConfig{Env: env}
			applyAgentPolicyTelemetry(p, "a", cfg, nil)
			require.NoError(t, startSciontoolPipeline(t, cfg.Env))
		})
	}
}
