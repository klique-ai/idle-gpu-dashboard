package health

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/klique-ai/idle-gpu-dashboard/collector/internal/collector"
	"github.com/klique-ai/idle-gpu-dashboard/collector/internal/report"
	"github.com/klique-ai/idle-gpu-dashboard/collector/internal/sender"
)

type fakeSource struct {
	last       *report.Report
	lastSample time.Time
}

func (f fakeSource) Status() collector.Status {
	return collector.Status{LastSample: f.lastSample}
}
func (f fakeSource) LastReport() *report.Report { return f.last }

type fakeSender struct{}

func (fakeSender) Enqueue(*report.Report) {}
func (fakeSender) Run(context.Context)    {}
func (fakeSender) Stats() sender.Stats    { return sender.Stats{Pending: 2, Dropped: 1} }

func get(t *testing.T, s *Server, path string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	s.srv.Handler.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	if rec.Code != 200 {
		t.Fatalf("%s: HTTP %d", path, rec.Code)
	}
	return rec.Body.String()
}

func TestMetrics(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Before the first report: only the delivery counters.
	s := New("x", fakeSource{lastSample: time.Now()}, fakeSender{}, time.Minute, quiet)
	if out := get(t, s, "/metrics"); strings.Contains(out, "klique_idle_gpu_") || !strings.Contains(out, "klique_idle_collector_reports_pending 2\n") {
		t.Errorf("before first report:\n%s", out)
	}

	r := &report.Report{Cluster: "c", Node: report.Node{Name: "n1"},
		GPUs: []report.GPU{{Index: 0, UUID: "GPU-a", Name: "NVIDIA H100", MemoryTotalBytes: 80 << 30, UtilPercent: &report.Stat{Avg: 50, Max: 90}}}}
	s = New("x", fakeSource{last: r, lastSample: time.Now()}, fakeSender{}, time.Minute, quiet)
	out := get(t, s, "/metrics")
	for _, want := range []string{
		`klique_idle_gpu_utilization_max_percent{cluster="c",node="n1",gpu="0",uuid="GPU-a",model="NVIDIA H100"} 90`,
		`klique_idle_node_errors{cluster="c",node="n1"} 0`,
		"klique_idle_collector_reports_dropped_total 1",
	} {
		if !strings.Contains(out, want+"\n") {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if !strings.Contains(get(t, s, "/healthz"), `"ok": true`) {
		t.Error("/healthz broke")
	}

	// A stalled sampling loop: the old report must not be served as current.
	s = New("x", fakeSource{last: r, lastSample: time.Now().Add(-time.Hour)}, fakeSender{}, time.Minute, quiet)
	if out := get(t, s, "/metrics"); strings.Contains(out, "klique_idle_gpu_") || !strings.Contains(out, "klique_idle_collector_reports_pending") {
		t.Errorf("stale report still exported:\n%s", out)
	}
}
