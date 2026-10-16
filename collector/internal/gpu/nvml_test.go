package gpu

import (
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"github.com/NVIDIA/go-nvml/pkg/nvml/mock"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func utilSample(ts uint64, v uint32) nvml.Sample {
	var s nvml.Sample
	s.TimeStamp = ts
	binary.NativeEndian.PutUint32(s.SampleValue[:4], v)
	return s
}

func busID(s string) [32]int8 {
	var b [32]int8
	for i := range s {
		b[i] = int8(s[i])
	}
	return b
}

// fakeDevice is an H100-like mock whose utilization samples are scripted.
func fakeDevice(uuid string, samples [][]nvml.Sample) *mock.Device {
	call := 0
	return &mock.Device{
		GetUUIDFunc: func() (string, nvml.Return) { return uuid, nvml.SUCCESS },
		GetNameFunc: func() (string, nvml.Return) { return "NVIDIA H100 80GB HBM3", nvml.SUCCESS },
		GetPciInfoFunc: func() (nvml.PciInfo, nvml.Return) {
			return nvml.PciInfo{BusId: busID("00000000:18:00.0")}, nvml.SUCCESS
		},
		GetMemoryInfo_v2Func: func() (nvml.Memory_v2, nvml.Return) {
			return nvml.Memory_v2{Total: 80 << 30, Reserved: 512 << 20, Used: 10 << 30}, nvml.SUCCESS
		},
		GetMigModeFunc: func() (int, int, nvml.Return) { return nvml.DEVICE_MIG_DISABLE, nvml.DEVICE_MIG_DISABLE, nvml.SUCCESS },
		GetSamplesFunc: func(st nvml.SamplingType, last uint64) (nvml.ValueType, []nvml.Sample, nvml.Return) {
			if st != nvml.GPU_UTILIZATION_SAMPLES {
				return 0, nil, nvml.ERROR_INVALID_ARGUMENT
			}
			if call >= len(samples) {
				return 0, nil, nvml.ERROR_NOT_FOUND
			}
			call++
			return nvml.VALUE_TYPE_UNSIGNED_INT, samples[call-1], nvml.SUCCESS
		},
		GetUtilizationRatesFunc: func() (nvml.Utilization, nvml.Return) { return nvml.Utilization{Gpu: 42}, nvml.SUCCESS },
		GetPowerUsageFunc:       func() (uint32, nvml.Return) { return 350500, nvml.SUCCESS },
		GetTemperatureFunc:      func(nvml.TemperatureSensors) (uint32, nvml.Return) { return 61, nvml.SUCCESS },
		GetComputeRunningProcessesFunc: func() ([]nvml.ProcessInfo, nvml.Return) {
			return []nvml.ProcessInfo{{Pid: 100, UsedGpuMemory: 4 << 30}, {Pid: 200, UsedGpuMemory: math.MaxUint64}}, nvml.SUCCESS
		},
		GetGraphicsRunningProcessesFunc: func() ([]nvml.ProcessInfo, nvml.Return) {
			return []nvml.ProcessInfo{{Pid: 100, UsedGpuMemory: 6 << 30}}, nvml.SUCCESS
		},
	}
}

func fakeLib(devices ...*mock.Device) *mock.Interface {
	return &mock.Interface{
		InitFunc:                   func() nvml.Return { return nvml.SUCCESS },
		ShutdownFunc:               func() nvml.Return { return nvml.SUCCESS },
		SystemGetDriverVersionFunc: func() (string, nvml.Return) { return "570.86.15", nvml.SUCCESS },
		SystemGetCudaDriverVersionFunc: func() (int, nvml.Return) {
			return 12080, nvml.SUCCESS
		},
		DeviceGetCountFunc: func() (int, nvml.Return) { return len(devices), nvml.SUCCESS },
		DeviceGetHandleByIndexFunc: func(i int) (nvml.Device, nvml.Return) {
			return devices[i], nvml.SUCCESS
		},
	}
}

func newTestNVML(lib nvml.Interface) *NVML {
	s := NewNVML("", quiet)
	s.newLib = func(string) nvml.Interface { return lib }
	return s
}

func TestNVMLInventoryAndSamples(t *testing.T) {
	dev := fakeDevice("GPU-aaaa", [][]nvml.Sample{
		{utilSample(100, 99), utilSample(200, 98)},                      // first call: driver backlog, baseline only
		{utilSample(200, 98), utilSample(300, 10), utilSample(400, 30)}, // ts 200 already seen
	})
	s := newTestNVML(fakeLib(dev))

	inv, err := s.Inventory()
	if err != nil {
		t.Fatal(err)
	}
	wantDev := Device{Index: 0, UUID: "GPU-aaaa", Name: "NVIDIA H100 80GB HBM3", PCIBusID: "00000000:18:00.0", MemoryTotal: 80 << 30}
	if len(inv.Devices) != 1 || inv.Devices[0] != wantDev {
		t.Fatalf("devices = %+v", inv.Devices)
	}
	if inv.Driver.Version != "570.86.15" || inv.Driver.CUDAVersion != "12.8" {
		t.Fatalf("driver = %+v", inv.Driver)
	}

	first, err := s.Sample()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first[0].Util, []float64{42}) {
		t.Fatalf("first sample util = %v, want point-in-time fallback [42]", first[0].Util)
	}
	second, _ := s.Sample()
	smp := second[0]
	if !reflect.DeepEqual(smp.Util, []float64{10, 30}) {
		t.Fatalf("second sample util = %v, want [10 30]", smp.Util)
	}
	if *smp.MemoryUsed != 10<<30 || *smp.PowerWatts != 350.5 || *smp.TempC != 61 {
		t.Fatalf("mem/power/temp = %d/%v/%v", *smp.MemoryUsed, *smp.PowerWatts, *smp.TempC)
	}
	wantProcs := []Process{{PID: 100, MemoryUsed: 6 << 30}, {PID: 200, MemoryUsed: 0}}
	if !reflect.DeepEqual(smp.Processes, wantProcs) {
		t.Fatalf("processes = %+v, want %+v", smp.Processes, wantProcs)
	}
	// No new readings: falls back to a point-in-time reading.
	third, _ := s.Sample()
	if !reflect.DeepEqual(third[0].Util, []float64{42}) {
		t.Fatalf("third sample util = %v", third[0].Util)
	}
}

