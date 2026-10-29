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
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// The Runtime Broker accepts an existing agent's hub credential on a start
// or restart only when the request classifies the resolved-env
// SCION_AUTH_TOKEN as secret-bootstrap (a30594a04). The hub must therefore
// send its env classifications with every start and restart, or each one
// fails with agent.ErrNoHubToken (ptone/scion#6663, ptone/scion#1350).

// TestDispatchStartAndRestartClassifyIssuedToken: start and restart send the
// token they issued together with a classification map that marks it
// secret-bootstrap.
func TestDispatchStartAndRestartClassifyIssuedToken(t *testing.T) {
	for _, tc := range runDispatches {
		if tc.name != "start" && tc.name != "restart" {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			f := newRunDispatchFixture(t)
			require.NoError(t, tc.dispatch(context.Background(), f))

			token, _, called := tc.sent(f)
			require.True(t, called)
			require.NotEmpty(t, token, "the hub issued a token for this %s", tc.name)

			extras := f.client.lastStartExtras
			if tc.name == "restart" {
				extras = f.client.lastRestartExtras
			}
			require.NotNil(t, extras.EnvClassifications, "the %s sends its env classifications", tc.name)
			assert.Equal(t, api.EnvKindSecretBootstrap, extras.EnvClassifications["SCION_AUTH_TOKEN"],
				"the issued token is classified secret-bootstrap, so the broker accepts it")
		})
	}
}

// TestBrokerHTTPTransport_SendsEnvClassifications: the HTTP transport puts
// StartExtras.EnvClassifications on the start and restart wire under
// envClassifications (shared applyStartExtras), and sends nothing for a nil
// map so an unavailable map stays distinct from an empty one.
func TestBrokerHTTPTransport_SendsEnvClassifications(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		gotBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	transport := newBrokerHTTPTransport(false, nil)
	env := map[string]string{"SCION_AUTH_TOKEN": "hub-issued"}
	classes := map[string]api.EnvKind{"SCION_AUTH_TOKEN": api.EnvKindSecretBootstrap}

	send := map[string]func(extras StartExtras) error{
		"start": func(extras StartExtras) error {
			_, err := transport.StartAgent(context.Background(), "broker-1", srv.URL, "agent-1", "project-1",
				"", "", "", "", "", "", env, nil, nil, nil, false, false, extras)
			return err
		},
		"restart": func(extras StartExtras) error {
			_, err := transport.RestartAgent(context.Background(), "broker-1", srv.URL, "agent-1", "project-1", env, extras)
			return err
		},
	}
	for name, do := range send {
		t.Run(name, func(t *testing.T) {
			require.NoError(t, do(StartExtras{EnvClassifications: classes}))
			var wire struct {
				EnvClassifications map[string]api.EnvKind `json:"envClassifications"`
			}
			require.NoError(t, json.Unmarshal(gotBody, &wire), "body %s", gotBody)
			assert.Equal(t, api.EnvKindSecretBootstrap, wire.EnvClassifications["SCION_AUTH_TOKEN"])

			require.NoError(t, do(StartExtras{}))
			var raw map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(gotBody, &raw), "body %s", gotBody)
			assert.NotContains(t, raw, "envClassifications", "a nil map sends nothing")
		})
	}
}
