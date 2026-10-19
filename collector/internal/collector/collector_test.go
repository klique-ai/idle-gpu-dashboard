package collector

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/klique-ai/idle-gpu-dashboard/collector/internal/gpu"
	"github.com/klique-ai/idle-gpu-dashboard/collector/internal/host"
	"github.com/klique-ai/idle-gpu-dashboard/collector/internal/owner"
	"github.com/klique-ai/idle-gpu-dashboard/collector/internal/report"
	"github.com/klique-ai/idle-gpu-dashboard/collector/internal/sender"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// scripted is a gpu.Source returning canned samples.
type scripted struct {
	devices []gpu.Device
	samples [][]gpu.Sample
	i       int
}

func (s *scripted) Kind() string { return "test" }
func (s *scripted) Inventory() (gpu.Inventory, error) {
	return gpu.Inventory{Devices: s.devices, Driver: &report.GPUDriver{Version: "1"}}, nil
}
func (s *scripted) Sample() ([]gpu.Sample, error) {
	smp := s.samples[min(s.i, len(s.samples)-1)]
	s.i++
	return smp, nil
}
func (s *scripted) Close() {}

func u64(v uint64) *uint64 { return &v }

func fakeProc(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"stat":                 "cpu  100 0 100 800 0 0 0 0\n",
		"meminfo":              "MemTotal: 1000 kB\nMemAvailable: 600 kB\n",
		"cpuinfo":              "processor\t: 0\nmodel name\t: Test CPU\n",
		"555/comm":             "python3\n",
		"555/status":           "Uid:\t0\t0\t0\t0\n",
		"555/cgroup":           "0::/system.slice/docker-" + strings.Repeat("ab", 32) + ".scope\n",
		"sys/kernel/osrelease": "6.1.0\n",
	}
	for name, content := range files {
		p := filepath.Join(root, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestOnceReport(t *testing.T) {
	src := &scripted{
		devices: []gpu.Device{{Index: 0, UUID: "GPU-a", Name: "Test GPU", MemoryTotal: 1000}, {Index: 1, UUID: "GPU-b", Name: "Test GPU", MemoryTotal: 1000}},
		samples: [][]gpu.Sample{
			{
				{UUID: "GPU-a", Util: []float64{0, 0}, MemoryUsed: u64(0)},
				{UUID: "GPU-b", Util: []float64{80, 100}, MemoryUsed: u64(500), Processes: []gpu.Process{{PID: 555, MemoryUsed: 400}}},
			},
			{
				{UUID: "GPU-a", Util: []float64{0}, MemoryUsed: u64(0)},
				{UUID: "GPU-b", Util: []float64{60}, MemoryUsed: u64(700), Processes: []gpu.Process{{PID: 555, MemoryUsed: 650}, {PID: 999}}},
			},
		},
	}
	proc := fakeProc(t)
	var buf bytes.Buffer
	c := New(Options{
		Cluster: "test", NodeName: "n1", Labels: map[string]string{"site": "lab"},
		SampleInterval: 20 * time.Millisecond, ReportInterval: time.Second, Version: "v-test", SchedulerKind: "none",
	}, src, host.New(proc, proc), owner.NewResolver(proc, false), nil, sender.NewStdout(&buf, false), quiet)
	c.Run(context.Background(), true)

	var r report.Report
	if err := json.Unmarshal(buf.Bytes(), &r); err != nil {
		t.Fatalf("output is not one report: %v\n%s", err, buf.String())
	}
	if r.Schema != report.Schema || r.Cluster != "test" || r.Node.Name != "n1" || r.Node.Labels["site"] != "lab" ||
		r.Collector.Seq != 1 || r.Collector.Version != "v-test" || r.Window.Samples != 2 {
		t.Fatalf("header fields wrong: %+v", r)
	}
	if r.Node.CPU.Model != "Test CPU" || r.Node.MemoryTotalBytes != 1000*1024 || r.Node.Kernel != "6.1.0" {
		t.Fatalf("node = %+v", r.Node)
	}
	if r.Host == nil || r.Host.MemoryUsedBytes.Max != 400*1024 {
		t.Fatalf("host = %+v", r.Host)
	}

	a, b := r.GPUs[0], r.GPUs[1]
	if a.UtilPercent.Max != 0 || len(a.Processes) != 0 {
		t.Fatalf("idle GPU = %+v", a)
	}
	if b.UtilPercent.Avg != 80 || b.UtilPercent.Max != 100 || b.MemoryUsedBytes.Max != 700 || b.MemoryUsedBytes.Avg != 600 {
		t.Fatalf("busy GPU stats = util %+v mem %+v", b.UtilPercent, b.MemoryUsedBytes)
	}
	if len(b.Processes) != 2 {
		t.Fatalf("processes = %+v", b.Processes)
	}
	p := b.Processes[0]
	if p.PID != 555 || p.Name != "python3" || p.User != "root" || p.MemoryUsedBytes != 650 || p.Owner == nil || p.Owner.Kind != "docker" {
		t.Fatalf("process = %+v owner=%+v", p, p.Owner)
	}
	if b.Processes[1].PID != 999 || b.Processes[1].Name != "" { // exited / invisible: PID only
		t.Fatalf("invisible process = %+v", b.Processes[1])
	}
	if len(r.Errors) != 0 {
		t.Fatalf("unexpected errors: %v", r.Errors)
	}
}

func TestWindowsAreContiguousAndFinalWindowHasData(t *testing.T) {
	src := &scripted{
		devices: []gpu.Device{{UUID: "GPU-a"}},
		samples: [][]gpu.Sample{{{UUID: "GPU-a", Util: []float64{50}, MemoryUsed: u64(1)}}},
	}
	proc := fakeProc(t)
	var buf bytes.Buffer
	c := New(Options{Cluster: "t", NodeName: "n", SampleInterval: 50 * time.Millisecond, ReportInterval: 200 * time.Millisecond},
		src, host.New(proc, proc), owner.NewResolver(proc, false), nil, sender.NewStdout(&buf, false), quiet)
	ctx, cancel := context.WithTimeout(context.Background(), 730*time.Millisecond)
	defer cancel()
	c.Run(ctx, false)

	var reports []report.Report
	dec := json.NewDecoder(&buf)
	for dec.More() {
		var r report.Report
		if err := dec.Decode(&r); err != nil {
			t.Fatal(err)
		}
		reports = append(reports, r)
	}
	if len(reports) < 3 {
		t.Fatalf("got %d reports, want at least 3", len(reports))
	}
	for i, r := range reports {
		if r.Collector.Seq != uint64(i+1) {
			t.Fatalf("report %d has seq %d", i, r.Collector.Seq)
		}
		if i > 0 && !r.Window.Start.Equal(reports[i-1].Window.End) {
			t.Fatalf("gap between windows %d and %d: %v -> %v", i-1, i, reports[i-1].Window.End, r.Window.Start)
		}
		if i > 0 && i < len(reports)-1 && r.Window.Samples != 4 {
			t.Errorf("full window %d has %d samples, want 4", i, r.Window.Samples)
		}
		if r.GPUs[0].UtilPercent == nil {
			t.Fatalf("window %d has no GPU data", i)
		}
	}
}

func TestHostPIDHint(t *testing.T) {
	src := &scripted{
		devices: []gpu.Device{{UUID: "GPU-a"}},
		samples: [][]gpu.Sample{{{UUID: "GPU-a", Processes: []gpu.Process{{PID: 4242}}}}},
	}
	proc := fakeProc(t)
	var buf bytes.Buffer
	c := New(Options{Cluster: "t", NodeName: "n", SampleInterval: 10 * time.Millisecond, ReportInterval: time.Second},
		src, host.New(proc, proc), owner.NewResolver(proc, false), nil, sender.NewStdout(&buf, false), quiet)
	c.Run(context.Background(), true)
	if !strings.Contains(buf.String(), "hostPID") {
		t.Fatalf("expected a hostPID hint in errors: %s", buf.String())
	}
}

func TestRedact(t *testing.T) {
	r := &report.Report{GPUs: []report.GPU{{
		Processes:   []report.Process{{PID: 1, Name: "python3", User: "alice", Cmdline: "python3 --token=x", Owner: &report.Owner{Kind: "kubernetes", Name: "pod", Namespace: "team"}}},
		AllocatedTo: &report.Owner{Kind: "slurm", ID: "7", Name: "job", User: "alice"},
	}}}
	redact(r, "cluster-a")
	p := r.GPUs[0].Processes[0]
	if p.Cmdline != "" || !strings.HasPrefix(p.User, "h:") || p.User == "alice" || p.Owner.Name == "pod" || p.Owner.Kind != "kubernetes" {
		t.Fatalf("process not redacted: %+v %+v", p, p.Owner)
	}
	if r.GPUs[0].AllocatedTo.User != p.User {
		t.Fatal("same user must hash to the same value within a cluster")
	}
	if r.GPUs[0].AllocatedTo.ID != "7" {
		t.Fatal("IDs are kept")
	}
	sys := &report.Report{GPUs: []report.GPU{{Processes: []report.Process{
		{Owner: &report.Owner{Kind: "systemd", ID: "jupyter-alice.service"}},
		{Owner: &report.Owner{Kind: "kubernetes", ID: "5b10cf23-b886-4608-a316-6471edbb8a3d"}},
	}}}}
	redact(sys, "cluster-a")
	if id := sys.GPUs[0].Processes[0].Owner.ID; !strings.HasPrefix(id, "h:") {
		t.Fatalf("systemd unit name not redacted: %q", id)
	}
	if id := sys.GPUs[0].Processes[1].Owner.ID; id != "5b10cf23-b886-4608-a316-6471edbb8a3d" {
		t.Fatalf("opaque pod UID should be kept, got %q", id)
	}
	r2 := &report.Report{GPUs: []report.GPU{{Processes: []report.Process{{User: "alice"}}}}}
	redact(r2, "cluster-b")
	if r2.GPUs[0].Processes[0].User == p.User {
		t.Fatal("hashes must differ across clusters")
	}
}

func TestAlignedEnd(t *testing.T) {
	t0 := time.Date(2026, 10, 16, 10, 0, 3, 500, time.UTC)
	if got := alignedEnd(t0, 10*time.Second); !got.Equal(time.Date(2026, 10, 16, 10, 0, 10, 0, time.UTC)) {
		t.Fatalf("alignedEnd = %v", got)
	}
	exact := time.Date(2026, 10, 16, 10, 0, 10, 0, time.UTC)
	if got := alignedEnd(exact, 10*time.Second); !got.Equal(exact.Add(10 * time.Second)) {
		t.Fatalf("alignedEnd(on boundary) = %v", got)
	}
}
