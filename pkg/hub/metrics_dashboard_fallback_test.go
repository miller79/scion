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
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/genproto/googleapis/api/distribution"
	googlemetricpb "google.golang.org/genproto/googleapis/api/metric"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// typeFilter is the filter string fetchTimeSeries builds for name and clauses.
func typeFilter(name string, clauses ...string) string {
	return strings.Join(append([]string{`metric.type = "` + metricPrefix + name + `"`}, clauses...), " AND ")
}

// cumulativeSeries is one CUMULATIVE series whose epoch began inside the
// window, reporting the given running totals an hour, then half an hour, ago.
func cumulativeSeries(name string, labels map[string]string, values ...*monitoringpb.TypedValue) *monitoringpb.TimeSeries {
	now := time.Now()
	epoch := now.Add(-2 * time.Hour)
	ts := &monitoringpb.TimeSeries{
		Metric:     &googlemetricpb.Metric{Type: metricPrefix + name, Labels: labels},
		MetricKind: googlemetricpb.MetricDescriptor_CUMULATIVE,
	}
	for i, v := range values {
		end := now.Add(-time.Hour + time.Duration(i)*30*time.Minute)
		ts.Points = append(ts.Points, &monitoringpb.Point{
			Interval: &monitoringpb.TimeInterval{StartTime: timestamppb.New(epoch), EndTime: timestamppb.New(end)},
			Value:    v,
		})
	}
	return ts
}

func intValue(v int64) *monitoringpb.TypedValue {
	return &monitoringpb.TypedValue{Value: &monitoringpb.TypedValue_Int64Value{Int64Value: v}}
}

func distValue(count int64, mean float64) *monitoringpb.TypedValue {
	return &monitoringpb.TypedValue{Value: &monitoringpb.TypedValue_DistributionValue{
		DistributionValue: &distribution.Distribution{Count: count, Mean: mean},
	}}
}

// TestDashboardFallbackReadsPreContractImages: agents on images that predate
// the usage contract export native metrics with project_id / agent_id labels
// only, and the summary still counts their calls, tokens and agents.
func TestDashboardFallbackReadsPreContractImages(t *testing.T) {
	client := newFakeMetricsClient()
	svc := newContractTestService(client)
	legacy := func(agent, harness string) map[string]string {
		return map[string]string{legacyAgentLabel: agent, legacyProjectLabel: "proj-1", "harness": harness}
	}

	client.seriesByFilter[typeFilter(telemetrycontract.MetricSessionCount)] = []*monitoringpb.TimeSeries{
		cumulativeSeries(telemetrycontract.MetricSessionCount, legacy("claude-a", "claude"), intValue(1)),
		cumulativeSeries(telemetrycontract.MetricSessionCount, legacy("copilot-b", "copilot"), intValue(1)),
	}
	// Claude Code: 100 then 150 input tokens, 20 output; 3 model requests.
	client.seriesByFilter[typeFilter("claude_code.token.usage")] = []*monitoringpb.TimeSeries{
		cumulativeSeries("claude_code.token.usage", map[string]string{legacyAgentLabel: "claude-a", "type": "input"}, intValue(100), intValue(150)),
		cumulativeSeries("claude_code.token.usage", map[string]string{legacyAgentLabel: "claude-a", "type": "output"}, intValue(20)),
	}
	client.seriesByFilter[typeFilter("claude_code.api_request.count")] = []*monitoringpb.TimeSeries{
		cumulativeSeries("claude_code.api_request.count", map[string]string{legacyAgentLabel: "claude-a"}, intValue(3)),
	}
	// Copilot: input histogram 4 calls x 250 tokens, output 4 x 50.
	copilotIn := cumulativeSeries("gen_ai.client.token.usage", map[string]string{legacyAgentLabel: "copilot-b", "gen_ai_token_type": "input"}, distValue(4, 250))
	copilotOut := cumulativeSeries("gen_ai.client.token.usage", map[string]string{legacyAgentLabel: "copilot-b", "gen_ai_token_type": "output"}, distValue(4, 50))
	client.seriesByFilter[typeFilter("gen_ai.client.token.usage")] = []*monitoringpb.TimeSeries{copilotIn, copilotOut}
	client.seriesByFilter[typeFilter("gen_ai.client.token.usage", `metric.labels.gen_ai_token_type = "input"`)] = []*monitoringpb.TimeSeries{copilotIn}

	summary, err := svc.QuerySummary(context.Background(), 7)
	require.NoError(t, err)
	assert.Equal(t, int64(2), summary.TotalSessions)
	assert.Equal(t, int64(3+4), summary.TotalAPICalls, "Claude requests + one Copilot input observation per call")
	assert.Equal(t, int64(150+20+1000+200), summary.TotalTokens)
	assert.Equal(t, 2, summary.UniqueAgents, "agents identified by the pre-contract agent_id label")
}

