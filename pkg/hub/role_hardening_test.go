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
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newHardeningHubAdmin creates a user bound to the built-in hub-admin role
// (not super-admin).
func newHardeningHubAdmin(t *testing.T, s store.Store, name string) *store.User {
	t.Helper()
	id := tid(name)
	createTestUserWithRole(t, s, id, name+"@test.com", "member", store.SystemRoleHubAdmin)
	u, err := s.GetUser(context.Background(), id)
	require.NoError(t, err)
	return u
}

// =============================================================================
// Pre-start hooks: writing needs project.manage_pre_start_hooks, which no
// built-in project role holds.
// =============================================================================

func TestProjectPreStartHookWrite_ProjectOwnerDenied(t *testing.T) {
	srv, s, alice, _, project := setupDemoPolicyTest(t)
	ctx := context.Background()

	// Positive control: alice owns the project (she may update it).
	aliceIdent := NewAuthenticatedUser(alice.ID, alice.Email, alice.DisplayName, "member", "api")
	require.True(t, srv.authzService.CheckAccess(ctx, aliceIdent, projectResource(project), ActionUpdate).Allowed,
		"alice must hold project.update for this test to mean anything")

	base := "/api/v1/projects/" + project.ID + "/pre-start-hooks"
	rec := doRequestAsUser(t, srv, alice, http.MethodPost, base, CreateProjectPreStartHookRequest{
		Name: "owner hook", Script: "#!/bin/sh\necho owned\n",
	})
	assert.Equal(t, http.StatusForbidden, rec.Code, "project owner must not create a pre-start hook: %s", rec.Body.String())

	hook, err := s.CreateProjectPreStartHook(ctx, &store.ProjectPreStartHook{
		ProjectID: project.ID, Name: "admin hook", Slug: "admin-hook", Script: "#!/bin/sh\ntrue\n",
	})
	require.NoError(t, err)

	newScript := "#!/bin/sh\necho changed\n"
	rec = doRequestAsUser(t, srv, alice, http.MethodPut, base+"/"+hook.ID, UpdateProjectPreStartHookRequest{Script: &newScript})
	assert.Equal(t, http.StatusForbidden, rec.Code, "project owner must not update a pre-start hook: %s", rec.Body.String())
	rec = doRequestAsUser(t, srv, alice, http.MethodPost, base+"/"+hook.ID+"/activate", nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, "project owner must not activate a pre-start hook: %s", rec.Body.String())
	rec = doRequestAsUser(t, srv, alice, http.MethodDelete, base+"/"+hook.ID, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, "project owner must not delete a pre-start hook: %s", rec.Body.String())

	stored, err := s.GetProjectPreStartHook(ctx, hook.ID, project.ID)
	require.NoError(t, err)
	assert.Equal(t, "#!/bin/sh\ntrue\n", stored.Script, "the hook is unchanged")

	// Reading is unchanged: the owner still lists and reads hooks.
	rec = doRequestAsUser(t, srv, alice, http.MethodGet, base, nil)
	assert.Equal(t, http.StatusOK, rec.Code, "project owner still lists hooks: %s", rec.Body.String())
	rec = doRequestAsUser(t, srv, alice, http.MethodGet, base+"/"+hook.ID, nil)
	assert.Equal(t, http.StatusOK, rec.Code, "project owner still reads a hook: %s", rec.Body.String())
}

func TestProjectPreStartHookWrite_HubAdminAllowed(t *testing.T) {
	srv, s, _, _, project := setupDemoPolicyTest(t)
	hubAdmin := newHardeningHubAdmin(t, s, "psh-hub-admin")

	rec := doRequestAsUser(t, srv, hubAdmin, http.MethodPost, "/api/v1/projects/"+project.ID+"/pre-start-hooks", CreateProjectPreStartHookRequest{
		Name: "admin hook", Script: "#!/bin/sh\ntrue\n",
	})
	assert.Equal(t, http.StatusCreated, rec.Code, "hub admin creates a pre-start hook: %s", rec.Body.String())
}

func TestBuiltInProjectRoles_NoPreStartHookWrite(t *testing.T) {
	for _, role := range BuiltInRoles() {
		if role.ScopeType != store.RoleScopeProject {
			continue
		}
		assert.NotContains(t, role.Permissions, "project.manage_pre_start_hooks", "built-in project role %s", role.Name)
	}
	for _, role := range BuiltInRoles() {
		if role.Name == store.SystemRoleHubAdmin || role.Name == store.SystemRoleSuperAdmin {
			assert.Contains(t, role.Permissions, "project.manage_pre_start_hooks", "%s keeps hook writes", role.Name)
		}
	}
}

// =============================================================================
// Runtime brokers: hub-member no longer holds broker.create.
// =============================================================================

