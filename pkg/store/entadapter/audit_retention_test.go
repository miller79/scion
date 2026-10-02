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

package entadapter

import (
	"context"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withAuditDeleteBatchSize shrinks the delete batch so a handful of rows
// crosses several batches.
func withAuditDeleteBatchSize(t *testing.T, n int) {
	t.Helper()
	prev := auditDeleteBatchSize
	auditDeleteBatchSize = n
	t.Cleanup(func() { auditDeleteBatchSize = prev })
}

func TestDeleteDecisionAuditsBefore_Batches(t *testing.T) {
	withAuditDeleteBatchSize(t, 3)
	das := NewDecisionAuditStore(enttest.NewClient(t))
	ctx := context.Background()
	cutoff := time.Now().Add(-24 * time.Hour)

	add := func(ts time.Time, n int) {
		for i := 0; i < n; i++ {
			require.NoError(t, das.CreateDecisionAudit(ctx, &store.DecisionAuditRecord{
				Timestamp: ts, PrincipalKind: "user", PrincipalID: "u1",
				ResourceType: "project", Permission: "read", Result: "deny", Reason: "test",
			}))
		}
	}
	// 7 old rows span three batches of 3 (3, 3, then a short batch of 1).
	add(cutoff.Add(-time.Hour), 7)
	add(time.Now(), 2)

	deleted, err := das.DeleteDecisionAuditsBefore(ctx, cutoff)
	require.NoError(t, err)
	assert.Equal(t, 7, deleted)

	_, remaining, err := das.ListDecisionAudits(ctx, store.DecisionAuditFilter{Limit: 100})
	require.NoError(t, err)
	assert.Equal(t, 2, remaining)

	// An exact multiple of the batch size still terminates.
	add(cutoff.Add(-time.Hour), 6)
	deleted, err = das.DeleteDecisionAuditsBefore(ctx, cutoff)
	require.NoError(t, err)
	assert.Equal(t, 6, deleted)
}

func TestDeleteDecisionAuditsBefore_StopsWhenContextEnds(t *testing.T) {
	withAuditDeleteBatchSize(t, 2)
	das := NewDecisionAuditStore(enttest.NewClient(t))
	ctx := context.Background()
	old := time.Now().Add(-48 * time.Hour)
	for i := 0; i < 4; i++ {
		require.NoError(t, das.CreateDecisionAudit(ctx, &store.DecisionAuditRecord{
			Timestamp: old, PrincipalKind: "user", PrincipalID: "u1",
			ResourceType: "project", Permission: "read", Result: "deny", Reason: "test",
		}))
	}

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	_, err := das.DeleteDecisionAuditsBefore(cancelled, time.Now())
	require.Error(t, err)

	// Nothing was lost: a later run deletes the rest.
	deleted, err := das.DeleteDecisionAuditsBefore(ctx, time.Now())
	require.NoError(t, err)
	assert.Equal(t, 4, deleted)
}

func TestDeleteMutationAuditsBefore_Batches(t *testing.T) {
	withAuditDeleteBatchSize(t, 2)
	mas := NewMutationAuditStore(enttest.NewClient(t))
	ctx := context.Background()
	cutoff := time.Now().Add(-24 * time.Hour)

	add := func(ts time.Time, n int) {
		for i := 0; i < n; i++ {
			require.NoError(t, mas.CreateMutationAudit(ctx, &store.MutationAuditRecord{
				Timestamp: ts, MutationType: "policy_create", ActorPrincipalKind: "user",
				ActorPrincipalID: "u1", TargetType: "policy", TargetID: "p1",
			}))
		}
	}
	add(cutoff.Add(-time.Hour), 5)
	add(time.Now(), 1)

	deleted, err := mas.DeleteMutationAuditsBefore(ctx, cutoff)
	require.NoError(t, err)
	assert.Equal(t, 5, deleted)

	_, remaining, err := mas.ListMutationAudits(ctx, store.MutationAuditFilter{Limit: 100})
	require.NoError(t, err)
	assert.Equal(t, 1, remaining)
}
