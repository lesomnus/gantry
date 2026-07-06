package retention

import (
	"context"
	"testing"
	"time"

	"github.com/lesomnus/otx"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestManagerGauges(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { mp.Shutdown(context.Background()) })
	ctx, cancel := context.WithCancel(otx.Into(context.Background(), otx.New(otx.WithMeterProvider(mp))))
	t.Cleanup(cancel)

	ix := openTemp(t)
	_ = ix.Touch("d", "r/a:1", time.Now())
	_ = ix.Touch("d", "r/a:2", time.Now())
	_ = ix.Pin("d", "r/a:1", false)
	eng := &fakeEng{name: "d"}
	m := mgr1("d", eng, ix, Policy{}, Schedule{})
	m.StartWatchers(ctx)

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatal(err)
	}
	want := map[string]int64{"gantry.retention.records": 2, "gantry.retention.pins": 1}
	for name, want_val := range want {
		found := false
		for _, sm := range rm.ScopeMetrics {
			for _, mt := range sm.Metrics {
				if mt.Name != name {
					continue
				}
				g, ok := mt.Data.(metricdata.Gauge[int64])
				if !ok {
					t.Fatalf("%s data = %T, want int64 gauge", name, mt.Data)
				}
				for _, dp := range g.DataPoints {
					if v, ok := dp.Attributes.Value(attribute.Key("store")); ok && v.AsString() == "d" {
						found = true
						if dp.Value != want_val {
							t.Errorf("%s{store=d} = %d, want %d", name, dp.Value, want_val)
						}
					}
				}
			}
		}
		if !found {
			t.Errorf("%s{store=d} not observed", name)
		}
	}
}
