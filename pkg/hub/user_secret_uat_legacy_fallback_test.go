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
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// User access token legacy fallback for an owner's user secrets
// (uatLegacySecretUseFallback).

// setUATLegacyCutoff moves the cutoff for one test.
func setUATLegacyCutoff(t *testing.T, at time.Time) {
	t.Helper()
	prev := uatLegacySecretUseCutoff
	uatLegacySecretUseCutoff = at
	t.Cleanup(func() { uatLegacySecretUseCutoff = prev })
}

// TestUATLegacySecretUseFallback_Predicate exercises the predicate in
// isolation: every condition, one at a time.
func TestUATLegacySecretUseFallback_Predicate(t *testing.T) {
	owner := uuid.NewString()
	agent := &store.Agent{ID: uuid.NewString(), OwnerID: owner, Ancestry: []string{owner}}
	setUATLegacyCutoff(t, time.Date(2026, time.November, 1, 0, 0, 0, 0, time.UTC))
	before := time.Date(2026, time.October, 15, 0, 0, 0, 0, time.UTC)

	uatEdge := func() *store.DelegationEdge {
		return &store.DelegationEdge{
			ID: uuid.NewString(), DelegatorType: store.DelegationPrincipalUser, DelegatorID: owner,
			DelegateType: store.DelegationPrincipalAgent, DelegateID: agent.ID, Role: string(AgentRoleBaseline), Active: true,
			CreatedAt: before,
			AuthorityProvenance: store.AuthorityProvenance{
				ProvenanceVersion: store.ProvenanceVersionV1, SourcePrincipalKind: store.DelegationPrincipalUser,
				SourcePrincipalID: owner, SourceCredentialKind: store.SourceCredentialUAT,
			},
			EffectCeiling: store.EffectCeiling{Kind: store.EffectCeilingBounded, PermissionIDs: []string{"agent.read"}},
		}
	}

	cases := []struct {
		name   string
		mutate func(*store.DelegationEdge)
		depth  int
		want   bool
	}{
		{"owner's token edge before the cutoff", nil, 0, true},
		{"full role", func(e *store.DelegationEdge) { e.Role = string(AgentRoleFull) }, 0, true},
		{"not the agent's own edge (depth 1)", nil, 1, false},
		{"agent delegator", func(e *store.DelegationEdge) {
			e.DelegatorType, e.DelegatorID = store.DelegationPrincipalAgent, uuid.NewString()
		}, 0, false},
		{"delegator is not the owner", func(e *store.DelegationEdge) { e.DelegatorID = uuid.NewString() }, 0, false},
		{"source principal is another user", func(e *store.DelegationEdge) { e.SourcePrincipalID = uuid.NewString() }, 0, false},
		{"session source", func(e *store.DelegationEdge) { e.SourceCredentialKind = store.SourceCredentialSession }, 0, false},
		{"adoption source", func(e *store.DelegationEdge) { e.SourceCredentialKind = store.SourceCredentialSystemMigration }, 0, false},
		{"principal ceiling", func(e *store.DelegationEdge) { e.Kind = store.EffectCeilingPrincipal }, 0, false},
		{"created at the cutoff", func(e *store.DelegationEdge) { e.CreatedAt = uatLegacySecretUseCutoff }, 0, false},
		{"created after the cutoff", func(e *store.DelegationEdge) { e.CreatedAt = uatLegacySecretUseCutoff.Add(time.Hour) }, 0, false},
		{"no creation time", func(e *store.DelegationEdge) { e.CreatedAt = time.Time{} }, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := uatEdge()
			if tc.mutate != nil {
				tc.mutate(e)
			}
			fb := uatLegacySecretUseFallback(agent, e, tc.depth)
			if !tc.want {
				assert.Nil(t, fb)
				return
			}
			require.NotNil(t, fb)
			assert.Equal(t, e.ID, fb.EdgeID)
			assert.Equal(t, e.Role, fb.Role)
			assert.Equal(t, "uat_created_"+e.Role, fb.cohort())
		})
	}
	assert.Nil(t, uatLegacySecretUseFallback(nil, uatEdge(), 0))
	assert.Nil(t, uatLegacySecretUseFallback(agent, nil, 0))
}

