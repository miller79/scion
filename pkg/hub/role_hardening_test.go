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

// Re-registering or re-minting a broker the caller already owns goes
// through the same broker.create gate as a first registration: a plain hub
// member who registered a broker before hub-member lost broker.create needs
// an admin to grant it before re-registering.
func TestHubMemberBrokerOwner_CannotReregisterOrRemint(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	owner := newHubMemberUser(t, s, "rh-broker-owner")
	broker := createReregistrationTestBroker(t, s, "rh-owned-broker", owner.ID)

	// Re-mint a join token for the owned broker (PreserveSettings).
	rec := mintJoinToken(t, srv, owner, broker.Name, 600)
	assert.Equal(t, http.StatusForbidden, rec.Code, "a hub-member owner may not re-mint: %s", rec.Body.String())
	// Re-register the owned broker by ID.
	rec = doRequestAsUserLoopback(t, srv, owner, http.MethodPost, "/api/v1/brokers", CreateBrokerRegistrationRequest{
		Name: broker.Name, BrokerID: broker.ID,
	})
	assert.Equal(t, http.StatusForbidden, rec.Code, "a hub-member owner may not re-register: %s", rec.Body.String())
	stored, err := s.GetRuntimeBroker(ctx, broker.ID)
	require.NoError(t, err)
	assert.Equal(t, owner.ID, stored.CreatedBy)
	assert.Equal(t, broker.Labels, stored.Labels, "the refused requests changed nothing")

	// With broker.create granted, the same owner re-mints.
	grantBrokerCreate(t, s, owner.ID, nil)
	rec = mintJoinToken(t, srv, owner, broker.Name, 600)
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	assert.Equal(t, broker.ID, decodeRegistration(t, rec).BrokerID)
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

// The CLI finds a template or harness config by name as well as by slug
// (cmd/template_resolution.go, cmd/harness_config.go), project scope first,
// so a project or user record whose name (or the slug of its name) is a
// global record's slug or name shadows it however its own slug differs.
func TestTemplate_NameShadowingGlobalRefused(t *testing.T) {
	srv, s, alice, _, project := setupDemoPolicyTest(t)
	ctx := context.Background()
	require.NoError(t, s.CreateTemplate(ctx, &store.Template{
		ID: tid("rhn-global-tmpl"), Name: "rhn-claude", Slug: "rhn-claude",
		Scope: store.TemplateScopeGlobal, Status: store.TemplateStatusActive,
	}))
	require.NoError(t, s.CreateTemplate(ctx, &store.Template{
		ID: tid("rhn-global-tmpl2"), Name: "RHN Display", Slug: "rhn-display-v2",
		Scope: store.TemplateScopeGlobal, Status: store.TemplateStatusActive,
	}))

	for name, req := range map[string]CreateTemplateRequest{
		"project name is a global slug":   {Name: "rhn-claude", Slug: "x-rhn-claude", Scope: store.TemplateScopeProject, ScopeID: project.ID},
		"slugified name is a global slug": {Name: "RHN-Claude", Slug: "y-rhn-claude", Scope: store.TemplateScopeProject, ScopeID: project.ID},
		"project name is a global name":   {Name: "RHN Display", Slug: "z-rhn-display", Scope: store.TemplateScopeProject, ScopeID: project.ID},
		"user name is a global slug":      {Name: "rhn-claude", Slug: "u-rhn-claude", Scope: store.TemplateScopeUser},
	} {
		req.Harness = "claude"
		rec := doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/templates", req)
		assert.Equal(t, http.StatusConflict, rec.Code, "%s: %s", name, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "shadows_global", name)
	}
	rec := doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/users/me/templates", CreateTemplateRequest{
		Name: "rhn-claude", Slug: "me-rhn-claude", Harness: "claude",
	})
	assert.Equal(t, http.StatusConflict, rec.Code, "user template route: %s", rec.Body.String())

	// A distinct name is created; renaming it onto a global name, with
	// its own slug unchanged, is refused by PATCH and PUT.
	rec = doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/templates", CreateTemplateRequest{
		Name: "rhn-own", Slug: "rhn-own", Harness: "claude", Scope: store.TemplateScopeProject, ScopeID: project.ID,
	})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	var created CreateTemplateResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	require.NotNil(t, created.Template)
	path := "/api/v1/templates/" + created.Template.ID
	rec = doRequestAsUser(t, srv, alice, http.MethodPatch, path, map[string]string{"name": "rhn-claude"})
	assert.Equal(t, http.StatusConflict, rec.Code, "PATCH name onto a global slug: %s", rec.Body.String())
	rec = doRequestAsUser(t, srv, alice, http.MethodPut, path, UpdateTemplateRequest{Name: "RHN Display", Slug: "rhn-own"})
	assert.Equal(t, http.StatusConflict, rec.Code, "PUT name onto a global name: %s", rec.Body.String())
	stored, err := s.GetTemplate(ctx, created.Template.ID)
	require.NoError(t, err)
	assert.Equal(t, "rhn-own", stored.Name)

	// An unrelated rename still works.
	rec = doRequestAsUser(t, srv, alice, http.MethodPatch, path, map[string]string{"name": "rhn-own-2"})
	assert.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	// Import and sync.
	err = (&templatePersistence{s: srv}).Create(ctx, &ResourceRecord{Name: "rhn-claude", Slug: "imp-rhn", Scope: store.TemplateScopeProject, ScopeID: project.ID}, nil)
	assert.ErrorIs(t, err, errShadowsGlobalSlug, "import of a project template named as a global one")
}

