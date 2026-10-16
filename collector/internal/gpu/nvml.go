package gpu

import (
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"time"

	"github.com/NVIDIA/go-nvml/pkg/nvml"

	"github.com/clearml/klique-idle-dashboard/collector/internal/report"
)

// Where to look for libnvidia-ml.so.1 when the dynamic loader can't find it.
// Covers the GPU Operator's driver container, GKE and common distro layouts.
var fallbackLibraryPaths = []string{
	"/run/nvidia/driver/usr/lib/x86_64-linux-gnu/libnvidia-ml.so.1",
	"/run/nvidia/driver/usr/lib/aarch64-linux-gnu/libnvidia-ml.so.1",
	"/run/nvidia/driver/usr/lib64/libnvidia-ml.so.1",
	"/usr/local/nvidia/lib64/libnvidia-ml.so.1",
	"/home/kubernetes/bin/nvidia/lib64/libnvidia-ml.so.1",
	"/usr/lib64/libnvidia-ml.so.1",
	"/usr/lib/x86_64-linux-gnu/libnvidia-ml.so.1",
	"/usr/lib/aarch64-linux-gnu/libnvidia-ml.so.1",
}

// ErrNoDriver means the NVIDIA management library could not be loaded.
var ErrNoDriver = errors.New("NVIDIA driver library libnvidia-ml.so.1 not found " +
	"(no NVIDIA driver on this node, or it is not visible to the collector)")

// ErrDriverContainer means the driver is run by the NVIDIA GPU Operator's
// driver container: its library is visible on the host, but the GPU device
// files only exist inside that container, so NVML can't open the GPUs.
var ErrDriverContainer = errors.New("the NVIDIA driver on this node is managed by the GPU Operator's " +
	"driver container (/run/nvidia/driver), so GPUs can't be read from the host: " +
	"deploy the collector with the Helm chart (DaemonSet) on this Kubernetes node instead")

// driverContainerRoot is where the GPU Operator exposes its driver container.
const driverContainerRoot = "/run/nvidia/driver/"

// NVML reads GPUs through the NVIDIA Management Library. It only issues
// read-only queries: it never creates a CUDA context or changes GPU settings.
type NVML struct {
	libPath string
	newLib  func(path string) nvml.Interface
	now     func() time.Time
	retry   time.Duration
	log     *slog.Logger

	lib     nvml.Interface
	ready   bool
	lastTry time.Time
	lastErr error
	devices []*nvmlDevice
	driver  *report.GPUDriver
}

type nvmlDevice struct {
	handle nvml.Device
	info   Device
	// GetSamples bookkeeping: the driver keeps a short buffer of utilization
	// readings; asking for everything newer than lastTS gives full coverage
	// between our polls instead of a single point-in-time reading.
	lastTS     uint64
	noSamples  bool // GetSamples unsupported: use point-in-time readings
	memV1      bool // GetMemoryInfo_v2 unsupported: use v1
	warnedUtil bool
}

// NewNVML returns an NVML source. libPath overrides the library location;
// empty means the dynamic loader's default search plus known fallbacks.
func NewNVML(libPath string, log *slog.Logger) *NVML {
	return &NVML{
		libPath: libPath,
		newLib: func(path string) nvml.Interface {
			if path == "" {
				return nvml.New()
			}
			return nvml.New(nvml.WithLibraryPath(path))
		},
		now:   time.Now,
		retry: 30 * time.Second,
		log:   log,
	}
}

func (s *NVML) Kind() string { return KindNVML }

// ensure initialises NVML, at most once per retry period while it keeps
// failing. Late success is normal on Kubernetes, where the driver container
// can start after the collector.
func (s *NVML) ensure() error {
	if s.ready {
		return nil
	}
	if !s.lastTry.IsZero() && s.now().Sub(s.lastTry) < s.retry {
		return s.lastErr
	}
	s.lastTry = s.now()
	s.lastErr = s.init()
	if s.lastErr == nil {
		s.ready = true
		s.log.Info("NVML initialised", "gpus", len(s.devices), "driver", s.driver.Version)
	}
	return s.lastErr
}

