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
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// doScheduleSessionRequest calls handleSchedules as identity with an
// interactive session credential.
func doScheduleSessionRequest(t *testing.T, srv *Server, identity Identity, method, projectID, schedulePath string) *httptest.ResponseRecorder {
	t.Helper()
	urlPath := "/api/v1/projects/" + projectID + "/schedules/" + schedulePath
	req := httptest.NewRequest(method, urlPath, bytes.NewReader(nil))
	ctx := contextWithIdentity(req.Context(), identity)
	ctx = contextWithCredentialContext(ctx, CredentialContext{Kind: CredentialKindInteractive, Type: identity.Type()})
	rec := httptest.NewRecorder()
	srv.handleSchedules(rec, req.WithContext(ctx), projectID, schedulePath)
	return rec
}

// TestSchedule_CreatorMayPauseAndDelete: a project member, who holds
// neither scheduled_event.update nor scheduled_event.delete, may pause and
// delete a schedule they created, and no one else's; resume and edit stay
// refused.
func TestSchedule_CreatorMayPauseAndDelete(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	ctx := context.Background()

	memberID := tid("sched-creator-member")
	createTestUserWithProjectRole(t, s, memberID, "schedcreator@test.com", projectID, store.ProjectRoleMember)
	member := NewAuthenticatedUser(memberID, "schedcreator@test.com", "Creator", "member", "web")
	otherID := tid("sched-creator-other")
	createTestUserWithProjectRole(t, s, otherID, "schedother@test.com", projectID, store.ProjectRoleMember)

	mk := func(name, createdBy, authorizedBy string) *store.Schedule {
		next := time.Now().Add(time.Hour).UTC()
		sc := &store.Schedule{
			ID: tid("sched-creator-" + name), ProjectID: projectID, Name: name, CronExpr: "0 * * * *",
			EventType: "message", Payload: `{"agentName":"all","message":"hi"}`,
			Status: store.ScheduleStatusActive, NextRunAt: &next, CreatedBy: createdBy,
			InitiatorAttribution: store.InitiatorAttribution{
				InitiatorPrincipalKind: member.Type(), InitiatorPrincipalID: authorizedBy,
				InitiatorCredentialKind: store.InitiatorCredentialKindSession,
				AttributionVersion:      1, AuthorizationRevision: 1,
			},
		}
		require.NoError(t, s.CreateSchedule(ctx, sc))
		return sc
	}
	own := mk("own", memberID, memberID)
	ownDel := mk("own-delete", memberID, memberID)
	others := mk("others", otherID, otherID)
	// Created by the member, then re-authorized (edited or resumed) by
	// someone else: it now runs under that principal.
	reattributed := mk("reattributed", memberID, otherID)

	// Someone else's schedule: still refused.
	assert.Equal(t, http.StatusForbidden, doScheduleSessionRequest(t, srv, member, http.MethodPost, projectID, others.ID+"/pause").Code)
	assert.Equal(t, http.StatusForbidden, doScheduleSessionRequest(t, srv, member, http.MethodPost, projectID, reattributed.ID+"/pause").Code)
	assert.Equal(t, http.StatusForbidden, doScheduleSessionRequest(t, srv, member, http.MethodDelete, projectID, reattributed.ID).Code)
	assert.Equal(t, http.StatusForbidden, doScheduleSessionRequest(t, srv, member, http.MethodDelete, projectID, others.ID).Code)

	// Their own: pause and delete succeed.
	rec := doScheduleSessionRequest(t, srv, member, http.MethodPost, projectID, own.ID+"/pause")
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got, err := s.GetSchedule(ctx, own.ID)
	require.NoError(t, err)
	assert.Equal(t, store.ScheduleStatusPaused, got.Status)

	rec = doScheduleSessionRequest(t, srv, member, http.MethodDelete, projectID, ownDel.ID)
	assert.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	_, err = s.GetSchedule(ctx, ownDel.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)

	// Resume re-arms future work: not covered by creator ownership.
	rec = doScheduleSessionRequest(t, srv, member, http.MethodPost, projectID, own.ID+"/resume")
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	// A non-session credential does not get the creator path.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/"+projectID+"/schedules/"+others.ID+"/pause", nil)
	noCred := contextWithIdentity(req.Context(), NewAuthenticatedUser(otherID, "schedother@test.com", "Other", "member", "api"))
	rec = httptest.NewRecorder()
	srv.handleSchedules(rec, req.WithContext(noCred), projectID, others.ID+"/pause")
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
}

