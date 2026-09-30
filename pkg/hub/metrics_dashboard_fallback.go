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
	"math"
	"time"

	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
)

// Pre-contract fallbacks.
//
// The dashboard's figures come from the canonical usage contract
// (telemetrycontract). Agents on harness images that predate it export the
// same usage in other shapes, and Copilot has no canonical usage source at
// all, so each figure also reads those shapes as fallbacks:
//
//   - Claude Code's native claude_code.token.usage, split by "type";
//   - Copilot CLI's native OpenTelemetry GenAI histogram
//     gen_ai.client.token.usage, split by gen_ai_token_type: its sum is the
//     token total and its count is one per model call (input observations);
//   - claude_code.api_request.count, which a collector can derive from
//     Claude Code's api_request log events;
//   - the identity labels project_id / agent_id, which older images set
//     instead of scion_project_id / scion_agent_id.
//
// Canonical data always wins: a fallback series is ignored for any agent
// whose canonical series for the same figure is present, so an agent that
// exports both is counted once.

// pointMeasure selects what a point contributes: its numeric value, or a
// distribution's sum or count.
type pointMeasure int

const (
	measureValue pointMeasure = iota
	measureDistributionSum
	measureDistributionCount
)

// Pre-contract identity labels, read when a series lacks the canonical one.
const (
	legacyProjectLabel = "project_id"
	legacyAgentLabel   = "agent_id"
)

// fallbackSource is a pre-contract metric that carries (part of) a figure.
type fallbackSource struct {
	name    string       // metric name without metricPrefix
	filter  string       // extra filter clause, e.g. its own token-type label
	measure pointMeasure // what each point contributes
	// labelAliases maps a canonical label key (e.g. "model") to the keys
	// this metric uses for it, tried in order.
	labelAliases map[string][]string
}

var copilotModelAliases = map[string][]string{"model": {"gen_ai_response_model", "gen_ai_request_model"}}

// claudeTokenTypes maps contract token types to claude_code.token.usage's
// "type" label. Every Claude type is summable.
var claudeTokenTypes = map[string]string{
	telemetrycontract.TokenTypeInput:      "input",
	telemetrycontract.TokenTypeOutput:     "output",
	telemetrycontract.TokenTypeCacheRead:  "cacheRead",
	telemetrycontract.TokenTypeCacheWrite: "cacheCreation",
}

// copilotTokenTypes maps contract token types to gen_ai.client.token.usage's
// gen_ai_token_type label. Copilot reports no cache or reasoning split.
var copilotTokenTypes = map[string]string{
	telemetrycontract.TokenTypeInput:  "input",
	telemetrycontract.TokenTypeOutput: "output",
}

// apiCallFallbacks returns the pre-contract sources of gen_ai.api.calls.
func apiCallFallbacks() []fallbackSource {
	return []fallbackSource{
		{name: "claude_code.api_request.count"},
		{name: "gen_ai.client.token.usage", filter: `metric.labels.gen_ai_token_type = "input"`, measure: measureDistributionCount, labelAliases: copilotModelAliases},
	}
}

// tokenFallbacks returns the pre-contract sources of scion.usage.tokens for
// one token type, or for every summable type when tokenType is "".
func tokenFallbacks(tokenType string) []fallbackSource {
	var out []fallbackSource
	claude := fallbackSource{name: "claude_code.token.usage"}
	copilot := fallbackSource{name: "gen_ai.client.token.usage", measure: measureDistributionSum, labelAliases: copilotModelAliases}
	if tokenType == "" {
		return append(out, claude, copilot)
	}
	if t, ok := claudeTokenTypes[tokenType]; ok {
		claude.filter = fmt.Sprintf(`metric.labels.type = "%s"`, t)
		out = append(out, claude)
	}
	if t, ok := copilotTokenTypes[tokenType]; ok {
		copilot.filter = fmt.Sprintf(`metric.labels.gen_ai_token_type = "%s"`, t)
		out = append(out, copilot)
	}
	return out
}

// usageQuery is one dashboard figure: its canonical metric and filter, and
// the pre-contract series that carry the same figure.
type usageQuery struct {
	metric string   // canonical metric name
	filter []string // canonical filter clauses (project scope, token type)
	// projectID scopes the fallbacks; "" is a global query.
	projectID string
	// legacyIdentity also reads the canonical metric's series that carry
	// only legacyProjectLabel, for project-scoped queries.
	legacyIdentity bool
	fallbacks      []fallbackSource
}

// canonicalUsageQuery is a query for metric with no pre-contract fallbacks.
func canonicalUsageQuery(metric string, filter []string) usageQuery {
	return usageQuery{metric: metric, filter: filter}
}

// sessionQuery counts agent.session.count, whose name predates the
// contract, so only its identity labels need a fallback.
func sessionQuery(w metricsQueryWindow) usageQuery {
	return usageQuery{metric: telemetrycontract.MetricSessionCount, filter: w.extraFilter, projectID: w.projectID, legacyIdentity: true}
}

// apiCallsQuery counts model calls.
func apiCallsQuery(w metricsQueryWindow) usageQuery {
	return usageQuery{metric: telemetrycontract.MetricAPICalls, filter: w.extraFilter, projectID: w.projectID, fallbacks: apiCallFallbacks()}
}

