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
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// agedUATStore reports every token as created age ago.
type agedUATStore struct {
	store.UserAccessTokenStore
	age time.Duration
}

func (a agedUATStore) GetUserAccessTokenByHash(ctx context.Context, hash string) (*store.UserAccessToken, error) {
	tok, err := a.UserAccessTokenStore.GetUserAccessTokenByHash(ctx, hash)
	if err == nil && tok != nil {
		tok.Created = tok.Created.Add(-a.age)
	}
	return tok, err
}

// TestUATIssuancePolicy_CreationDisabled: with token creation turned off,
// the service and the API refuse new tokens and existing tokens still work.
func TestUATIssuancePolicy_CreationDisabled(t *testing.T) {
	srv, s := testServer(t)
	adminID := tid("uatpolicy-disabled-admin")
	createTestUserWithRole(t, s, adminID, adminID+"@test.com", "admin", store.SystemRoleSuperAdmin)

	key, _, err := srv.uatService.CreateTokenWithParams(rs4MintContext(adminID), CreateTokenParams{
		UserID: adminID, Name: "uatpolicy-before", Boundary: TokenBoundary{Kind: BoundaryKindHub}, Scopes: []string{"agent:read"},
	})
	require.NoError(t, err)

	srv.uatService.SetIssuancePolicy(0, true)
	_, _, err = srv.uatService.CreateTokenWithParams(rs4MintContext(adminID), CreateTokenParams{
		UserID: adminID, Name: "uatpolicy-after", Boundary: TokenBoundary{Kind: BoundaryKindHub}, Scopes: []string{"agent:read"},
	})
	assert.ErrorIs(t, err, ErrUATCreationDisabled)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/auth/tokens", map[string]interface{}{
		"name": "uatpolicy-api", "boundary": map[string]string{"kind": "hub"}, "scopes": []string{"agent:read"},
	})
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "token_creation_disabled")

	_, err = srv.uatService.ValidateToken(context.Background(), key)
	assert.NoError(t, err, "an existing token keeps working when creation is disabled")
}

// TestUATIssuancePolicy_MaxLifetime: a lowered maximum refuses a longer
// explicit expiry, clamps the default expiry, and refuses at use a token
// older than the maximum.
func TestUATIssuancePolicy_MaxLifetime(t *testing.T) {
	srv, s := testServer(t)
	adminID := tid("uatpolicy-max-admin")
	createTestUserWithRole(t, s, adminID, adminID+"@test.com", "admin", store.SystemRoleSuperAdmin)
	const maxLifetime = 30 * 24 * time.Hour
	srv.uatService.SetIssuancePolicy(maxLifetime, false)
	now := time.Now()

	tooLong := now.Add(60 * 24 * time.Hour)
	_, _, err := srv.uatService.CreateTokenWithParams(rs4MintContext(adminID), CreateTokenParams{
		UserID: adminID, Name: "uatpolicy-long", Boundary: TokenBoundary{Kind: BoundaryKindHub}, Scopes: []string{"agent:read"}, ExpiresAt: &tooLong,
	})
	require.ErrorIs(t, err, ErrUATExpiryTooLong)
	assert.Contains(t, err.Error(), "30 days")

	key, tok, err := srv.uatService.CreateTokenWithParams(rs4MintContext(adminID), CreateTokenParams{
		UserID: adminID, Name: "uatpolicy-default", Boundary: TokenBoundary{Kind: BoundaryKindHub}, Scopes: []string{"agent:read"},
	})
	require.NoError(t, err)
	require.NotNil(t, tok.ExpiresAt)
	assert.WithinDuration(t, now.Add(maxLifetime), *tok.ExpiresAt, time.Minute,
		"the 90-day default expiry is clamped to the hub maximum")

	_, err = srv.uatService.ValidateToken(context.Background(), key)
	require.NoError(t, err, "a token within the maximum is accepted")

	srv.uatService.tokens = agedUATStore{UserAccessTokenStore: srv.uatService.tokens, age: maxLifetime + time.Hour}
	_, err = srv.uatService.ValidateToken(context.Background(), key)
	assert.ErrorIs(t, err, ErrUATExpired, "a token older than the hub maximum is refused at use")
}

// TestUATIssuancePolicy_DefaultUnchanged: with no policy set, the default
// expiry is 90 days, the maximum one year, and old tokens are not refused
// by age.
func TestUATIssuancePolicy_DefaultUnchanged(t *testing.T) {
	srv, s := testServer(t)
	adminID := tid("uatpolicy-default-admin")
	createTestUserWithRole(t, s, adminID, adminID+"@test.com", "admin", store.SystemRoleSuperAdmin)
	now := time.Now()

	key, tok, err := srv.uatService.CreateTokenWithParams(rs4MintContext(adminID), CreateTokenParams{
		UserID: adminID, Name: "uatpolicy-unset", Boundary: TokenBoundary{Kind: BoundaryKindHub}, Scopes: []string{"agent:read"},
	})
	require.NoError(t, err)
	assert.WithinDuration(t, now.Add(store.UATDefaultExpiry), *tok.ExpiresAt, time.Minute)

	almostYear := now.Add(store.UATMaxExpiry - time.Hour)
	_, _, err = srv.uatService.CreateTokenWithParams(rs4MintContext(adminID), CreateTokenParams{
		UserID: adminID, Name: "uatpolicy-year", Boundary: TokenBoundary{Kind: BoundaryKindHub}, Scopes: []string{"agent:read"}, ExpiresAt: &almostYear,
	})
	require.NoError(t, err)

	srv.uatService.tokens = agedUATStore{UserAccessTokenStore: srv.uatService.tokens, age: 400 * 24 * time.Hour}
	_, err = srv.uatService.ValidateToken(context.Background(), key)
	assert.NoError(t, err, "without a configured maximum, token age alone never refuses a token")
}
