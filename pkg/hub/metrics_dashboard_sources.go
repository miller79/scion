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
	"errors"
	"sort"
	"strings"
	"time"

	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"google.golang.org/genproto/googleapis/api/metric"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Dashboard figures are assembled from every metric that carries them, since
// harnesses report usage differently (miller79/scion#136):
//
//   - hook counters from sciontool (scion.hook.tokens.*, gen_ai.api.calls),
//     plus the pre-#1792 gen_ai.tokens.* names for older data;
//   - Claude Code's native claude_code.token.usage, split by "type";
//   - Copilot CLI's native OpenTelemetry GenAI histogram
//     gen_ai.client.token.usage, split by gen_ai.token.type. Its sum is the
//     token total and its count is one per model call (input observations).
//
// A logical name below maps to its sources; any other name is one metric.
const (
	logicalTokensInput  = "tokens.input"
	logicalTokensOutput = "tokens.output"
	logicalAPICalls     = "api.calls"
)

// pointMeasure selects what a point contributes: its value (a distribution's
// sum), or a distribution's count.
type pointMeasure int

const (
	measureValue pointMeasure = iota
	measureCount
)

// metricSource is one Cloud Monitoring metric contributing to a figure.
type metricSource struct {
	name    string       // metric name without metricPrefix
	filter  string       // extra filter clause, e.g. a token-type label
	measure pointMeasure // value (default) or distribution count
	// labelAliases maps a dashboard grouping key (e.g. "model") to the label
	// keys this metric uses for it, tried in order.
	labelAliases map[string][]string
}

var copilotModelAliases = map[string][]string{"model": {"gen_ai_response_model", "gen_ai_request_model"}}

var dashboardSources = map[string][]metricSource{
	logicalTokensInput: {
		{name: "scion.hook.tokens.input"},
		{name: "gen_ai.tokens.input"},
		{name: "claude_code.token.usage", filter: `metric.labels.type = "input"`},
		{name: "gen_ai.client.token.usage", filter: `metric.labels.gen_ai_token_type = "input"`, labelAliases: copilotModelAliases},
	},
	logicalTokensOutput: {
		{name: "scion.hook.tokens.output"},
		{name: "gen_ai.tokens.output"},
		{name: "claude_code.token.usage", filter: `metric.labels.type = "output"`},
		{name: "gen_ai.client.token.usage", filter: `metric.labels.gen_ai_token_type = "output"`, labelAliases: copilotModelAliases},
	},
	logicalAPICalls: {
		{name: "gen_ai.api.calls"},
		// One input-token observation per model call.
		{name: "gen_ai.client.token.usage", filter: `metric.labels.gen_ai_token_type = "input"`, measure: measureCount, labelAliases: copilotModelAliases},
	},
}

// sourcesFor returns the metrics behind a dashboard figure.
func sourcesFor(name string) []metricSource {
	if sources, ok := dashboardSources[name]; ok {
		return sources
	}
	return []metricSource{{name: name}}
}

// sourceFilter builds the Cloud Monitoring filter for one source.
func sourceFilter(src metricSource, extraFilter []string) string {
	filter := `metric.type = "` + metricPrefix + src.name + `"`
	if src.filter != "" {
		filter += " AND " + src.filter
	}
	for _, f := range extraFilter {
		filter += " AND " + f
	}
	return filter
}

// isAbsentMetricErr reports whether Cloud Monitoring rejected a query only
// because the metric type, or a label it filters on, does not exist in the
// project (e.g. no Copilot agent has reported yet, or a harness-native metric
// has no project_id label). Such a source contributes nothing.
func isAbsentMetricErr(err error) bool {
	if err == nil {
		return false
	}
	st, ok := status.FromError(err)
	if !ok {
		var se interface{ GRPCStatus() *status.Status }
		if errors.As(err, &se) {
			st = se.GRPCStatus()
		} else {
			return false
		}
	}
	switch st.Code() {
	case codes.NotFound:
		return true
	case codes.InvalidArgument:
		return strings.Contains(st.Message(), "Cannot find metric")
	}
	return false
}

// pointAmount returns what a single point contributes under a measure.
func pointAmount(p *monitoringpb.Point, m pointMeasure) float64 {
	v := p.GetValue()
	if d := v.GetDistributionValue(); d != nil {
		if m == measureCount {
			return float64(d.GetCount())
		}
		return d.GetMean() * float64(d.GetCount())
	}
	if m == measureCount {
		return 0
	}
	switch v.GetValue().(type) {
	case *monitoringpb.TypedValue_DoubleValue:
		return v.GetDoubleValue()
	default:
		return float64(v.GetInt64Value())
	}
}

// increment is an amount attributed to the time it was observed.
type increment struct {
	at     time.Time
	amount float64
}

// seriesIncrements turns one time series into the amounts it added.
//
// For CUMULATIVE series each point is a running total since its interval
// start, so points are grouped by start time (one group per emitting process)
// and each point contributes its growth over the previous point in the group;
// a drop is treated as a reset. Summing a group therefore yields its latest
// value rather than the sum of every report. Other kinds contribute each
// point's value as-is, which is also what a one-point-per-process cumulative
// series reduces to.
func seriesIncrements(ts *monitoringpb.TimeSeries, m pointMeasure) []increment {
	points := ts.GetPoints()
	if ts.GetMetricKind() != metric.MetricDescriptor_CUMULATIVE {
		out := make([]increment, 0, len(points))
		for _, p := range points {
			out = append(out, increment{at: p.GetInterval().GetEndTime().AsTime(), amount: pointAmount(p, m)})
		}
		return out
	}
	groups := map[int64][]*monitoringpb.Point{}
	for _, p := range points {
		start := p.GetInterval().GetStartTime().AsTime().UnixNano()
		groups[start] = append(groups[start], p)
	}
	var out []increment
	for _, group := range groups {
		sort.Slice(group, func(i, j int) bool {
			return group[i].GetInterval().GetEndTime().AsTime().Before(group[j].GetInterval().GetEndTime().AsTime())
		})
		prev := 0.0
		for _, p := range group {
			cur := pointAmount(p, m)
			delta := cur - prev
			if delta < 0 {
				delta = cur // counter reset
			}
			out = append(out, increment{at: p.GetInterval().GetEndTime().AsTime(), amount: delta})
			prev = cur
		}
	}
	return out
}

// seriesLabel returns the grouping value for a series, honouring a source's
// label aliases, or "(unknown)".
func seriesLabel(ts *monitoringpb.TimeSeries, src metricSource, labelKey string) string {
	labels := ts.GetMetric().GetLabels()
	keys := append(append([]string{}, src.labelAliases[labelKey]...), labelKey)
	for _, k := range keys {
		if v := labels[k]; v != "" {
			return v
		}
	}
	return "(unknown)"
}

// dayTotals accumulates rounded amounts per day.
func addToDays(days map[string]float64, incs []increment) {
	for _, inc := range incs {
		days[inc.at.Format("2006-01-02")] += inc.amount
	}
}

// sortedDayPoints converts per-day amounts to sorted, rounded points.
func sortedDayPoints(days map[string]float64) []TimeSeriesPoint {
	points := make([]TimeSeriesPoint, 0, len(days))
	for day, total := range days {
		points = append(points, TimeSeriesPoint{Timestamp: day, Value: roundAmount(total)})
	}
	sort.Slice(points, func(i, j int) bool { return points[i].Timestamp < points[j].Timestamp })
	return points
}

func roundAmount(v float64) int64 {
	if v < 0 {
		return int64(v - 0.5)
	}
	return int64(v + 0.5)
}
