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

package config

import (
	"fmt"
	"time"
)

const (
	// UserAccessTokenMaxLifetimeLimit is the longest lifetime a user access
	// token can have, and the default of
	// server.hub.user_access_tokens.max_lifetime (store.UATMaxExpiry).
	UserAccessTokenMaxLifetimeLimit = 365 * 24 * time.Hour

	// UserAccessTokenMinLifetimeSetting is the shortest value
	// server.hub.user_access_tokens.max_lifetime accepts.
	UserAccessTokenMinLifetimeSetting = time.Hour
)

// HubUserAccessTokensConfig holds the hub's user access token issuance
// policy (server.hub.user_access_tokens). Read at startup.
type HubUserAccessTokensConfig struct {
	// MaxLifetime is the longest lifetime a new token may be given, as a Go
	// duration ("" = UserAccessTokenMaxLifetimeLimit, one year; between
	// UserAccessTokenMinLifetimeSetting and that limit). A request for a
	// longer expiry is refused; a request with no expiry gets the default
	// (90 days) or MaxLifetime, whichever is shorter. Existing tokens older
	// than MaxLifetime are refused at use.
	MaxLifetime string `json:"maxLifetime,omitempty" yaml:"maxLifetime,omitempty" koanf:"maxLifetime"`

	// CreationDisabled refuses the creation of new tokens. Existing tokens
	// keep working until they expire or are revoked.
	CreationDisabled bool `json:"creationDisabled,omitempty" yaml:"creationDisabled,omitempty" koanf:"creationDisabled"`
}

// IsZero reports whether no user access token setting is configured.
func (c HubUserAccessTokensConfig) IsZero() bool {
	return c.MaxLifetime == "" && !c.CreationDisabled
}

// MaxLifetimeDuration parses MaxLifetime ("" = UserAccessTokenMaxLifetimeLimit).
func (c HubUserAccessTokensConfig) MaxLifetimeDuration() (time.Duration, error) {
	if c.MaxLifetime == "" {
		return UserAccessTokenMaxLifetimeLimit, nil
	}
	d, err := time.ParseDuration(c.MaxLifetime)
	if err != nil {
		return 0, fmt.Errorf("invalid server.hub.user_access_tokens.max_lifetime %q: %w", c.MaxLifetime, err)
	}
	if d < UserAccessTokenMinLifetimeSetting || d > UserAccessTokenMaxLifetimeLimit {
		return 0, fmt.Errorf("invalid server.hub.user_access_tokens.max_lifetime %q: must be between %s and %s",
			c.MaxLifetime, UserAccessTokenMinLifetimeSetting, UserAccessTokenMaxLifetimeLimit)
	}
	return d, nil
}

// Validate checks every user access token setting.
func (c HubUserAccessTokensConfig) Validate() error {
	_, err := c.MaxLifetimeDuration()
	return err
}
