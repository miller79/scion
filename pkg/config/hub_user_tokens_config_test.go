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
	"testing"
	"time"
)

func TestHubUserAccessTokensConfig_MaxLifetime(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{in: "", want: UserAccessTokenMaxLifetimeLimit},
		{in: "720h", want: 720 * time.Hour},
		{in: "1h", want: time.Hour},
		{in: "8760h", want: UserAccessTokenMaxLifetimeLimit},
		{in: "59m", wantErr: true},
		{in: "8761h", wantErr: true},
		{in: "30d", wantErr: true},
		{in: "-1h", wantErr: true},
	} {
		got, err := HubUserAccessTokensConfig{MaxLifetime: tc.in}.MaxLifetimeDuration()
		if (err != nil) != tc.wantErr {
			t.Errorf("MaxLifetimeDuration(%q) err = %v, wantErr %v", tc.in, err, tc.wantErr)
			continue
		}
		if !tc.wantErr && got != tc.want {
			t.Errorf("MaxLifetimeDuration(%q) = %s, want %s", tc.in, got, tc.want)
		}
	}
}

func TestHubUserAccessTokensConfig_V1RoundTrip(t *testing.T) {
	disabled := true
	gc := ConvertV1ServerToGlobalConfig(&V1ServerConfig{Hub: &V1ServerHubConfig{
		UserAccessTokens: &V1ServerHubUserAccessTokensConfig{MaxLifetime: "720h", CreationDisabled: &disabled},
	}})
	if got := gc.Hub.UserAccessTokens; got.MaxLifetime != "720h" || !got.CreationDisabled {
		t.Fatalf("Hub.UserAccessTokens = %+v", got)
	}
	v1 := ConvertGlobalToV1ServerConfig(gc)
	ut := v1.Hub.UserAccessTokens
	if ut == nil || ut.MaxLifetime != "720h" || ut.CreationDisabled == nil || !*ut.CreationDisabled {
		t.Fatalf("round trip lost server.hub.user_access_tokens: %+v", ut)
	}
}
