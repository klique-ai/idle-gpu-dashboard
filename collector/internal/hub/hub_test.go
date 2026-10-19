package hub

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/clearml/klique-idle-dashboard/collector/internal/report"
	"github.com/clearml/klique-idle-dashboard/collector/internal/sender"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func ptr[T any](v T) *T { return &v }

func sampleReport(node string) report.Report {
	end := time.Date(2026, 10, 19, 10, 0, 10, 0, time.UTC)
	return report.Report{
		Schema:  report.Schema,
		Cluster: "prod",
		Node:    report.Node{Name: node, MemoryTotalBytes: 64 << 30},
		Window:  report.Window{Start: end.Add(-10 * time.Second), End: end},
		Host:    &report.Host{CPUUtilPercent: report.Stat{Avg: 12.5, Max: 30}, MemoryUsedBytes: report.IntStat{Avg: 8 << 30, Max: 9 << 30}},
		GPUs: []report.GPU{
			{Index: 0, UUID: "GPU-a", Name: `NVIDIA "H100"`, MemoryTotalBytes: 80 << 30,
				UtilPercent: &report.Stat{Avg: 97, Max: 100}, MemoryUsedBytes: &report.IntStat{Avg: 40 << 30, Max: 41 << 30},
				PowerWatts: &report.Stat{Avg: 600}, Allocated: ptr(true), Processes: []report.Process{{PID: 1}}},
			{Index: 1, UUID: "GPU-b", Name: "NVIDIA H100", MemoryTotalBytes: 80 << 30, MIGEnabled: true}, // MIG: utilization unknown
		},
		Scheduler: report.Scheduler{Kind: "kubernetes", GPUsAllocated: ptr(1)},
	}
}