func (s *NVML) init() error {
	paths := []string{s.libPath}
	if s.libPath == "" {
		paths = append(paths, fallbackLibraryPaths...)
	}
	ret := nvml.ERROR_LIBRARY_NOT_FOUND
	for _, p := range paths {
		lib := s.newLib(p)
		if ret = lib.Init(); ret == nvml.SUCCESS {
			s.lib = lib
			if p != "" {
				s.log.Info("loaded NVML from fallback path", "path", p)
			}
			break
		}
		if ret != nvml.ERROR_LIBRARY_NOT_FOUND {
			if ret == nvml.ERROR_DRIVER_NOT_LOADED && strings.HasPrefix(p, driverContainerRoot) {
				return ErrDriverContainer
			}
			return fmt.Errorf("NVML init: %s", ret.Error())
		}
	}
	if ret == nvml.ERROR_LIBRARY_NOT_FOUND {
		return ErrNoDriver
	}

	driver := &report.GPUDriver{}
	if v, r := s.lib.SystemGetDriverVersion(); r == nvml.SUCCESS {
		driver.Version = v
	}
	if v, r := s.lib.SystemGetCudaDriverVersion(); r == nvml.SUCCESS && v > 0 {
		driver.CUDAVersion = fmt.Sprintf("%d.%d", v/1000, (v%1000)/10)
	}

	count, r := s.lib.DeviceGetCount()
	if r != nvml.SUCCESS {
		s.lib.Shutdown()
		return fmt.Errorf("NVML device count: %s", r.Error())
	}
	devices := make([]*nvmlDevice, 0, count)
	for i := 0; i < count; i++ {
		d, err := s.describe(i)
		if err != nil {
			s.lib.Shutdown()
			return err
		}
		devices = append(devices, d)
	}
	s.devices, s.driver = devices, driver
	return nil
}

func (s *NVML) describe(i int) (*nvmlDevice, error) {
	h, r := s.lib.DeviceGetHandleByIndex(i)
	if r != nvml.SUCCESS {
		return nil, fmt.Errorf("NVML GPU %d handle: %s", i, r.Error())
	}
	d := &nvmlDevice{handle: h, info: Device{Index: i}}
	if d.info.UUID, r = h.GetUUID(); r != nvml.SUCCESS {
		return nil, fmt.Errorf("NVML GPU %d UUID: %s", i, r.Error())
	}
	if name, r := h.GetName(); r == nvml.SUCCESS {
		d.info.Name = name
	}
	if pci, r := h.GetPciInfo(); r == nvml.SUCCESS {
		d.info.PCIBusID = cString(pci.BusId[:])
	}
	if total, _, ok := d.memory(); ok {
		d.info.MemoryTotal = total
	}
	if cur, _, r := h.GetMigMode(); r == nvml.SUCCESS {
		d.info.MIGEnabled = cur == nvml.DEVICE_MIG_ENABLE
	}
	return d, nil
}

// Inventory implements Source.
func (s *NVML) Inventory() (Inventory, error) {
	if err := s.ensure(); err != nil {
		return Inventory{}, err
	}
	inv := Inventory{Driver: s.driver, Devices: make([]Device, len(s.devices))}
	for i, d := range s.devices {
		inv.Devices[i] = d.info
	}
	return inv, nil
}

// Sample implements Source.
func (s *NVML) Sample() ([]Sample, error) {
	if err := s.ensure(); err != nil {
		return nil, err
	}
	out := make([]Sample, len(s.devices))
	lost := false
	for i, d := range s.devices {
		out[i] = s.sampleDevice(d)
		for _, e := range out[i].Errors {
			if strings.Contains(e, nvml.ERROR_GPU_IS_LOST.Error()) {
				lost = true
			}
		}
	}
	if lost {
		// A GPU fell off the bus; re-enumerate on the next call.
		s.reset()
	}
	return out, nil
}

func (s *NVML) sampleDevice(d *nvmlDevice) Sample {
	smp := Sample{UUID: d.info.UUID}
	h := d.handle

	if d.info.MIGEnabled {
		smp.Errors = append(smp.Errors, "utilization not available: MIG is enabled")
	} else if vals, err := d.utilization(s.log); err != nil {
		smp.Errors = append(smp.Errors, err.Error())
	} else {
		smp.Util = vals
	}

	if _, used, ok := d.memory(); ok {
		smp.MemoryUsed = &used
	}
	if mw, r := h.GetPowerUsage(); r == nvml.SUCCESS {
		w := float64(mw) / 1000
		smp.PowerWatts = &w
	}
	if t, r := h.GetTemperature(nvml.TEMPERATURE_GPU); r == nvml.SUCCESS {
		c := float64(t)
		smp.TempC = &c
	}

	procs, err := d.processes()
	if err != nil {
		smp.Errors = append(smp.Errors, err.Error())
	}
	smp.Processes = procs
	return smp
}

