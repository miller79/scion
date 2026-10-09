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
	"net/http"
	"sort"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// listUsersCaps returns GET /api/v1/users as user ID -> sorted allowed actions.
func listUsersCaps(t *testing.T, srv *Server, u *store.User) map[string][]string {
	t.Helper()
	rec := doRequestAsUser(t, srv, u, http.MethodGet, "/api/v1/users?limit=100", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp ListUsersResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	out := make(map[string][]string, len(resp.Users))
	for _, item := range resp.Users {
		var actions []string
		if item.Cap != nil {
			actions = append(actions, item.Cap.Actions...)
		}
		sort.Strings(actions)
		out[item.ID] = actions
	}
	return out
}

// legacyUsersCaps is what listUsers returned before: every user action
// decided for every listed user, filtered on read.
func legacyUsersCaps(t *testing.T, srv *Server, u *store.User) map[string][]string {
	t.Helper()
	ctx := context.Background()
	all, err := srv.store.ListUsers(ctx, store.UserFilter{}, store.ListOptions{Limit: 100})
	require.NoError(t, err)
	resources := make([]Resource, len(all.Items))
	for i := range all.Items {
		resources[i] = userResource(&all.Items[i])
	}
	caps := srv.authzService.ComputeCapabilitiesBatch(ctx, identityOf(u), resources, "user")
	out := map[string][]string{}
	for i, item := range all.Items {
		if !capabilityAllows(caps[i], ActionRead) {
			continue
		}
		actions := append([]string(nil), caps[i].Actions...)
		sort.Strings(actions)
		out[item.ID] = actions
	}
	return out
}

func usersListFixture(t *testing.T) (*Server, *store.User, *store.User, *store.User) {
	t.Helper()
	srv, s, _, member, _ := msgAuthzSetup(t)
	ctx := context.Background()
	admin, err := s.GetUser(ctx, tid("msg-superadmin"))
	require.NoError(t, err)
	// A hub admin by role binding only: its user role is "member", so the
	// scope lookup, not the legacy role, is what grants the actions.
	createTestUserWithRole(t, s, tid("users-list-binding-admin"), "binding-admin@example.com", store.UserRoleMember, store.SystemRoleSuperAdmin)
	bindingAdmin, err := s.GetUser(ctx, tid("users-list-binding-admin"))
	require.NoError(t, err)
	return srv, member, admin, bindingAdmin
}

// The users list returns the same capabilities as deciding every action for
// every row, for a member, a legacy admin and a hub admin by binding.
func TestListUsers_CapabilitiesMatchFullEvaluation(t *testing.T) {
	srv, member, admin, bindingAdmin := usersListFixture(t)
	for _, u := range []*store.User{member, admin, bindingAdmin} {
		t.Run(u.Email, func(t *testing.T) {
			want := legacyUsersCaps(t, srv, u)
			require.NotEmpty(t, want)
			assert.Equal(t, want, listUsersCaps(t, srv, u))
		})
	}
}

// A member's users list records no denial for another user's management
// actions: only read is decided for other users, and every action for the
// member's own row.
func TestListUsers_MemberDecidesNoManagementActionsForOthers(t *testing.T) {
	srv, member, _, _ := usersListFixture(t)
	emitter := &parityRecordingAuditEmitter{}
	srv.authzService.SetDecisionAuditEmitter(emitter)
	srv.authzService.DecisionAuditSampleRate = 1.0

	caps := listUsersCaps(t, srv, member)
	require.Contains(t, caps, member.ID, "the member's own row is listed")

	others := 0
	for _, r := range emitter.snapshot() {
		if r.ResourceType != "user" {
			continue
		}
		if r.ResourceID == member.ID {
			continue
		}
		others++
		assert.Equal(t, "read", r.Permission, "user %s: only read may be decided for another user", r.ResourceID)
	}
	assert.Greater(t, others, 0, "fixture: other users are listed")
}