func post(t *testing.T, h http.Handler, auth string, body []byte, gz bool) int {
	t.Helper()
	if gz {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		_, _ = zw.Write(body)
		_ = zw.Close()
		body = buf.Bytes()
	}
	req := httptest.NewRequest("POST", "/v1/reports", bytes.NewReader(body))
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	if gz {
		req.Header.Set("Content-Encoding", "gzip")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

func get(h http.Handler, path, auth string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", path, nil)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestTokenAndValidation(t *testing.T) {
	hb := New(Options{Token: "s3cret"}, quiet)
	h := hb.Handler()
	good, _ := json.Marshal(sampleReport("n1"))
	badSchema, _ := json.Marshal(report.Report{Schema: "klique-idle.report.v9", Node: report.Node{Name: "n1"}})
	noNode, _ := json.Marshal(report.Report{Schema: report.Schema})

	for _, tc := range []struct {
		name, auth string
		body       []byte
		gz         bool
		want       int
	}{
		{"no token", "", good, false, http.StatusUnauthorized},
		{"wrong token", "Bearer nope", good, false, http.StatusUnauthorized},
		{"accepted", "Bearer s3cret", good, false, http.StatusAccepted},
		{"gzip", "Bearer s3cret", good, true, http.StatusAccepted},
		{"bad JSON", "Bearer s3cret", []byte("{"), false, http.StatusBadRequest},
		{"unknown schema", "Bearer s3cret", badSchema, false, http.StatusUnprocessableEntity},
		{"no node name", "Bearer s3cret", noNode, false, http.StatusBadRequest},
	} {
		if got := post(t, h, tc.auth, tc.body, tc.gz); got != tc.want {
			t.Errorf("%s: got %d, want %d", tc.name, got, tc.want)
		}
	}

	if got := get(h, "/v1/nodes", "").Code; got != http.StatusUnauthorized {
		t.Errorf("/v1/nodes without token: got %d", got)
	}
	var nodes []report.Report
	if err := json.Unmarshal(get(h, "/v1/nodes", "Bearer s3cret").Body.Bytes(), &nodes); err != nil || len(nodes) != 1 {
		t.Fatalf("/v1/nodes: %v, %d nodes", err, len(nodes))
	}
	for _, path := range []string{"/metrics", "/healthz"} {
		if got := get(h, path, "").Code; got != http.StatusOK {
			t.Errorf("%s should need no token: got %d", path, got)
		}
	}
	// 2 accepted; rejected: 2 bad tokens + bad JSON + unknown schema + no node name.
	if rec, rej := hb.counters(); rec != 2 || rej != 5 {
		t.Errorf("counters: received %d rejected %d, want 2 and 5", rec, rej)
	}
}

func TestMetrics(t *testing.T) {
	hb := New(Options{}, quiet)
	h := hb.Handler()
	body, _ := json.Marshal(sampleReport("n1"))
	post(t, h, "", body, false)
	out := get(h, "/metrics", "").Body.String()

	for _, want := range []string{
		`klique_idle_gpu_utilization_percent{cluster="prod",node="n1",gpu="0",uuid="GPU-a",model="NVIDIA \"H100\""} 97`,
		`klique_idle_gpu_utilization_max_percent{cluster="prod",node="n1",gpu="0",uuid="GPU-a",model="NVIDIA \"H100\""} 100`,
		`klique_idle_gpu_memory_used_bytes{cluster="prod",node="n1",gpu="0",uuid="GPU-a",model="NVIDIA \"H100\""} 44023414784`,
		`klique_idle_gpu_memory_total_bytes{cluster="prod",node="n1",gpu="1",uuid="GPU-b",model="NVIDIA H100"} 85899345920`,
		`klique_idle_gpu_allocated{cluster="prod",node="n1",gpu="0",uuid="GPU-a",model="NVIDIA \"H100\""} 1`,
		`klique_idle_node_cpu_utilization_percent{cluster="prod",node="n1"} 12.5`,
		`klique_idle_node_gpus_allocated{cluster="prod",node="n1"} 1`,
		`klique_idle_node_last_report_timestamp_seconds{cluster="prod",node="n1"} 1792404010`,
		"klique_idle_hub_nodes 1",
		"klique_idle_hub_reports_received_total 1",
	} {
		if !strings.Contains(out, want+"\n") {
			t.Errorf("missing line %q", want)
		}
	}
	// Unknown values are left out, not reported as 0.
	if strings.Contains(out, `utilization_percent{cluster="prod",node="n1",gpu="1"`) {
		t.Error("MIG GPU without utilization must not get a utilization series")
	}
	// Each family is declared once, before its samples.
	if n := strings.Count(out, "# TYPE klique_idle_gpu_memory_total_bytes gauge"); n != 1 {
		t.Errorf("TYPE line appears %d times", n)
	}
	if strings.Contains(out, "forward") {
		t.Error("forward metrics without forwarding configured")
	}
}

func TestStaleNodesDisappear(t *testing.T) {
	hb := New(Options{StaleAfter: time.Minute}, quiet)
	h := hb.Handler()
	body, _ := json.Marshal(sampleReport("n1"))
	post(t, h, "", body, false)

	hb.now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	if out := get(h, "/metrics", "").Body.String(); strings.Contains(out, `node="n1"`) || !strings.Contains(out, "klique_idle_hub_nodes 0\n") {
		t.Errorf("stale node still exported:\n%s", out)
	}
	if body := get(h, "/v1/nodes", "").Body.String(); strings.TrimSpace(body) != "[]" {
		t.Errorf("/v1/nodes still lists the stale node: %s", body)
	}
}

// TestForwardToCloud runs the real HTTP sender against a stand-in Klique Cloud.
func TestForwardToCloud(t *testing.T) {
	var mu sync.Mutex
	var got []map[string]any
	var auth, agent string
	cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var rep map[string]any
		_ = json.NewDecoder(r.Body).Decode(&rep)
		mu.Lock()
		got, auth, agent = append(got, rep), r.Header.Get("Authorization"), r.Header.Get("User-Agent")
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}))
	defer cloud.Close()

	fwd, err := sender.NewPool(2, sender.HTTPOptions{URL: cloud.URL + "/v1/reports", Token: "kq_abc_def", Timeout: time.Second, QueueSize: 10, UserAgent: "klique-idle-hub/test"}, quiet)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go fwd.Run(ctx)

	hb := New(Options{Forward: fwd}, quiet)
	rep := sampleReport("n1")
	rep.ReportID = "id-1"
	body, _ := json.Marshal(rep)
	// A field from a newer collector, unknown to this hub, must reach the server.
	body = append(body[:len(body)-1], []byte(`,"future_field":"kept"}`)...)
	if code := post(t, hb.Handler(), "", body, true); code != http.StatusAccepted {
		t.Fatalf("post: %d", code)
	}

	deadline := time.Now().Add(3 * time.Second)
	for fwd.Stats().Sent == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0]["report_id"] != "id-1" || got[0]["future_field"] != "kept" {
		t.Fatalf("cloud got %+v", got)
	}
	if auth != "Bearer kq_abc_def" || agent != "klique-idle-hub/test" {
		t.Errorf("headers: Authorization %q, User-Agent %q", auth, agent)
	}
	if out := get(hb.Handler(), "/metrics", "").Body.String(); !strings.Contains(out, "klique_idle_hub_forward_sent_total 1\n") {
		t.Error("forward metrics missing")
	}
}

func TestGzipBombRejected(t *testing.T) {
	h := New(Options{}, quiet).Handler()
	huge := append([]byte(`{"schema":"`+report.Schema+`","pad":"`), bytes.Repeat([]byte("a"), maxBody+10)...)
	huge = append(huge, '"', '}')
	if code := post(t, h, "", huge, true); code != http.StatusRequestEntityTooLarge {
		t.Errorf("expanded body over the limit: got %d, want 413", code)
	}
}
