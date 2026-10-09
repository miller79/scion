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
	"context"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// scopedListAuthzStore is mockAuthzStore whose ListProjects honours
// AuthorizedProjectIDs, as the real store does, and records the filter.
type scopedListAuthzStore struct {
	*mockAuthzStore
	lastFilter *store.ProjectFilter
}

func (m *scopedListAuthzStore) ListProjects(ctx context.Context, f store.ProjectFilter, o store.ListOptions) (*store.ListResult[store.Project], error) {
	m.lastFilter = &f
	res, err := m.mockAuthzStore.ListProjects(ctx, f, o)
	if err != nil || res == nil || f.AuthorizedProjectIDs == nil {
		return res, err
	}
	allowed := map[string]bool{}
	for _, id := range f.AuthorizedProjectIDs {
		allowed[id] = true
	}
	items := []store.Project{}
	for _, p := range res.Items {
		if allowed[p.ID] {
			items = append(items, p)
		}
	}
	return &store.ListResult[store.Project]{Items: items, TotalCount: len(items)}, nil
}

func sseScopeFixture(t *testing.T) (*WebServer, *scopedListAuthzStore, *parityRecordingAuditEmitter) {
	t.Helper()
	ms := &scopedListAuthzStore{mockAuthzStore: &mockAuthzStore{
		projects: []store.Project{
			{ID: "proj-1", OwnerID: "user-1"},
			{ID: "proj-2", OwnerID: "user-1"},
			{ID: "proj-3", OwnerID: "other-user"},
		},
		projectMemberships: map[string]*store.ProjectMembership{
			"proj-1:user-1": {ProjectID: "proj-1", UserID: "user-1", Role: store.ProjectRoleOwner},
			"proj-2:user-1": {ProjectID: "proj-2", UserID: "user-1", Role: store.ProjectRoleOwner},
		},
	}}
	authz := NewAuthzService(ms, nil)
	emitter := &parityRecordingAuditEmitter{}
	authz.SetDecisionAuditEmitter(emitter)
	authz.DecisionAuditSampleRate = 1.0
	return &WebServer{store: ms, authzService: authz}, ms, emitter
}

func projectDecisions(records []*store.DecisionAuditRecord) []*store.DecisionAuditRecord {
	var out []*store.DecisionAuditRecord
	for _, r := range records {
		if r.ResourceType == "project" {
			out = append(out, r)
		}
	}
	return out
}

// The project.> expansion lists only the caller's project.read scope and
// decides ActionRead alone: no project outside the scope is decided, and no
// other project action is decided or audited.
func TestExpandSSEWildcards_ProjectWildcard_ScopedReadOnlyDecisions(t *testing.T) {
	ws, ms, emitter := sseScopeFixture(t)
	req := httptest.NewRequest("GET", "/events", nil)
	user := &webSessionUser{UserID: "user-1", Email: "user-1@example.com", Role: "user"}
	req = req.WithContext(context.WithValue(req.Context(), webUserContextKey{}, user))

	expanded := ws.expandSSEWildcards(req, []string{"project.>"})

	assert.ElementsMatch(t, []string{"project.proj-1.>", "project.proj-2.>"}, expanded)
	require.NotNil(t, ms.lastFilter)
	assert.ElementsMatch(t, []string{"proj-1", "proj-2"}, ms.lastFilter.AuthorizedProjectIDs,
		"the project list must be narrowed to the caller's project.read scope")

	decisions := projectDecisions(emitter.snapshot())
	require.Len(t, decisions, 2, "one decision per project in scope")
	for _, d := range decisions {
		assert.Equal(t, "read", d.Permission)
		assert.Equal(t, "allow", d.Result)
		assert.NotEqual(t, "proj-3", d.ResourceID, "a project outside the scope must not be decided")
	}
}

// A user with no project scope gets no project subjects and no project
// decisions.
func TestExpandSSEWildcards_ProjectWildcard_NoScopeNoDecisions(t *testing.T) {
	ws, _, emitter := sseScopeFixture(t)
	req := httptest.NewRequest("GET", "/events", nil)
	user := &webSessionUser{UserID: "outsider", Email: "outsider@example.com", Role: "user"}
	req = req.WithContext(context.WithValue(req.Context(), webUserContextKey{}, user))

	expanded := ws.expandSSEWildcards(req, []string{"project.>", "notification.>"})

	assert.Equal(t, []string{"notification.>"}, expanded)
	assert.Empty(t, projectDecisions(emitter.snapshot()))
}

// Explicit project subjects are checked with ActionRead alone.
func TestAuthorizeSSESubjects_ProjectSubject_ReadOnlyDecision(t *testing.T) {
	ws, _, emitter := sseScopeFixture(t)
	req := httptest.NewRequest("GET", "/events", nil)
	user := &webSessionUser{UserID: "user-1", Email: "user-1@example.com", Role: "user"}
	req = req.WithContext(context.WithValue(req.Context(), webUserContextKey{}, user))

	denied := ws.authorizeSSESubjects(req, []string{"project.proj-1.>", "project.proj-3.>"})

	assert.Equal(t, []string{"project.proj-3.>"}, denied)
	decisions := projectDecisions(emitter.snapshot())
	require.Len(t, decisions, 2, "one read decision per requested project")
	for _, d := range decisions {
		assert.Equal(t, "read", d.Permission)
	}
}
