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
	"fmt"
	"testing"
	"time"

	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"github.com/stretchr/testify/assert"
	"google.golang.org/genproto/googleapis/api/distribution"
	"google.golang.org/genproto/googleapis/api/metric"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var t0 = time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)

func intPoint(start, end time.Time, v int64) *monitoringpb.Point {
	return &monitoringpb.Point{
		Interval: &monitoringpb.TimeInterval{StartTime: timestamppb.New(start), EndTime: timestamppb.New(end)},
		Value:    &monitoringpb.TypedValue{Value: &monitoringpb.TypedValue_Int64Value{Int64Value: v}},
	}
}

func doublePoint(start, end time.Time, v float64) *monitoringpb.Point {
	return &monitoringpb.Point{
		Interval: &monitoringpb.TimeInterval{StartTime: timestamppb.New(start), EndTime: timestamppb.New(end)},
		Value:    &monitoringpb.TypedValue{Value: &monitoringpb.TypedValue_DoubleValue{DoubleValue: v}},
	}
}

func distPoint(start, end time.Time, count int64, mean float64) *monitoringpb.Point {
	return &monitoringpb.Point{
		Interval: &monitoringpb.TimeInterval{StartTime: timestamppb.New(start), EndTime: timestamppb.New(end)},
		Value: &monitoringpb.TypedValue{Value: &monitoringpb.TypedValue_DistributionValue{
			DistributionValue: &distribution.Distribution{Count: count, Mean: mean},
		}},
	}
}

func series(kind metric.MetricDescriptor_MetricKind, labels map[string]string, points ...*monitoringpb.Point) *monitoringpb.TimeSeries {
	return &monitoringpb.TimeSeries{MetricKind: kind, Metric: &metric.Metric{Labels: labels}, Points: points}
}

func total(incs []increment) float64 {
	var sum float64
	for _, i := range incs {
		sum += i.amount
	}
	return sum
}

func TestSeriesIncrements_CumulativeTakesLatestPerProcess(t *testing.T) {
	// Cloud Monitoring returns newest first; order must not matter.
	restart := t0.Add(2 * time.Hour)
	ts := series(metric.MetricDescriptor_CUMULATIVE, nil,
		intPoint(restart, restart.Add(time.Minute), 3),
		intPoint(t0, t0.Add(3*time.Minute), 12),
		intPoint(t0, t0.Add(time.Minute), 5),
		intPoint(t0, t0.Add(2*time.Minute), 8),
	)
	// Process 1 reached 12; process 2 reached 3. Summing every report would give 28.
	assert.Equal(t, 15.0, total(seriesIncrements(ts, measureValue)))
}

func TestSeriesIncrements_CumulativeResetWithinProcess(t *testing.T) {
	ts := series(metric.MetricDescriptor_CUMULATIVE, nil,
		intPoint(t0, t0.Add(time.Minute), 10),
		intPoint(t0, t0.Add(2*time.Minute), 4), // reset
	)
	assert.Equal(t, 14.0, total(seriesIncrements(ts, measureValue)))
}

func TestSeriesIncrements_DeltaSumsEveryPoint(t *testing.T) {
	ts := series(metric.MetricDescriptor_DELTA, nil,
		intPoint(t0, t0.Add(time.Minute), 2),
		intPoint(t0.Add(time.Minute), t0.Add(2*time.Minute), 3),
	)
	assert.Equal(t, 5.0, total(seriesIncrements(ts, measureValue)))
}

func TestSeriesIncrements_OnePointPerProcessMatchesPreviousBehaviour(t *testing.T) {
	// Short-lived hook processes: one cumulative point each, as before.
	ts := series(metric.MetricDescriptor_CUMULATIVE, nil,
		intPoint(t0, t0.Add(time.Second), 1),
		intPoint(t0.Add(time.Minute), t0.Add(time.Minute+time.Second), 1),
		intPoint(t0.Add(2*time.Minute), t0.Add(2*time.Minute+time.Second), 1),
	)
	assert.Equal(t, 3.0, total(seriesIncrements(ts, measureValue)))
}

func TestSeriesIncrements_DoubleValues(t *testing.T) {
	ts := series(metric.MetricDescriptor_CUMULATIVE, nil,
		doublePoint(t0, t0.Add(time.Minute), 120.5),
		doublePoint(t0, t0.Add(2*time.Minute), 300.25),
	)
	assert.InDelta(t, 300.25, total(seriesIncrements(ts, measureValue)), 1e-9)
}