// TestDashboardFallbackCanonicalWinsPerAgent: an agent that exports both the
// canonical counter and its native metric is counted once, from the
// canonical series; an agent with only native data is still counted.
func TestDashboardFallbackCanonicalWinsPerAgent(t *testing.T) {
	client := newFakeMetricsClient()
	svc := newContractTestService(client)

	client.seriesByFilter[typeFilter(telemetrycontract.MetricUsageTokens, notSummableTokenTypeFilter())] = []*monitoringpb.TimeSeries{
		cumulativeSeries(telemetrycontract.MetricUsageTokens, map[string]string{telemetrycontract.AgentLabel: "new-image", telemetrycontract.TokenTypeLabel: "input"}, intValue(500)),
	}
	client.seriesByFilter[typeFilter("claude_code.token.usage")] = []*monitoringpb.TimeSeries{
		cumulativeSeries("claude_code.token.usage", map[string]string{telemetrycontract.AgentLabel: "new-image", "type": "input"}, intValue(500)),
		cumulativeSeries("claude_code.token.usage", map[string]string{legacyAgentLabel: "old-image", "type": "input"}, intValue(70)),
	}

	summary, err := svc.QuerySummary(context.Background(), 7)
	require.NoError(t, err)
	assert.Equal(t, int64(500+70), summary.TotalTokens)
}

// TestDashboardFallbackProjectScope: a project-scoped view reads legacy-only
// agent.session.count series in a second pass, skipping any that carry the
// canonical label (the canonical query already matched or excluded those),
// and reads native metrics by either project label without counting a
// series twice.
func TestDashboardFallbackProjectScope(t *testing.T) {
	client := newFakeMetricsClient()
	svc := newContractTestService(client)

	client.seriesByFilter[typeFilter(telemetrycontract.MetricSessionCount, projectFilter("proj-1"))] = []*monitoringpb.TimeSeries{
		cumulativeSeries(telemetrycontract.MetricSessionCount, map[string]string{telemetrycontract.AgentLabel: "new", telemetrycontract.ProjectLabel: "proj-1"}, intValue(1)),
	}
	client.seriesByFilter[typeFilter(telemetrycontract.MetricSessionCount, `metric.labels.project_id = "proj-1"`)] = []*monitoringpb.TimeSeries{
		cumulativeSeries(telemetrycontract.MetricSessionCount, map[string]string{telemetrycontract.AgentLabel: "new", telemetrycontract.ProjectLabel: "proj-1", legacyProjectLabel: "proj-1"}, intValue(1)),
		cumulativeSeries(telemetrycontract.MetricSessionCount, map[string]string{legacyAgentLabel: "old", legacyProjectLabel: "proj-1"}, intValue(1)),
	}
	// A new-image native series carries both labels, so the legacy-label
	// query returns it again; it must be counted once.
	newNative := cumulativeSeries("claude_code.token.usage", map[string]string{telemetrycontract.AgentLabel: "new", telemetrycontract.ProjectLabel: "proj-1", legacyProjectLabel: "proj-1", "type": "input"}, intValue(5))
	client.seriesByFilter[typeFilter("claude_code.token.usage", projectFilter("proj-1"))] = []*monitoringpb.TimeSeries{newNative}
	client.seriesByFilter[typeFilter("claude_code.token.usage", legacyProjectFilter("proj-1"))] = []*monitoringpb.TimeSeries{
		newNative,
		cumulativeSeries("claude_code.token.usage", map[string]string{legacyAgentLabel: "old", legacyProjectLabel: "proj-1", "type": "output"}, intValue(40)),
	}

	summary, err := svc.QuerySummary(context.Background(), 7, WithProjectID("proj-1"))
	require.NoError(t, err)
	assert.Equal(t, int64(2), summary.TotalSessions, "canonical series once, plus the legacy-only one")
	assert.Equal(t, 2, summary.UniqueAgents)
	assert.Equal(t, int64(45), summary.TotalTokens, "each native series once")

	project, err := svc.QueryProjectSummary(context.Background(), "proj-1")
	require.NoError(t, err)
	assert.Equal(t, int64(2), project.SessionsCount24h)
	assert.Equal(t, 2, project.ActiveAgents24h)
	assert.Equal(t, int64(45), project.TokenUsage24h)
}