func TestNVMLMemoryV1Fallback(t *testing.T) {
	dev := fakeDevice("GPU-v1", nil)
	dev.GetMemoryInfo_v2Func = func() (nvml.Memory_v2, nvml.Return) { return nvml.Memory_v2{}, nvml.ERROR_FUNCTION_NOT_FOUND }
	dev.GetMemoryInfoFunc = func() (nvml.Memory, nvml.Return) { return nvml.Memory{Total: 40 << 30, Used: 1 << 30}, nvml.SUCCESS }
	s := newTestNVML(fakeLib(dev))
	inv, err := s.Inventory()
	if err != nil {
		t.Fatal(err)
	}
	if inv.Devices[0].MemoryTotal != 40<<30 {
		t.Fatalf("total = %d", inv.Devices[0].MemoryTotal)
	}
	smp, _ := s.Sample()
	if *smp[0].MemoryUsed != 1<<30 {
		t.Fatalf("used = %d", *smp[0].MemoryUsed)
	}
}

func TestNVMLMIGHasNoUtilization(t *testing.T) {
	dev := fakeDevice("GPU-mig", nil)
	dev.GetMigModeFunc = func() (int, int, nvml.Return) { return nvml.DEVICE_MIG_ENABLE, nvml.DEVICE_MIG_ENABLE, nvml.SUCCESS }
	s := newTestNVML(fakeLib(dev))
	smp, err := s.Sample()
	if err != nil {
		t.Fatal(err)
	}
	if smp[0].Util != nil || len(smp[0].Errors) != 1 {
		t.Fatalf("util = %v errors = %v", smp[0].Util, smp[0].Errors)
	}
}

func TestNVMLNoDriverRetriesLater(t *testing.T) {
	tries := 0
	var current nvml.Interface = &mock.Interface{InitFunc: func() nvml.Return { tries++; return nvml.ERROR_LIBRARY_NOT_FOUND }}
	s := NewNVML("", quiet)
	s.newLib = func(string) nvml.Interface { return current }
	now := time.Unix(1000, 0)
	s.now = func() time.Time { return now }

	if _, err := s.Sample(); !errors.Is(err, ErrNoDriver) {
		t.Fatalf("err = %v, want ErrNoDriver", err)
	}
	if tries != 1+len(fallbackLibraryPaths) {
		t.Fatalf("tried %d library paths, want %d", tries, 1+len(fallbackLibraryPaths))
	}
	s.Sample() // within the retry period: no new attempt
	if tries != 1+len(fallbackLibraryPaths) {
		t.Fatalf("retried too early (%d tries)", tries)
	}

	// The driver shows up later (e.g. driver container started after us).
	current = fakeLib(fakeDevice("GPU-late", nil))
	now = now.Add(31 * time.Second)
	inv, err := s.Inventory()
	if err != nil || len(inv.Devices) != 1 {
		t.Fatalf("after driver appeared: %v, %+v", err, inv)
	}
}