// utilization returns all driver readings since the previous call, falling
// back to a single point-in-time reading.
func (d *nvmlDevice) utilization(log *slog.Logger) ([]float64, error) {
	if !d.noSamples {
		vt, samples, r := d.handle.GetSamples(nvml.GPU_UTILIZATION_SAMPLES, d.lastTS)
		switch r {
		case nvml.SUCCESS:
			first := d.lastTS == 0
			var vals []float64
			for _, smp := range samples {
				if smp.TimeStamp <= d.lastTS {
					continue
				}
				d.lastTS = smp.TimeStamp
				if v, ok := sampleValue(vt, smp.SampleValue); ok {
					vals = append(vals, v)
				}
			}
			// The first call returns the driver's whole buffer, which predates
			// this window: keep its timestamp as a baseline only.
			if !first && len(vals) > 0 {
				return clampPercent(vals, d, log), nil
			}
		case nvml.ERROR_NOT_SUPPORTED, nvml.ERROR_FUNCTION_NOT_FOUND:
			d.noSamples = true
		}
		// ERROR_NOT_FOUND (no new readings) and other cases: fall through.
	}
	u, r := d.handle.GetUtilizationRates()
	if r != nvml.SUCCESS {
		return nil, fmt.Errorf("utilization: %s", r.Error())
	}
	return []float64{float64(u.Gpu)}, nil
}

// clampPercent guards against readings outside 0-100, which would indicate a
// driver reporting in different units; it logs once per device.
func clampPercent(vals []float64, d *nvmlDevice, log *slog.Logger) []float64 {
	for i, v := range vals {
		if v > 100 || v < 0 {
			if !d.warnedUtil {
				d.warnedUtil = true
				log.Warn("GPU utilization sample outside 0-100, clamping", "gpu", d.info.Index, "value", v)
			}
			vals[i] = math.Max(0, math.Min(100, v))
		}
	}
	return vals
}

// memory returns total and used framebuffer bytes. v2 excludes memory the
// driver reserves for itself, so an idle GPU reads close to zero.
func (d *nvmlDevice) memory() (total, used uint64, ok bool) {
	if !d.memV1 {
		m, r := d.handle.GetMemoryInfo_v2()
		if r == nvml.SUCCESS {
			return m.Total, m.Used, true
		}
		if r == nvml.ERROR_NOT_SUPPORTED || r == nvml.ERROR_FUNCTION_NOT_FOUND || r == nvml.ERROR_ARGUMENT_VERSION_MISMATCH {
			d.memV1 = true
		}
	}
	m, r := d.handle.GetMemoryInfo()
	if r != nvml.SUCCESS {
		return 0, 0, false
	}
	return m.Total, m.Used, true
}

// processes lists compute and graphics processes, merged by PID.
func (d *nvmlDevice) processes() ([]Process, error) {
	compute, rc := d.handle.GetComputeRunningProcesses()
	graphics, rg := d.handle.GetGraphicsRunningProcesses()
	if rc != nvml.SUCCESS && rg != nvml.SUCCESS {
		if rc == nvml.ERROR_NOT_SUPPORTED {
			return nil, nil
		}
		return nil, fmt.Errorf("processes: %s", rc.Error())
	}
	byPID := map[uint32]int{}
	var out []Process
	for _, p := range append(compute, graphics...) {
		mem := p.UsedGpuMemory
		if mem == math.MaxUint64 { // NVML_VALUE_NOT_AVAILABLE
			mem = 0
		}
		if i, seen := byPID[p.Pid]; seen {
			out[i].MemoryUsed = max(out[i].MemoryUsed, mem)
			continue
		}
		byPID[p.Pid] = len(out)
		out = append(out, Process{PID: int(p.Pid), MemoryUsed: mem})
	}
	return out, nil
}

func (s *NVML) reset() {
	if s.lib != nil && s.ready {
		s.lib.Shutdown()
	}
	s.ready, s.devices, s.lastTry = false, nil, time.Time{}
}

// Close implements Source.
func (s *NVML) Close() { s.reset() }

func sampleValue(vt nvml.ValueType, v [8]byte) (float64, bool) {
	switch vt {
	case nvml.VALUE_TYPE_DOUBLE:
		return math.Float64frombits(binary.NativeEndian.Uint64(v[:])), true
	case nvml.VALUE_TYPE_UNSIGNED_INT:
		return float64(binary.NativeEndian.Uint32(v[:4])), true
	case nvml.VALUE_TYPE_UNSIGNED_LONG, nvml.VALUE_TYPE_UNSIGNED_LONG_LONG:
		return float64(binary.NativeEndian.Uint64(v[:])), true
	case nvml.VALUE_TYPE_SIGNED_LONG_LONG:
		return float64(int64(binary.NativeEndian.Uint64(v[:]))), true
	case nvml.VALUE_TYPE_SIGNED_INT:
		return float64(int32(binary.NativeEndian.Uint32(v[:4]))), true
	case nvml.VALUE_TYPE_UNSIGNED_SHORT:
		return float64(binary.NativeEndian.Uint16(v[:2])), true
	}
	return 0, false
}

// cString converts a NUL-terminated C char array.
func cString(b []int8) string {
	var sb strings.Builder
	for _, c := range b {
		if c == 0 {
			break
		}
		sb.WriteByte(byte(c))
	}
	return sb.String()
}