// TestDashboardFallbackGroupsByAliasedModel: Copilot's model comes from its
// GenAI response/request model label.
func TestDashboardFallbackGroupsByAliasedModel(t *testing.T) {
	client := newFakeMetricsClient()
	svc := newContractTestService(client)
	client.seriesByFilter[typeFilter("gen_ai.client.token.usage", `metric.labels.gen_ai_token_type = "input"`)] = []*monitoringpb.TimeSeries{
		cumulativeSeries("gen_ai.client.token.usage", map[string]string{legacyAgentLabel: "c", "gen_ai_token_type": "input", "gen_ai_request_model": "req-model", "gen_ai_response_model": "resp-model"}, distValue(2, 10)),
	}

	calls, err := svc.QueryModelCalls(context.Background(), 7)
	require.NoError(t, err)
	require.Len(t, calls.ByModel, 1)
	assert.Equal(t, "resp-model", calls.ByModel[0].Label)
	assert.Equal(t, int64(2), calls.ByModel[0].Points[0].Value)

	tokens, err := svc.QueryTokens(context.Background(), 7)
	require.NoError(t, err)
	require.Len(t, tokens.Input, 1)
	assert.Equal(t, int64(20), tokens.Input[0].Points[0].Value)
}

func TestMeasuredValue(t *testing.T) {
	v, ok := measuredValue(distValue(4, 250), measureDistributionSum)
	assert.True(t, ok)
	assert.Equal(t, int64(1000), v)
	v, ok = measuredValue(distValue(4, 250), measureDistributionCount)
	assert.True(t, ok)
	assert.Equal(t, int64(4), v)
	_, ok = measuredValue(intValue(3), measureDistributionSum)
	assert.False(t, ok, "a non-distribution point has no distribution sum")
	v, ok = measuredValue(intValue(3), measureValue)
	assert.True(t, ok)
	assert.Equal(t, int64(3), v)
}

func TestTokenFallbacks(t *testing.T) {
	filters := func(sources []fallbackSource) []string {
		var out []string
		for _, s := range sources {
			out = append(out, s.name+"|"+s.filter)
		}
		return out
	}
	assert.Equal(t, []string{"claude_code.token.usage|", "gen_ai.client.token.usage|"}, filters(tokenFallbacks("")))
	assert.Equal(t, []string{
		`claude_code.token.usage|metric.labels.type = "input"`,
		`gen_ai.client.token.usage|metric.labels.gen_ai_token_type = "input"`,
	}, filters(tokenFallbacks(telemetrycontract.TokenTypeInput)))
	assert.Equal(t, []string{`claude_code.token.usage|metric.labels.type = "cacheCreation"`},
		filters(tokenFallbacks(telemetrycontract.TokenTypeCacheWrite)), "Copilot reports no cache split")
	assert.Empty(t, tokenFallbacks(telemetrycontract.TokenTypeReasoning))
}