func TestHubMember_CannotRegisterBroker(t *testing.T) {
	srv, s := testServer(t)
	member := newHubMemberUser(t, s, "rh-broker-member")

	rec := mintJoinToken(t, srv, member, "rh-member-broker", 600)
	assert.Equal(t, http.StatusForbidden, rec.Code, "an ordinary hub member must not register a broker: %s", rec.Body.String())
	_, err := s.GetRuntimeBrokerByName(context.Background(), "rh-member-broker")
	assert.ErrorIs(t, err, store.ErrNotFound)

	hubAdmin := newHardeningHubAdmin(t, s, "rh-broker-hub-admin")
	rec = mintJoinToken(t, srv, hubAdmin, "rh-admin-broker", 600)
	assert.Equal(t, http.StatusCreated, rec.Code, "a hub admin registers a broker: %s", rec.Body.String())

	for _, role := range BuiltInRoles() {
		if role.Name == store.SystemRoleHubMember {
			assert.NotContains(t, role.Permissions, "broker.create")
		}
	}
}

// =============================================================================
// Name shadowing: a project or user template or harness config may not take
// the slug of a global one.
// =============================================================================

func TestTemplateCreate_ProjectSlugShadowingGlobalRefused(t *testing.T) {
	srv, s, alice, _, project := setupDemoPolicyTest(t)
	ctx := context.Background()
	require.NoError(t, s.CreateTemplate(ctx, &store.Template{
		ID: tid("rh-global-tmpl"), Name: "rh-shadowed", Slug: "rh-shadowed",
		Scope: store.TemplateScopeGlobal, Status: store.TemplateStatusActive,
	}))

	rec := doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/templates", CreateTemplateRequest{
		Name: "RH Shadowed", Harness: "claude", Scope: store.TemplateScopeProject, ScopeID: project.ID,
	})
	assert.Equal(t, http.StatusConflict, rec.Code, "a project template may not shadow a global one: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "shadows_global")

	rec = doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/templates", CreateTemplateRequest{
		Name: "rh-shadowed", Harness: "claude", Scope: store.TemplateScopeUser,
	})
	assert.Equal(t, http.StatusConflict, rec.Code, "a user template may not shadow a global one: %s", rec.Body.String())

	// A distinct name is still created, and renaming it onto the global
	// slug is refused.
	rec = doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/templates", CreateTemplateRequest{
		Name: "rh-own", Harness: "claude", Scope: store.TemplateScopeProject, ScopeID: project.ID,
	})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	var created CreateTemplateResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	require.NotNil(t, created.Template)

	rec = doRequestAsUser(t, srv, alice, http.MethodPatch, "/api/v1/templates/"+created.Template.ID, map[string]string{"slug": "rh-shadowed"})
	assert.Equal(t, http.StatusConflict, rec.Code, "renaming a project template onto a global slug is refused: %s", rec.Body.String())
	stored, err := s.GetTemplate(ctx, created.Template.ID)
	require.NoError(t, err)
	assert.Equal(t, "rh-own", stored.Slug)
}

func TestHarnessConfigCreate_ProjectSlugShadowingGlobalRefused(t *testing.T) {
	srv, s, alice, _, project := setupDemoPolicyTest(t)
	ctx := context.Background()
	require.NoError(t, s.CreateHarnessConfig(ctx, &store.HarnessConfig{
		ID: tid("rh-global-hc"), Name: "rh-hc-shadowed", Slug: "rh-hc-shadowed", Harness: "claude",
		Scope: store.HarnessConfigScopeGlobal, Status: store.HarnessConfigStatusActive,
	}))

	rec := doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/harness-configs", CreateHarnessConfigRequest{
		Name: "rh-hc-shadowed", Harness: "claude", Scope: store.HarnessConfigScopeProject, ScopeID: project.ID,
	})
	assert.Equal(t, http.StatusConflict, rec.Code, "a project harness config may not shadow a global one: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "shadows_global")

	rec = doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/harness-configs", CreateHarnessConfigRequest{
		Name: "rh-hc-own", Harness: "claude", Scope: store.HarnessConfigScopeProject, ScopeID: project.ID,
	})
	assert.Equal(t, http.StatusCreated, rec.Code, "a distinct name is still created: %s", rec.Body.String())
}

func TestResourceImport_ProjectSlugShadowingGlobalRefused(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	require.NoError(t, s.CreateTemplate(ctx, &store.Template{
		ID: tid("rh-imp-global-tmpl"), Name: "rh-imp", Slug: "rh-imp",
		Scope: store.TemplateScopeGlobal, Status: store.TemplateStatusActive,
	}))
	require.NoError(t, s.CreateHarnessConfig(ctx, &store.HarnessConfig{
		ID: tid("rh-imp-global-hc"), Name: "rh-imp", Slug: "rh-imp", Harness: "claude",
		Scope: store.HarnessConfigScopeGlobal, Status: store.HarnessConfigStatusActive,
	}))

	rec := &ResourceRecord{Name: "rh-imp", Slug: "rh-imp", Scope: store.TemplateScopeProject, ScopeID: tid("rh-imp-proj")}
	err := (&templatePersistence{s: srv}).Create(ctx, rec, nil)
	assert.ErrorIs(t, err, errShadowsGlobalSlug, "template import into a project may not shadow a global template")
	err = (&harnessConfigPersistence{s: srv}).Create(ctx, rec, nil)
	assert.ErrorIs(t, err, errShadowsGlobalSlug, "harness config import into a project may not shadow a global one")
}

