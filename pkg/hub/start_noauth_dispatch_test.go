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

//go:build !no_sqlite

package hub

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDispatch_StartAndRestartCarryNoAuth: an agent whose applied config
// records no-auth (role=none, or the auto no-auth fallback) is started and
// restarted with NoAuth set, as create sends it; without it the broker runs
// the harness's normal command with no credentials.
func TestDispatch_StartAndRestartCarryNoAuth(t *testing.T) {
	ctx := context.Background()
	d, client, ag := autoExposeDispatchFixture(t)
	ag.AppliedConfig = &store.AgentAppliedConfig{NoAuth: true, AgentRole: string(AgentRoleNone)}

	require.NoError(t, d.DispatchAgentStart(ctx, ag, "", false))
	assert.True(t, client.lastStartExtras.NoAuth, "start must carry the agent's no-auth intent")

	require.NoError(t, d.DispatchAgentRestart(ctx, ag))
	assert.True(t, client.lastRestartExtras.NoAuth, "restart must carry the agent's no-auth intent")
}

// TestDispatch_StartAndRestartOmitNoAuthByDefault: an agent without the
// no-auth intent, or without an applied config, sends NoAuth false.
func TestDispatch_StartAndRestartOmitNoAuthByDefault(t *testing.T) {
	ctx := context.Background()
	d, client, ag := autoExposeDispatchFixture(t)

	require.NoError(t, d.DispatchAgentStart(ctx, ag, "", false))
	assert.False(t, client.lastStartExtras.NoAuth, "no applied config: start sends no NoAuth")

	ag.AppliedConfig = &store.AgentAppliedConfig{AgentRole: string(AgentRoleFull)}
	require.NoError(t, d.DispatchAgentRestart(ctx, ag))
	assert.False(t, client.lastRestartExtras.NoAuth, "NoAuth unset: restart sends no NoAuth")
}

// TestApplyStartExtras_NoAuth: NoAuth is written under the wire key the
// broker reads (noAuth), and only when set.
func TestApplyStartExtras_NoAuth(t *testing.T) {
	payload := map[string]interface{}{}
	applyStartExtras(payload, StartExtras{})
	_, ok := payload["noAuth"]
	assert.False(t, ok, "no noAuth key when NoAuth is false")

	applyStartExtras(payload, StartExtras{NoAuth: true})
	blob, err := json.Marshal(payload)
	require.NoError(t, err)
	assert.JSONEq(t, `{"noAuth":true}`, string(blob))
}
