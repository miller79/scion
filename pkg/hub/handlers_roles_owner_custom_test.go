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
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Project owners may assign custom project-scoped roles on their own project.
//
// Previously every custom project role required hub-level role_binding.create,
// so an owner could add members with the three built-in roles but could not
// grant any narrower capability, even though governance already let them
// remove such bindings. The CanDelegate ceiling in createRoleBinding still
// bounds what they can grant.

type ownerCustomRoleFixture struct {
	srv            *Server
	store          store.Store
	owner          *store.User
	projectID      string
	projectSlug    string
	otherProjectID string
	member         *store.User
	withinCeiling  *store.RoleDefinition // project-member perms + agent.message
	beyondCeiling  *store.RoleDefinition // carries agent.port_access, which owners do not hold
}

func setupOwnerCustomRoleFixture(t *testing.T) *ownerCustomRoleFixture {
	t.Helper()
	srv, s := testServer(t)
	ctx := context.Background()

	ownerID, projectID := seedProjectOwner(t, s, "ocr-owner")
	owner, err := s.GetUser(ctx, ownerID)
	require.NoError(t, err)
	_, otherProjectID := seedProjectOwner(t, s, "ocr-other")

	member := &store.User{
		ID: tid("ocr-member"), Email: "ocr-member@test.com", DisplayName: "ocr-member",
		Role: store.UserRoleMember, Status: "active",
	}
	require.NoError(t, s.CreateUser(ctx, member))
	ensureHubMembership(ctx, s, member.ID)

	memberRole, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: memberRole.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      member.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	within, err := s.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name:        "ocr-member-messaging",
		ScopeType:   store.RoleScopeProject,
		Permissions: append(append([]string{}, memberRole.Permissions...), "agent.message"),
	})
	require.NoError(t, err)
	beyond, err := s.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name:        "ocr-port-viewer",
		ScopeType:   store.RoleScopeProject,
		Permissions: []string{"project.read", "agent.list", "agent.read", "agent.port_access"},
	})
	require.NoError(t, err)

	return &ownerCustomRoleFixture{
		srv: srv, store: s, owner: owner, projectID: projectID, projectSlug: "ocr-owner",
		otherProjectID: otherProjectID, member: member, withinCeiling: within, beyondCeiling: beyond,
	}
}

func postRoleBinding(t *testing.T, srv *Server, actor *store.User, roleID, principalType, principalID, scopeType, scopeID string) *httptest.ResponseRecorder {
	t.Helper()
	return doRequestAsUser(t, srv, actor, http.MethodPost, "/api/v1/admin/role-bindings", map[string]interface{}{
		"roleDefinitionId": roleID,
		"principalType":    principalType,
		"principalId":      principalID,
		"scopeType":        scopeType,
		"scopeId":          scopeID,
	})
}

func TestOwnerAssignsCustomProjectRole_WithinCeiling(t *testing.T) {
	f := setupOwnerCustomRoleFixture(t)

	rec := postRoleBinding(t, f.srv, f.owner, f.withinCeiling.ID, "user", f.member.ID, "project", f.projectID)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	bindings, err := f.store.ListRoleBindingsForPrincipal(context.Background(), store.RoleBindingPrincipalUser, f.member.ID)
	require.NoError(t, err)
	found := false
	for _, b := range bindings {
		if b.RoleDefinitionID == f.withinCeiling.ID && b.ScopeID == f.projectID {
			found = true
		}
	}
	assert.True(t, found, "custom binding should exist on the owner's project")
}

func TestOwnerAssignsCustomProjectRole_ByProjectSlug(t *testing.T) {
	f := setupOwnerCustomRoleFixture(t)

	rec := postRoleBinding(t, f.srv, f.owner, f.withinCeiling.ID, "user", f.member.ID, "project", f.projectSlug)
	assert.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
}

func TestOwnerCannotAssignCustomProjectRole_BeyondCeiling(t *testing.T) {
	f := setupOwnerCustomRoleFixture(t)

	rec := postRoleBinding(t, f.srv, f.owner, f.beyondCeiling.ID, "user", f.member.ID, "project", f.projectID)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "cannot create binding",
		"the refusal should come from the delegation ceiling, not the entry gate")
}

func TestOwnerCannotAssignCustomProjectRole_OnAnotherProject(t *testing.T) {
	f := setupOwnerCustomRoleFixture(t)

	rec := postRoleBinding(t, f.srv, f.owner, f.withinCeiling.ID, "user", f.member.ID, "project", f.otherProjectID)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
}

func TestMemberCannotAssignCustomProjectRole(t *testing.T) {
	f := setupOwnerCustomRoleFixture(t)

	rec := postRoleBinding(t, f.srv, f.member, f.withinCeiling.ID, "user", f.member.ID, "project", f.projectID)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
}

func TestOwnerCannotAssignCustomProjectRole_ToAgent(t *testing.T) {
	f := setupOwnerCustomRoleFixture(t)

	rec := postRoleBinding(t, f.srv, f.owner, f.withinCeiling.ID, "agent", tid("ocr-agent"), "project", f.projectID)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
}

func TestOwnerCannotCreateSystemScopedBinding(t *testing.T) {
	f := setupOwnerCustomRoleFixture(t)
	hubMember, err := f.store.GetRoleDefinitionByName(context.Background(), store.SystemRoleHubMember, store.RoleScopeSystem)
	require.NoError(t, err)

	rec := postRoleBinding(t, f.srv, f.owner, hubMember.ID, "user", f.member.ID, "system", "")
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
}

// Hub admins keep their existing path: they pass the entry gate on
// role_binding.create, and the CanDelegate ceiling then applies exactly as
// before. hub-admin is a system role that carries no project permissions such
// as agent.create, so on a project where the admin holds no project role the
// ceiling refuses a role built from project-member permissions. This pins that
// the owner path neither widened nor narrowed hub-admin behavior.
func TestHubAdminCustomProjectRole_UnchangedCeilingBehavior(t *testing.T) {
	f := setupOwnerCustomRoleFixture(t)
	ctx := context.Background()

	admin := &store.User{
		ID: tid("ocr-admin"), Email: "ocr-admin@test.com", DisplayName: "ocr-admin",
		Role: store.UserRoleMember, Status: "active",
	}
	require.NoError(t, f.store.CreateUser(ctx, admin))
	ensureHubMembership(ctx, f.store, admin.ID)
	hubAdmin, err := f.store.GetRoleDefinitionByName(ctx, store.SystemRoleHubAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	_, err = f.store.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: hubAdmin.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      admin.ID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	rec := postRoleBinding(t, f.srv, admin, f.withinCeiling.ID, "user", f.member.ID, "project", f.otherProjectID)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "cannot create binding",
		"a hub admin should reach the delegation ceiling, not be stopped at the entry gate")
}

func TestOwnerRemovesAssignedCustomProjectRole(t *testing.T) {
	f := setupOwnerCustomRoleFixture(t)

	rec := postRoleBinding(t, f.srv, f.owner, f.withinCeiling.ID, "user", f.member.ID, "project", f.projectID)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var created struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	require.NotEmpty(t, created.ID)

	rec = doRequestAsUser(t, f.srv, f.owner, http.MethodDelete,
		"/api/v1/projects/"+f.projectID+"/members/"+created.ID, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	_, err := f.store.GetRoleBinding(context.Background(), created.ID)
	assert.True(t, err != nil && strings.Contains(strings.ToLower(err.Error()), "not found"),
		"binding should be gone after removal; got err=%v", err)
}