// uatCreated stores a baseline agent owned by the fixture user with the
// edge a user access token create writes: bounded, source uat, no
// secret.use in the ceiling.
func (f *lafFixture) uatCreated(t *testing.T, name string) string {
	t.Helper()
	id := tid("uatl-" + name)
	createDCAgent(t, f.Store, id, f.ProjectID, f.UserID, AgentRoleBaseline)
	require.NoError(t, f.Store.CreateDelegationEdge(context.Background(), &store.DelegationEdge{
		DelegatorType: store.DelegationPrincipalUser, DelegatorID: f.UserID,
		DelegateType: store.DelegationPrincipalAgent, DelegateID: id,
		ScopeType: store.RoleScopeProject, ScopeID: f.ProjectID, Role: string(AgentRoleBaseline), Active: true,
		AuthorityProvenance: store.AuthorityProvenance{
			ProvenanceVersion: store.ProvenanceVersionV1, SourcePrincipalKind: store.DelegationPrincipalUser,
			SourcePrincipalID: f.UserID, SourceCredentialKind: store.SourceCredentialUAT,
		},
		EffectCeiling: store.EffectCeiling{Kind: store.EffectCeilingBounded, PermissionIDs: lafV1(t, AgentRoleBaseline, false)},
	}))
	edges := activeEdgesFor(t, f.Store, id)
	require.Len(t, edges, 1)
	require.False(t, EffectCeilingAllows(edges[0].EffectCeiling, secretUsePermission, false), "the token edge lacks secret.use")
	return id
}

// TestUATLegacyFallback_PreCutoffDeliversOwnerSecrets: an agent created
// from the owner's user access token before the cutoff receives the owner's
// shared secret at start and restart, logged with its cohort; the runtime
// read and the list stay refused, as for the adopted cohort.
func TestUATLegacyFallback_PreCutoffDeliversOwnerSecrets(t *testing.T) {
	setUATLegacyCutoff(t, time.Now().Add(24*time.Hour))
	f := newLAFFixture(t, "uatl-pre")
	id := f.uatCreated(t, "pre")
	log := usaCaptureDefaultLog(t)
	before := f.Server.authzService.LegacyAdoptedFallbackUses()

	d := f.ceiling(t, id, sdAlwaysKey)
	require.True(t, d.Allowed, "the ceiling allows the owner's secret under the fallback; reason %q", d.Reason)
	require.NotNil(t, d.legacyFallback)
	assert.Equal(t, "uat_created_baseline", d.legacyFallback.cohort())
	assertCohortDelivery(t, f.delivery(t, id), "uat_created_baseline")
	assert.Contains(t, log.String(), "cohort=uat_created_baseline")
	assert.NotContains(t, log.String(), sdAlwaysValue, "the Warn never carries the value")
	assert.Greater(t, f.Server.authzService.LegacyAdoptedFallbackUses(), before, "every use is counted")
}

// TestUATLegacyFallback_PostCutoffStillRefused: an agent created from a
// user access token after the cutoff gets the strict check.
func TestUATLegacyFallback_PostCutoffStillRefused(t *testing.T) {
	setUATLegacyCutoff(t, time.Now().Add(-24*time.Hour))
	f := newLAFFixture(t, "uatl-post")
	id := f.uatCreated(t, "post")
	d := f.ceiling(t, id, sdAlwaysKey)
	require.False(t, d.Allowed)
	assert.Equal(t, DenyCauseCeilingEffectExceeded, d.DenyCause)
	assert.False(t, sdStartDelivery(t, f.disp, f.Store, id).always)
}