// =============================================================================
// Message mode: an agent changes the mode only of its own descendants.
// =============================================================================

func TestSetMessageMode_FullRoleAgentPeerDenied(t *testing.T) {
	srv, s, owner, member, _, projectID := smmSetup(t)

	// The target belongs to another user; the caller is a full-role agent
	// in the same project but not its ancestor.
	target := smmAgent(t, s, "smm-peer-target", projectID, store.MessageModeNone, []string{member.ID})
	caller := smmAgent(t, s, "smm-peer-caller", projectID, store.MessageModeProject, []string{owner.ID})
	callerIdent := msgAuthzAgentIdentity(caller.ID, projectID, caller.Ancestry, ScopesForRole(AgentRoleFull)...)

	rr := smmDoRequest(t, srv, target.ID, SetMessageModeRequest{Mode: "project"}, callerIdent)
	assert.Equal(t, http.StatusForbidden, rr.Code, "a peer agent may not change the message mode: %s", rr.Body.String())
	got, err := s.GetAgent(context.Background(), target.ID)
	require.NoError(t, err)
	assert.Equal(t, store.MessageModeNone, got.MessageMode, "the quarantined agent stays quarantined")

	// Nor may an agent change its own mode.
	self := smmDoRequest(t, srv, caller.ID, SetMessageModeRequest{Mode: "hub"}, callerIdent)
	assert.Equal(t, http.StatusForbidden, self.Code, "an agent may not change its own message mode: %s", self.Body.String())

	// A descendant of the caller is still allowed.
	child := smmAgent(t, s, "smm-peer-child", projectID, store.MessageModeProject, []string{owner.ID, caller.ID})
	ok := smmDoRequest(t, srv, child.ID, SetMessageModeRequest{Mode: "lineage"}, callerIdent)
	assert.Equal(t, http.StatusOK, ok.Code, "an ancestor agent changes its descendant's mode: %s", ok.Body.String())
}

// =============================================================================
// Project clone: needs project.update on the source.
// =============================================================================

func TestProjectClone_ReadOnlyMemberDenied(t *testing.T) {
	srv, s, alice, bob, project := setupDemoPolicyTest(t)
	ctx := context.Background()
	addProjectMemberWithRole(t, s, project, bob.ID, store.GroupMemberRoleMember)

	bobIdent := NewAuthenticatedUser(bob.ID, bob.Email, bob.DisplayName, "member", "api")
	require.True(t, srv.authzService.CheckAccess(ctx, bobIdent, projectResource(project), ActionRead).Allowed,
		"bob must read the source project for this test to mean anything")

	rec := doRequestAsUser(t, srv, bob, http.MethodPost, "/api/v1/projects/"+project.ID+"/clone", CloneProjectRequest{Name: "bob-clone"})
	assert.Equal(t, http.StatusForbidden, rec.Code, "a project member with read access may not clone: %s", rec.Body.String())

	rec = doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/projects/"+project.ID+"/clone", CloneProjectRequest{Name: "alice-clone"})
	assert.Equal(t, http.StatusCreated, rec.Code, "the source owner clones: %s", rec.Body.String())
}

// =============================================================================
// Service-account registration: the Hub's and broker hosts' accounts are
// reserved.
// =============================================================================

func TestCreateGCPServiceAccount_RefusesHubAndBrokerHostSA(t *testing.T) {
	srv, s := testServer(t)
	projectID := createTestProjectForSA(t, srv, s)
	srv.gcpTokenGenerator = &mockGCPTokenGenerator{email: "hub-sa@hub-project.iam.gserviceaccount.com"}
	require.NoError(t, s.CreateRuntimeBroker(context.Background(), &store.RuntimeBroker{
		ID: tid("rh-sa-broker"), Name: "rh-sa-broker", Slug: "rh-sa-broker",
		GCPHostServiceAccountEmail: "broker-host@host-project.iam.gserviceaccount.com",
		Status:                     store.BrokerStatusOnline, Created: time.Now(), Updated: time.Now(),
	}))
	path := fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts", projectID)

	for _, email := range []string{
		"hub-sa@hub-project.iam.gserviceaccount.com",
		"HUB-SA@hub-project.iam.gserviceaccount.com",
		"broker-host@host-project.iam.gserviceaccount.com",
	} {
		rec := doRequest(t, srv, http.MethodPost, path, map[string]string{"email": email})
		assert.Equal(t, http.StatusUnprocessableEntity, rec.Code, "%s must be refused: %s", email, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "reserved_service_account")
	}
	list, err := s.ListGCPServiceAccounts(context.Background(), store.GCPServiceAccountFilter{Scope: store.ScopeProject, ScopeID: projectID})
	require.NoError(t, err)
	assert.Empty(t, list, "no reserved account was registered")

	rec := doRequest(t, srv, http.MethodPost, path, map[string]string{"email": "agent@my-project.iam.gserviceaccount.com"})
	assert.Equal(t, http.StatusCreated, rec.Code, "an ordinary account is still registered: %s", rec.Body.String())
}
