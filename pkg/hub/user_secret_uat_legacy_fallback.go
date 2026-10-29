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

package hub

import (
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Legacy fallback for an owner's user secrets: agents created from the
// owner's user access token before the ceiling applied to the owner path.
//
// No user access token scope grants secret.use (ptone/scion#2340), so the
// edge of an agent created from a user access token never holds it, and
// S3c refuses the owner's progeny-shared user secrets at start and restart
// since #4234. Before #4234 the owner path delivered them with no check.
// The launch material fallback already treats these agents as a product
// gap (uat_created_no_deliver_mapping); this is the matching exception for
// the owner's own user secrets, limited to agents that already existed:
//   - the hop is the agent's own edge (depth 0), its delegator is a user
//     who is the agent's owner, and the recorded source principal is that
//     same user;
//   - the edge was recorded from a user access token and is bounded;
//   - the edge was created before uatLegacySecretUseCutoff, so agents
//     created (or role-changed) from a token afterwards get the strict
//     check, as upstream intends.
//
// Every other clause still applies, exactly as for the adopted cohort.
// Uses are counted and logged at Warn with cohort uat_created_<role>.

// uatLegacySecretUseCutoff is the hub's upgrade to a release with #4234.
// Edges created from a user access token before it keep the owner path's
// pre-#4234 behaviour.
var uatLegacySecretUseCutoff = time.Date(2026, time.November, 1, 0, 0, 0, 0, time.UTC)

// uatLegacySecretUseFallback reports whether edge, the hop at depth of
// agent's chain that S3c refuses, qualifies for the user access token
// legacy fallback.
func uatLegacySecretUseFallback(agent *store.Agent, edge *store.DelegationEdge, depth int) *legacyAdoptedFallback {
	if agent == nil || edge == nil || depth != 0 {
		return nil
	}
	if edge.DelegatorType != store.DelegationPrincipalUser || edge.DelegatorID == "" || edge.DelegatorID != agent.OwnerID {
		return nil
	}
	if edge.SourceCredentialKind != store.SourceCredentialUAT || edge.Kind != store.EffectCeilingBounded {
		return nil
	}
	if edge.SourcePrincipalKind != store.DelegationPrincipalUser || edge.SourcePrincipalID != edge.DelegatorID {
		return nil
	}
	if edge.CreatedAt.IsZero() || !edge.CreatedAt.Before(uatLegacySecretUseCutoff) {
		return nil
	}
	return &legacyAdoptedFallback{Role: edge.Role, EdgeID: edge.ID, Cohort: "uat_created_" + edge.Role}
}