// On GPU Operator nodes the host sees the driver container's library but not
// its device files: NVML loads from /run/nvidia/driver and reports "driver not
// loaded". Seen on a Nebius H100 node.
func TestNVMLDriverContainerOnHost(t *testing.T) {
	s := NewNVML("", quiet)
	s.newLib = func(path string) nvml.Interface {
		ret := nvml.ERROR_LIBRARY_NOT_FOUND
		if strings.HasPrefix(path, driverContainerRoot) {
			ret = nvml.ERROR_DRIVER_NOT_LOADED
		}
		return &mock.Interface{InitFunc: func() nvml.Return { return ret }}
	}
	if _, err := s.Inventory(); !errors.Is(err, ErrDriverContainer) {
		t.Fatalf("err = %v, want ErrDriverContainer", err)
	}

	// "Driver not loaded" from a regular library path stays a plain NVML error.
	s = NewNVML("/usr/lib64/libnvidia-ml.so.1", quiet)
	s.newLib = func(string) nvml.Interface {
		return &mock.Interface{InitFunc: func() nvml.Return { return nvml.ERROR_DRIVER_NOT_LOADED }}
	}
	if _, err := s.Inventory(); err == nil || errors.Is(err, ErrDriverContainer) {
		t.Fatalf("err = %v, want a plain NVML error", err)
	}
}

func TestNVMLLostGPUReenumerates(t *testing.T) {
	dev := fakeDevice("GPU-lost", nil)
	dev.GetUtilizationRatesFunc = func() (nvml.Utilization, nvml.Return) { return nvml.Utilization{}, nvml.ERROR_GPU_IS_LOST }
	s := newTestNVML(fakeLib(dev))
	smp, err := s.Sample()
	if err != nil {
		t.Fatal(err)
	}
	if len(smp[0].Errors) == 0 || s.ready {
		t.Fatalf("expected a lost-GPU error and a reset; errors=%v ready=%v", smp[0].Errors, s.ready)
	}
}

func TestSampleValueDecoding(t *testing.T) {
	var v [8]byte
	binary.NativeEndian.PutUint64(v[:], math.Float64bits(12.5))
	if got, ok := sampleValue(nvml.VALUE_TYPE_DOUBLE, v); !ok || got != 12.5 {
		t.Fatalf("double = %v", got)
	}
	binary.NativeEndian.PutUint64(v[:], 77)
	if got, ok := sampleValue(nvml.VALUE_TYPE_UNSIGNED_LONG_LONG, v); !ok || got != 77 {
		t.Fatalf("ull = %v", got)
	}
}

func TestFakeIsDeterministicAndPlausible(t *testing.T) {
	a := NewFake(4, FakeModels["h200"], "c/n1", "kubernetes")
	b := NewFake(4, FakeModels["h200"], "c/n1", "kubernetes")
	clock := time.Unix(1_700_000_000, 0)
	a.now = func() time.Time { return clock }
	b.now = a.now

	ia, _ := a.Inventory()
	ib, _ := b.Inventory()
	if !reflect.DeepEqual(ia, ib) {
		t.Fatal("same seed gave different inventories")
	}
	sawIdle, sawBusy := false, false
	for i := 0; i < 2000; i++ { // ~66 simulated hours at 2 min steps
		clock = clock.Add(2 * time.Minute)
		sa, _ := a.Sample()
		sb, _ := b.Sample()
		if !reflect.DeepEqual(sa, sb) {
			t.Fatal("same seed gave different samples")
		}
		for j, s := range sa {
			if *s.MemoryUsed > ia.Devices[j].MemoryTotal {
				t.Fatalf("memory used %d > total", *s.MemoryUsed)
			}
			for _, u := range s.Util {
				if u < 0 || u > 100 {
					t.Fatalf("util %v out of range", u)
				}
			}
			if s.Util[0] == 0 && len(s.Processes) == 0 {
				sawIdle = true
			}
			if s.Util[0] > 0 {
				sawBusy = true
				if s.Processes[0].Owner == nil || s.Processes[0].Owner.Kind != "kubernetes" {
					t.Fatalf("busy GPU without a kubernetes owner: %+v", s.Processes)
				}
			}
		}
	}
	if !sawIdle || !sawBusy {
		t.Fatalf("simulation never showed both states (idle=%v busy=%v)", sawIdle, sawBusy)
	}
}
