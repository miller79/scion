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

package runtimebroker

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestStartAgentPassesNoAuth: the Hub's noAuth on a start reaches the start
// options, so the agent starts in the harness's no-auth mode as it did on
// create instead of running the harness with no credentials.
func TestStartAgentPassesNoAuth(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want bool
	}{
		{name: "noAuth set", body: `{"noAuth": true}`, want: true},
		{name: "noAuth absent", body: `{}`, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, mgr := newTestServerWithProvisionCapture()

			req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/noauth-agent/start", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()

			srv.Handler().ServeHTTP(w, req)

			if w.Code != http.StatusAccepted {
				t.Fatalf("expected status %d, got %d: %s", http.StatusAccepted, w.Code, w.Body.String())
			}
			if !mgr.startCalled {
				t.Fatal("expected Start to be called")
			}
			if mgr.lastOpts.NoAuth != tc.want {
				t.Errorf("StartOptions.NoAuth = %v, want %v", mgr.lastOpts.NoAuth, tc.want)
			}
		})
	}
}

// TestRestartAgentPassesNoAuth: the restart path carries the Hub's noAuth
// to the start options exactly as start does.
func TestRestartAgentPassesNoAuth(t *testing.T) {
	srv, mgr := newTestServerWithProvisionCapture()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/noauth-agent/restart", strings.NewReader(`{"noAuth": true}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if !mgr.startCalled {
		t.Fatalf("expected Start to be called (status %d: %s)", w.Code, w.Body.String())
	}
	if !mgr.lastOpts.NoAuth {
		t.Error("StartOptions.NoAuth = false on restart, want true")
	}
}