func TestHarnessConfig_NameShadowingGlobalRefused(t *testing.T) {
	srv, s, alice, _, project := setupDemoPolicyTest(t)
	ctx := context.Background()
	require.NoError(t, s.CreateHarnessConfig(ctx, &store.HarnessConfig{
		ID: tid("rhn-global-hc"), Name: "rhn-hc", Slug: "rhn-hc", Harness: "claude",
		Scope: store.HarnessConfigScopeGlobal, Status: store.HarnessConfigStatusActive,
	}))

	rec := doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/harness-configs", CreateHarnessConfigRequest{
		Name: "rhn-hc", Slug: "x-rhn-hc", Harness: "claude", Scope: store.HarnessConfigScopeProject, ScopeID: project.ID,
	})
	assert.Equal(t, http.StatusConflict, rec.Code, "a project harness config named as a global one: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "shadows_global")

	rec = doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/harness-configs", CreateHarnessConfigRequest{
		Name: "rhn-hc-own", Slug: "rhn-hc-own", Harness: "claude", Scope: store.HarnessConfigScopeProject, ScopeID: project.ID,
	})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	var created CreateHarnessConfigResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	require.NotNil(t, created.HarnessConfig)
	path := "/api/v1/harness-configs/" + created.HarnessConfig.ID
	rec = doRequestAsUser(t, srv, alice, http.MethodPatch, path, map[string]string{"name": "rhn-hc"})
	assert.Equal(t, http.StatusConflict, rec.Code, "PATCH name onto a global slug: %s", rec.Body.String())
	rec = doRequestAsUser(t, srv, alice, http.MethodPut, path, UpdateHarnessConfigRequest{Name: "rhn-hc", Slug: "rhn-hc-own"})
	assert.Equal(t, http.StatusConflict, rec.Code, "PUT name onto a global slug: %s", rec.Body.String())

	err := (&harnessConfigPersistence{s: srv}).Create(ctx, &ResourceRecord{Name: "rhn-hc", Slug: "imp-rhn-hc", Scope: store.HarnessConfigScopeProject, ScopeID: project.ID}, nil)
	assert.ErrorIs(t, err, errShadowsGlobalSlug, "import of a project harness config named as a global one")
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

// The hub-scope bring-your-own route refuses the same reserved accounts as
// the project route.
func TestCreateHubScopedGCPServiceAccount_RefusesHubAndBrokerHostSA(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	srv.gcpTokenGenerator = &mockGCPTokenGenerator{email: "hub-sa@hub-project.iam.gserviceaccount.com"}
	require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID: tid("rh-hubsa-broker"), Name: "rh-hubsa-broker", Slug: "rh-hubsa-broker",
		GCPHostServiceAccountEmail: "broker-host@host-project.iam.gserviceaccount.com",
		Status:                     store.BrokerStatusOnline, Created: time.Now(), Updated: time.Now(),
	}))
	member := newHubMemberUser(t, s, "rh-hubsa-member")
	const path = "/api/v1/gcp-service-accounts?scope=hub"

	for _, email := range []string{
		"hub-sa@hub-project.iam.gserviceaccount.com",
		"HUB-SA@hub-project.iam.gserviceaccount.com",
		"broker-host@host-project.iam.gserviceaccount.com",
	} {
		rec := doRequestAsUser(t, srv, member, http.MethodPost, path, map[string]string{"email": email})
		assert.Equal(t, http.StatusUnprocessableEntity, rec.Code, "member: %s must be refused: %s", email, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "reserved_service_account")
		rec = doRequest(t, srv, http.MethodPost, path, map[string]string{"email": email})
		assert.Equal(t, http.StatusUnprocessableEntity, rec.Code, "admin: %s must be refused: %s", email, rec.Body.String())
	}
	list, err := s.ListGCPServiceAccounts(ctx, store.GCPServiceAccountFilter{Scope: store.ScopeHub})
	require.NoError(t, err)
	assert.Empty(t, list, "no reserved account was registered")

	rec := doRequestAsUser(t, srv, member, http.MethodPost, path, map[string]string{"email": "agent@my-project.iam.gserviceaccount.com"})
	assert.Equal(t, http.StatusCreated, rec.Code, "an ordinary account is still registered: %s", rec.Body.String())
}

func TestProjectClone_SkipsReservedServiceAccount(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	projectID := createTestProjectForSA(t, srv, s)
	srv.gcpTokenGenerator = &mockGCPTokenGenerator{email: "hub-sa@hub-project.iam.gserviceaccount.com"}

	// Rows registered before reserved accounts were refused.
	reserved := &store.GCPServiceAccount{ID: tid("rh-clone-reserved-sa"), Scope: store.ScopeProject, ScopeID: projectID,
		Email: "hub-sa@hub-project.iam.gserviceaccount.com", ProjectID: "hub-project", Verified: true}
	ordinary := &store.GCPServiceAccount{ID: tid("rh-clone-ordinary-sa"), Scope: store.ScopeProject, ScopeID: projectID,
		Email: "agent@my-project.iam.gserviceaccount.com", ProjectID: "my-project", Verified: true}
	require.NoError(t, s.CreateGCPServiceAccount(ctx, reserved))
	require.NoError(t, s.CreateGCPServiceAccount(ctx, ordinary))

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+projectID+"/clone", CloneProjectRequest{Name: "rh-sa-clone"})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	var clone store.Project
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &clone))

	list, err := s.ListGCPServiceAccounts(ctx, store.GCPServiceAccountFilter{Scope: store.ScopeProject, ScopeID: clone.ID})
	require.NoError(t, err)
	var emails []string
	for _, sa := range list {
		emails = append(emails, sa.Email)
	}
	assert.Equal(t, []string{"agent@my-project.iam.gserviceaccount.com"}, emails, "the reserved account is not carried into the clone")
}