// reauthorizeOnFirstReadStore re-authorizes schedule id to ownerID (a new
// initiator and authorization revision) right after the first GetSchedule
// of it returns, as an owner edit or resume landing between the creator
// check and the handler that acts on it.
type reauthorizeOnFirstReadStore struct {
	store.Store
	t       *testing.T
	id      string
	ownerID string
	reads   *int
}

func (s reauthorizeOnFirstReadStore) GetSchedule(ctx context.Context, id string) (*store.Schedule, error) {
	sc, err := s.Store.GetSchedule(ctx, id)
	if err != nil || id != s.id {
		return sc, err
	}
	*s.reads++
	if *s.reads == 1 {
		next := *sc
		attr := sc.InitiatorAttribution
		attr.InitiatorPrincipalID = s.ownerID
		attr.AuthorizationRevision = sc.AuthorizationRevision + 1
		require.NoError(s.t, s.Store.UpdateSchedule(ctx, &next, store.ScheduleFieldMask{Status: true},
			sc.AuthorizationRevision, true, &attr))
	}
	return sc, nil
}

// TestSchedule_CreatorStopRefusedWhenReauthorizedMeanwhile: when an owner
// re-authorizes a schedule between the creator check and the pause or
// delete that follows it, the creator's request is refused with 409 and the
// schedule, now running under the owner, is left unchanged.
func TestSchedule_CreatorStopRefusedWhenReauthorizedMeanwhile(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	ctx := context.Background()

	memberID := tid("sched-race-member")
	createTestUserWithProjectRole(t, s, memberID, "schedrace@test.com", projectID, store.ProjectRoleMember)
	member := NewAuthenticatedUser(memberID, "schedrace@test.com", "Creator", "member", "web")
	ownerID := tid("sched-race-owner")

	mk := func(name string) *store.Schedule {
		next := time.Now().Add(time.Hour).UTC()
		sc := &store.Schedule{
			ID: tid("sched-race-" + name), ProjectID: projectID, Name: name, CronExpr: "0 * * * *",
			EventType: "message", Payload: `{"agentName":"all","message":"hi"}`,
			Status: store.ScheduleStatusActive, NextRunAt: &next, CreatedBy: memberID,
			InitiatorAttribution: store.InitiatorAttribution{
				InitiatorPrincipalKind: member.Type(), InitiatorPrincipalID: memberID,
				InitiatorCredentialKind: store.InitiatorCredentialKindSession,
				AttributionVersion:      1, AuthorizationRevision: 1,
			},
		}
		require.NoError(t, s.CreateSchedule(ctx, sc))
		return sc
	}
	base := srv.store
	t.Cleanup(func() { srv.store = base })

	for _, tc := range []struct{ name, method, suffix string }{
		{"delete", http.MethodDelete, ""},
		{"pause", http.MethodPost, "/pause"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sc := mk(tc.name)
			reads := 0
			srv.store = reauthorizeOnFirstReadStore{Store: base, t: t, id: sc.ID, ownerID: ownerID, reads: &reads}
			defer func() { srv.store = base }()

			rec := doScheduleSessionRequest(t, srv, member, tc.method, projectID, sc.ID+tc.suffix)
			assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
			assert.Equal(t, 2, reads, "the creator check and the handler each read the schedule once")

			got, err := s.GetSchedule(ctx, sc.ID)
			require.NoError(t, err, "the schedule must not be deleted")
			assert.Equal(t, store.ScheduleStatusActive, got.Status, "the schedule must not be paused")
			assert.Equal(t, ownerID, got.InitiatorPrincipalID)
		})
	}
}
