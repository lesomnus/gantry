package warm

import (
	"context"
	"testing"
	"time"

	"github.com/lesomnus/otx"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// otxContext returns a ctx whose otx meter feeds the returned manual reader.
func otxContext(t *testing.T) (context.Context, *sdkmetric.ManualReader) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { mp.Shutdown(context.Background()) })
	return otx.Into(context.Background(), otx.New(otx.WithMeterProvider(mp))), reader
}

func gaugeValue(t *testing.T, rm metricdata.ResourceMetrics, name, attr_key, attr_val string) (int64, bool) {
	t.Helper()
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			g, ok := m.Data.(metricdata.Gauge[int64])
			if !ok {
				t.Fatalf("%s data = %T, want int64 gauge", name, m.Data)
			}
			for _, dp := range g.DataPoints {
				if attr_key == "" {
					return dp.Value, true
				}
				if v, ok := dp.Attributes.Value(attribute.Key(attr_key)); ok && v.AsString() == attr_val {
					return dp.Value, true
				}
			}
		}
	}
	return 0, false
}

func TestWarmerGauges(t *testing.T) {
	ctx, reader := otxContext(t)
	ctx, cancel := context.WithCancel(ctx)
	w, js := newWarmer(t, nil, true)
	w.Start(ctx)
	t.Cleanup(func() { cancel(); w.Stop() })

	done_job := NewJob("job_done", "a/b:1", nil, time.Now())
	done_job.State = JobDone
	done_job.DateEnded = time.Now()
	if err := js.Add(done_job); err != nil {
		t.Fatal(err)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatal(err)
	}
	if got, ok := gaugeValue(t, rm, "gantry.queue.capacity", "", ""); !ok || got != int64(cap(w.jobs)) {
		t.Errorf("queue.capacity = %d (found=%v), want %d", got, ok, cap(w.jobs))
	}
	if got, ok := gaugeValue(t, rm, "gantry.jobs", "state", "done"); !ok || got != 1 {
		t.Errorf("jobs{state=done} = %d (found=%v), want 1", got, ok)
	}
}
