package metrics

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestWaitingRunRecordsResourcesWithoutSuccessOrFailure(t *testing.T) {
	recorder, collect := newTestRecorder(t)
	recorder.StageRun(context.Background(), StageAttrs{Stage: "work", Agent: "agent", Outcome: OutcomeWaiting}, time.Minute)
	recorder.Tokens(context.Background(), "work", "agent", TokenUsage{Input: 100, Output: 20})
	metrics := collect()
	runs := metrics["kontora.stage.runs"]
	require.Equal(t, "waiting", attrMap(t, runs)["outcome"])
	require.Equal(t, int64(1), runs.Data.(metricdata.Sum[int64]).DataPoints[0].Value)
	duration := metrics["kontora.stage.duration"]
	require.Equal(t, "waiting", attrMap(t, duration)["outcome"])
	require.Equal(t, float64(60), duration.Data.(metricdata.Histogram[float64]).DataPoints[0].Sum)
	require.Contains(t, metrics, "kontora.agent.tokens")
	require.NotContains(t, metrics, "kontora.agent.errors")
}
