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
	"fmt"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// reservedSAListPageSize is the page size reservedServiceAccountUse lists
// Runtime Brokers with.
const reservedSAListPageSize = 200

// reservedServiceAccountUse reports why email may not be registered as a
// project-scoped service account, or "" when it may. The Hub's own service
// account and the host service account of any Runtime Broker (configured or
// observed) are reserved: a project-scoped account is assignable to agents by
// the project's members, so registering one of these would hand the Hub's or
// a broker host's cloud identity to agents. They are refused even when the
// Hub can impersonate them, because that is exactly when assignment works.
func (s *Server) reservedServiceAccountUse(ctx context.Context, email string) (string, error) {
	email = strings.TrimSpace(email)
	if email == "" {
		return "", nil
	}
	s.mu.RLock()
	gen := s.gcpTokenGenerator
	s.mu.RUnlock()
	if gen != nil {
		if hubSA := strings.TrimSpace(gen.ServiceAccountEmail()); hubSA != "" && strings.EqualFold(hubSA, email) {
			return "it is the Hub's own service account", nil
		}
	}
	cursor := ""
	for {
		page, err := s.store.ListRuntimeBrokers(ctx, store.RuntimeBrokerFilter{}, store.ListOptions{
			Limit: reservedSAListPageSize, Cursor: cursor, SkipTotalCount: true,
		})
		if err != nil {
			return "", fmt.Errorf("listing runtime brokers: %w", err)
		}
		for i := range page.Items {
			b := &page.Items[i]
			if strings.EqualFold(strings.TrimSpace(b.GCPHostServiceAccountEmail), email) {
				return "it is a Runtime Broker's host service account", nil
			}
			if b.HostObservation != nil && strings.EqualFold(strings.TrimSpace(b.HostObservation.Email), email) {
				return "it is a Runtime Broker's host service account", nil
			}
		}
		if page.NextCursor == "" || page.NextCursor == cursor {
			return "", nil
		}
		cursor = page.NextCursor
	}
}