func TestSeriesIncrements_DistributionSumAndCount(t *testing.T) {
	// Copilot gen_ai.client.token.usage: 4 calls averaging 250 tokens, then 6 averaging 300.
	ts := series(metric.MetricDescriptor_CUMULATIVE, nil,
		distPoint(t0, t0.Add(time.Minute), 4, 250),
		distPoint(t0, t0.Add(2*time.Minute), 6, 300),
	)
	assert.InDelta(t, 1800.0, total(seriesIncrements(ts, measureValue)), 1e-9, "sum = mean × count of the latest point")
	assert.Equal(t, 6.0, total(seriesIncrements(ts, measureCount)), "count = number of model calls")
}

func TestSeriesIncrements_CumulativeSplitsAcrossDays(t *testing.T) {
	day1 := time.Date(2026, 9, 24, 23, 0, 0, 0, time.UTC)
	ts := series(metric.MetricDescriptor_CUMULATIVE, nil,
		intPoint(day1, day1.Add(30*time.Minute), 10),
		intPoint(day1, day1.Add(2*time.Hour), 25), // next day
	)
	days := map[string]float64{}
	addToDays(days, seriesIncrements(ts, measureValue))
	assert.Equal(t, []TimeSeriesPoint{{Timestamp: "2026-09-24", Value: 10}, {Timestamp: "2026-09-25", Value: 15}}, sortedDayPoints(days))
}

func TestPointAmount_CountOfNonDistributionIsZero(t *testing.T) {
	assert.Equal(t, 0.0, pointAmount(intPoint(t0, t0, 7), measureCount))
}

func TestSeriesLabel_UsesAliasesThenKey(t *testing.T) {
	copilot := metricSource{labelAliases: copilotModelAliases}
	assert.Equal(t, "gpt-5", seriesLabel(series(metric.MetricDescriptor_CUMULATIVE,
		map[string]string{"gen_ai_request_model": "gpt-5"}), copilot, "model"))
	assert.Equal(t, "gpt-5.1", seriesLabel(series(metric.MetricDescriptor_CUMULATIVE,
		map[string]string{"gen_ai_request_model": "gpt-5", "gen_ai_response_model": "gpt-5.1"}), copilot, "model"))
	assert.Equal(t, "claude-x", seriesLabel(series(metric.MetricDescriptor_CUMULATIVE,
		map[string]string{"model": "claude-x"}), metricSource{}, "model"))
	assert.Equal(t, "(unknown)", seriesLabel(series(metric.MetricDescriptor_CUMULATIVE, nil), metricSource{}, "harness"))
}

func TestSourcesFor_TokenFiguresCoverEveryEmitter(t *testing.T) {
	names := func(logical string) []string {
		var out []string
		for _, s := range sourcesFor(logical) {
			out = append(out, s.name)
		}
		return out
	}
	assert.ElementsMatch(t, []string{"scion.hook.tokens.input", "gen_ai.tokens.input", "claude_code.token.usage", "gen_ai.client.token.usage"}, names(logicalTokensInput))
	assert.ElementsMatch(t, []string{"scion.hook.tokens.output", "gen_ai.tokens.output", "claude_code.token.usage", "gen_ai.client.token.usage"}, names(logicalTokensOutput))
	assert.ElementsMatch(t, []string{"gen_ai.api.calls", "gen_ai.client.token.usage"}, names(logicalAPICalls))
	assert.Equal(t, []metricSource{{name: "agent.session.count"}}, sourcesFor("agent.session.count"))
}

func TestSourceFilter(t *testing.T) {
	src := metricSource{name: "claude_code.token.usage", filter: `metric.labels.type = "input"`}
	assert.Equal(t,
		`metric.type = "workload.googleapis.com/claude_code.token.usage" AND metric.labels.type = "input" AND metric.labels.project_id = "p1"`,
		sourceFilter(src, []string{projectFilter("p1")}))
}

func TestIsAbsentMetricErr(t *testing.T) {
	assert.True(t, isAbsentMetricErr(status.Error(codes.NotFound, "no such metric")))
	assert.True(t, isAbsentMetricErr(status.Error(codes.InvalidArgument,
		`Cannot find metric(s) that match type = "workload.googleapis.com/x" label = "project_id"`)))
	assert.True(t, isAbsentMetricErr(fmt.Errorf("wrapped: %w", status.Error(codes.NotFound, "x"))))
	assert.False(t, isAbsentMetricErr(status.Error(codes.InvalidArgument, "bad filter syntax")))
	assert.False(t, isAbsentMetricErr(status.Error(codes.PermissionDenied, "denied")))
	assert.False(t, isAbsentMetricErr(nil))
}
