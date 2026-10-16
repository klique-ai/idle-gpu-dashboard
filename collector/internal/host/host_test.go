package host

import (
	"math"
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReader(t *testing.T) {
	root := t.TempDir()
	proc, hostRoot := filepath.Join(root, "proc"), filepath.Join(root, "host")
	write(t, filepath.Join(proc, "meminfo"), "MemTotal:       16384000 kB\nMemFree:         1000000 kB\nMemAvailable:    4096000 kB\n")
	write(t, filepath.Join(proc, "cpuinfo"),
		"processor\t: 0\nmodel name\t: AMD EPYC 9654 96-Core Processor\n\nprocessor\t: 1\nmodel name\t: AMD EPYC 9654 96-Core Processor\n")
	write(t, filepath.Join(proc, "sys", "kernel", "osrelease"), "6.8.0-45-generic\n")
	write(t, filepath.Join(hostRoot, "etc", "os-release"), "NAME=\"Ubuntu\"\nPRETTY_NAME=\"Ubuntu 24.04.1 LTS\"\n")
	// user nice system idle iowait irq softirq steal guest guest_nice
	write(t, filepath.Join(proc, "stat"), "cpu  100 0 100 700 100 0 0 0 50 0\ncpu0 50 0 50 350 50 0 0 0 25 0\n")

	r := New(proc, hostRoot)
	info, err := r.Info()
	if err != nil {
		t.Fatal(err)
	}
	want := Info{CPUModel: "AMD EPYC 9654 96-Core Processor", LogicalCores: 2, MemoryTotal: 16384000 * 1024,
		Kernel: "6.8.0-45-generic", OS: "Ubuntu 24.04.1 LTS"}
	if info != want {
		t.Fatalf("Info() = %+v, want %+v", info, want)
	}

	s, err := r.Sample()
	if err != nil {
		t.Fatal(err)
	}
	if s.CPUOK {
		t.Fatal("first sample must not report CPU utilization")
	}
	if want := uint64((16384000 - 4096000) * 1024); s.MemoryUsed != want {
		t.Fatalf("MemoryUsed = %d, want %d", s.MemoryUsed, want)
	}

	// +300 busy (user+system), +100 idle+iowait, guest not double counted.
	write(t, filepath.Join(proc, "stat"), "cpu  250 0 250 780 120 0 0 0 999 0\n")
	s, err = r.Sample()
	if err != nil {
		t.Fatal(err)
	}
	if !s.CPUOK || math.Abs(s.CPUUtil-75) > 1e-9 {
		t.Fatalf("CPUUtil = %v (ok=%v), want 75", s.CPUUtil, s.CPUOK)
	}
}

func TestMeminfoWithoutMemAvailable(t *testing.T) {
	proc := t.TempDir()
	write(t, filepath.Join(proc, "meminfo"), "MemTotal: 1000 kB\nMemFree: 100 kB\nBuffers: 100 kB\nCached: 300 kB\n")
	write(t, filepath.Join(proc, "stat"), "cpu  1 0 1 1 0 0 0 0\n")
	s, err := New(proc, "/").Sample()
	if err != nil {
		t.Fatal(err)
	}
	if s.MemoryUsed != 500*1024 {
		t.Fatalf("MemoryUsed = %d, want %d", s.MemoryUsed, 500*1024)
	}
}

func TestMissingProc(t *testing.T) {
	r := New(filepath.Join(t.TempDir(), "nope"), "/")
	if _, err := r.Sample(); err == nil {
		t.Fatal("expected an error without procfs")
	}
	if _, err := r.Info(); err == nil {
		t.Fatal("expected an error without procfs")
	}
}
