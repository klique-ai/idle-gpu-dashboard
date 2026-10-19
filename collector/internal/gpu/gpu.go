// Package gpu reads GPU inventory and utilization. NVML is the real source;
// Fake simulates GPUs for development and demos on machines without them.
package gpu

import "github.com/klique-ai/idle-gpu-dashboard/collector/internal/report"

// Source kinds, reported as collector.gpu_source.
const (
	KindNVML = "nvml"
	KindFake = "fake"
	KindNone = "none"
)

// Source provides GPU data. Implementations are not safe for concurrent use;
// the collector calls them from a single goroutine.
type Source interface {
	Kind() string
	// Inventory returns the GPUs currently visible. A Source that cannot reach
	// its backend (driver missing, ...) returns an error and no devices, and
	// keeps retrying on later calls.
	Inventory() (Inventory, error)
	// Sample returns one Sample per device in Inventory order, covering the
	// time since the previous call.
	Sample() ([]Sample, error)
	Close()
}

// Inventory is the static view of the node's GPUs.
type Inventory struct {
	Devices []Device
	Driver  *report.GPUDriver
}

// Device is one GPU's static properties.
type Device struct {
	Index       int
	UUID        string
	Name        string
	PCIBusID    string
	MemoryTotal uint64
	MIGEnabled  bool
}

// Sample is one device's dynamic state. Pointer fields are nil when the value
// is not available on this GPU.
type Sample struct {
	UUID       string
	Util       []float64 // utilization readings (percent) since the previous sample; nil = unknown
	MemoryUsed *uint64
	PowerWatts *float64
	TempC      *float64
	Processes  []Process
	Errors     []string
}

// Process is a process using a GPU. Name, User and Owner are normally filled
// in later from /proc; the fake source fills them directly.
type Process struct {
	PID        int
	MemoryUsed uint64
	Name       string
	User       string
	Owner      *report.Owner
}

// None is the Source for nodes where GPU collection is disabled.
type None struct{}

func (None) Kind() string                  { return KindNone }
func (None) Inventory() (Inventory, error) { return Inventory{}, nil }
func (None) Sample() ([]Sample, error)     { return nil, nil }
func (None) Close()                        {}