// tokensQuery sums tokens of one type, or every summable type when tokenType
// is "" (reasoning is an informational subset of output and is excluded).
func tokensQuery(w metricsQueryWindow, tokenType string) usageQuery {
	typeFilter := notSummableTokenTypeFilter()
	if tokenType != "" {
		typeFilter = tokenTypeFilter(tokenType)
	}
	filter := append(append([]string{}, w.extraFilter...), typeFilter)
	return usageQuery{metric: telemetrycontract.MetricUsageTokens, filter: filter, projectID: w.projectID, fallbacks: tokenFallbacks(tokenType)}
}

// sourcedSeries is a fetched series with the source it came from.
type sourcedSeries struct {
	ts           *monitoringpb.TimeSeries
	measure      pointMeasure
	labelAliases map[string][]string
}

// label returns the series' value for a canonical label key, falling back
// to its source's aliases and then the pre-contract identity labels.
func (ss sourcedSeries) label(key string) string {
	labels := ss.ts.GetMetric().GetLabels()
	if v := labels[key]; v != "" {
		return v
	}
	for _, k := range ss.labelAliases[key] {
		if v := labels[k]; v != "" {
			return v
		}
	}
	switch key {
	case telemetrycontract.AgentLabel:
		return labels[legacyAgentLabel]
	case telemetrycontract.ProjectLabel:
		return labels[legacyProjectLabel]
	}
	return ""
}

// increases returns the series' per-flush increments (see seriesIncreases).
func (ss sourcedSeries) increases(fetchStart time.Time) []seriesIncrement {
	return seriesIncreasesBy(ss.ts.GetPoints(), fetchStart, func(v *monitoringpb.TypedValue) (int64, bool) {
		return measuredValue(v, ss.measure)
	})
}

// measuredValue reads what a point contributes under m.
func measuredValue(v *monitoringpb.TypedValue, m pointMeasure) (int64, bool) {
	switch m {
	case measureDistributionSum, measureDistributionCount:
		d := v.GetDistributionValue()
		if d == nil {
			return 0, false
		}
		if m == measureDistributionCount {
			return d.GetCount(), true
		}
		return int64(math.Round(d.GetMean() * float64(d.GetCount()))), true
	default:
		return pointValue(v)
	}
}

// legacyProjectFilter matches a project on the pre-contract project label.
// Cloud Monitoring rejects an OR across labels combined with another label
// clause, so the canonical and pre-contract labels are queried separately.
func legacyProjectFilter(projectID string) string {
	return fmt.Sprintf(`metric.labels.%s = "%s"`, legacyProjectLabel, projectID)
}

// fetchProjectScoped lists a metric's series for a project: those with the
// canonical project label, then those identified only by the pre-contract
// label. A series with the canonical label is never taken from the second
// query (the first already matched it, or it belongs to another project).
// Without a project it is one unfiltered query.
func (s *MetricsDashboardService) fetchProjectScoped(ctx context.Context, metric, projectID string, fetchStart, end time.Time, extra []string, includeCanonical bool) ([]*monitoringpb.TimeSeries, error) {
	if projectID == "" {
		return s.fetchTimeSeries(ctx, metric, fetchStart, end, extra)
	}
	var out []*monitoringpb.TimeSeries
	if includeCanonical {
		canonical, err := s.fetchTimeSeries(ctx, metric, fetchStart, end, append([]string{projectFilter(projectID)}, extra...))
		if err != nil {
			return nil, err
		}
		out = append(out, canonical...)
	}
	legacy, err := s.fetchTimeSeries(ctx, metric, fetchStart, end, append([]string{legacyProjectFilter(projectID)}, extra...))
	if err != nil {
		return nil, err
	}
	for _, ts := range legacy {
		if ts.GetMetric().GetLabels()[telemetrycontract.ProjectLabel] != "" {
			continue
		}
		out = append(out, ts)
	}
	return out, nil
}

// fetchUsageSeries lists every series behind q: the canonical series, then
// (for a project-scoped query) canonical-metric series identified only by
// the pre-contract project label, then each fallback's series (by either
// project label) for agents with no canonical series.
func (s *MetricsDashboardService) fetchUsageSeries(ctx context.Context, q usageQuery, fetchStart, end time.Time) ([]sourcedSeries, error) {
	canonical, err := s.fetchTimeSeries(ctx, q.metric, fetchStart, end, q.filter)
	if err != nil {
		return nil, err
	}
	out := make([]sourcedSeries, 0, len(canonical))
	canonicalAgents := make(map[string]bool)
	for _, ts := range canonical {
		ss := sourcedSeries{ts: ts}
		out = append(out, ss)
		if a := ss.label(telemetrycontract.AgentLabel); a != "" {
			canonicalAgents[a] = true
		}
	}

	if q.legacyIdentity && q.projectID != "" {
		// The canonical query above already covered the canonical label.
		legacy, err := s.fetchProjectScoped(ctx, q.metric, q.projectID, fetchStart, end, nil, false)
		if err != nil {
			return nil, err
		}
		for _, ts := range legacy {
			out = append(out, sourcedSeries{ts: ts})
		}
	}

	for _, fb := range q.fallbacks {
		var extra []string
		if fb.filter != "" {
			extra = append(extra, fb.filter)
		}
		series, err := s.fetchProjectScoped(ctx, fb.name, q.projectID, fetchStart, end, extra, true)
		if err != nil {
			return nil, err
		}
		for _, ts := range series {
			ss := sourcedSeries{ts: ts, measure: fb.measure, labelAliases: fb.labelAliases}
			if a := ss.label(telemetrycontract.AgentLabel); a != "" && canonicalAgents[a] {
				continue
			}
			out = append(out, ss)
		}
	}
	return out, nil
}
