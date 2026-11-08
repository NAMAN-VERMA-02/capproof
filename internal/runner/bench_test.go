package runner

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestBenchMeasuresFromTheSchedule(t *testing.T) {
	// One call at a time and 20ms each, at 100 calls a second: the target can't keep
	// up, so latency must grow with the queue. Timing from the actual send would not show it.
	slots := make(chan struct{}, 1)
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		slots <- struct{}{}
		time.Sleep(20 * time.Millisecond)
		<-slots
	}))
	defer gw.Close()
	rep, err := Bench(context.Background(), BenchOptions{Target: gw.URL, Rate: 100, Duration: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Sent != 100 || rep.Failed != 0 {
		t.Fatalf("%+v", rep)
	}
	if rep.MaxMS < 500 {
		t.Fatalf("max %.1fms hides the queue", rep.MaxMS)
	}
	if rep.P50MS > rep.P95MS || rep.P95MS > rep.P99MS || rep.P99MS > rep.MaxMS {
		t.Fatalf("percentiles out of order: %+v", rep)
	}
}

func TestBenchCountsFailures(t *testing.T) {
	var calls atomic.Int64
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1)%2 == 0 {
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer gw.Close()
	rep, err := Bench(context.Background(), BenchOptions{Target: gw.URL, Rate: 100, Duration: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Failed != 50 {
		t.Fatalf("failed = %d", rep.Failed)
	}
}

func TestBenchRefusesBadSettingsAndAllFailures(t *testing.T) {
	if _, err := Bench(context.Background(), BenchOptions{Target: "http://x", Rate: 0, Duration: time.Second}); err == nil {
		t.Fatal("zero rate accepted")
	}
	if _, err := Bench(context.Background(), BenchOptions{Target: "http://127.0.0.1:1", Rate: 10, Duration: time.Second, Timeout: time.Second}); err == nil {
		t.Fatal("a run with no successes must not report numbers")
	}
}
