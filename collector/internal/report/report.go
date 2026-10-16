// Package report defines the JSON document a collector sends for every
// reporting window. It is the contract between the collector and any server
// that receives its data; docs/payload.md describes it field by field.
//
// Compatibility rule: within one Schema value, fields are only ever added.
// Renaming, removing or changing the meaning of a field requires a new Schema.
package report

import (
	"crypto/rand"
	"fmt"
	"time"
)

// Schema identifies the payload format and version.
const Schema = "klique-idle.report.v1"

// Report is one node's view of one reporting window. Each report is
// self-contained: it repeats the node and GPU inventory so a receiver never
// needs earlier reports to interpret it.
type Report struct {
	Schema    string    `json:"schema"`
	ReportID  string    `json:"report_id"`  // random UUID; identical on retries, so receivers can de-duplicate
	CreatedAt time.Time `json:"created_at"` // when the report was assembled (end of window)
	Collector Collector `json:"collector"`
	Cluster   string    `json:"cluster"`
	Node      Node      `json:"node"`
	Window    Window    `json:"window"`
	Host      *Host     `json:"host,omitempty"` // nil when host stats could not be read
	GPUs      []GPU     `json:"gpus"`           // empty when the node has no readable NVIDIA GPUs
	Scheduler Scheduler `json:"scheduler"`
	Errors    []string  `json:"errors,omitempty"` // node-level problems (driver missing, scheduler unreachable, ...)
}

// Collector describes the agent that produced the report.
type Collector struct {
	Version        string    `json:"version"`
	InstanceID     string    `json:"instance_id"` // new random ID on every start
	StartedAt      time.Time `json:"started_at"`
	Seq            uint64    `json:"seq"`             // 1, 2, 3... per instance; a gap means a report was lost
	GPUSource      string    `json:"gpu_source"`      // "nvml", "fake" (simulated data) or "none"
	Redacted       bool      `json:"redacted"`        // true when names were replaced by hashes
	DroppedReports uint64    `json:"dropped_reports"` // reports discarded since start (send queue full or rejected)
}

// Node identifies the machine and its static hardware.
type Node struct {
	Name             string            `json:"name"`             // node name as the scheduler knows it (K8s node name, hostname otherwise)
	Labels           map[string]string `json:"labels,omitempty"` // user-supplied, e.g. site=sjc-1
	Arch             string            `json:"arch"`             // amd64, arm64
	Kernel           string            `json:"kernel,omitempty"`
	OS               string            `json:"os,omitempty"`
	CPU              CPU               `json:"cpu"`
	MemoryTotalBytes uint64            `json:"memory_total_bytes"`
	GPUDriver        *GPUDriver        `json:"gpu_driver,omitempty"`
}

// CPU is the host processor inventory.
type CPU struct {
	Model        string `json:"model,omitempty"`
	LogicalCores int    `json:"logical_cores"`
}

// GPUDriver is the NVIDIA driver installed on the node.
type GPUDriver struct {
	Version     string `json:"version"`
	CUDAVersion string `json:"cuda_version,omitempty"` // highest CUDA version the driver supports, e.g. "12.8"
}

// Window is the time span the report aggregates. Windows are aligned to
// multiples of the report interval (e.g. :00, :10, :20 for 10 s), so reports
// from different nodes line up; the first and last window of a run can be
// shorter.
type Window struct {
	Start                 time.Time `json:"start"`
	End                   time.Time `json:"end"`
	SampleIntervalSeconds float64   `json:"sample_interval_seconds"`
	Samples               int       `json:"samples"` // host/memory/process samples taken in the window
}

// Host is whole-machine CPU and memory usage over the window.
type Host struct {
	CPUUtilPercent  Stat    `json:"cpu_util_percent"`  // 0-100 across all cores
	MemoryUsedBytes IntStat `json:"memory_used_bytes"` // MemTotal - MemAvailable
}

// GPU is one physical GPU over the window.
type GPU struct {
	Index            int    `json:"index"` // NVML index (may differ from CUDA_VISIBLE_DEVICES order)
	UUID             string `json:"uuid"`
	Name             string `json:"name"` // marketing name, e.g. "NVIDIA H100 80GB HBM3"
	PCIBusID         string `json:"pci_bus_id,omitempty"`
	MemoryTotalBytes uint64 `json:"memory_total_bytes"`
	MIGEnabled       bool   `json:"mig_enabled"`

	// UtilPercent is the share of time a kernel was running (NVML "GPU
	// utilization"), aggregated over every driver sample in the window.
	// Null when unknown, e.g. on MIG-enabled GPUs.
	UtilPercent     *Stat    `json:"util_percent"`
	MemoryUsedBytes *IntStat `json:"memory_used_bytes"` // null when unknown
	PowerWatts      *Stat    `json:"power_watts,omitempty"`
	TemperatureC    *Stat    `json:"temperature_c,omitempty"`

	// Processes holds every process seen on the GPU during the window.
	Processes []Process `json:"processes"`

	// Allocation as seen by the scheduler, when the scheduler exposes which
	// physical GPU a job holds (Slurm does; Kubernetes needs the kubelet
	// PodResources API, not used yet). Omitted when unknown.
	Allocated   *bool  `json:"allocated,omitempty"`
	AllocatedTo *Owner `json:"allocated_to,omitempty"`

	Errors []string `json:"errors,omitempty"` // per-GPU problems, e.g. "utilization not supported"
}

// Process is a process that used the GPU during the window.
type Process struct {
	PID             int    `json:"pid"` // host PID namespace
	Name            string `json:"name,omitempty"`
	User            string `json:"user,omitempty"`
	Cmdline         string `json:"cmdline,omitempty"`           // only with --include-cmdline
	MemoryUsedBytes uint64 `json:"memory_used_bytes,omitempty"` // peak in window; 0 when the driver doesn't say
	Owner           *Owner `json:"owner,omitempty"`
}

// Owner is the workload a process (or GPU allocation) belongs to.
type Owner struct {
	Kind        string `json:"kind"`                   // kubernetes, slurm, pbs, docker, podman, systemd, session
	ID          string `json:"id,omitempty"`           // pod UID, job ID, container ID or unit name
	Name        string `json:"name,omitempty"`         // pod name, job name
	Namespace   string `json:"namespace,omitempty"`    // Kubernetes namespace
	Container   string `json:"container,omitempty"`    // Kubernetes container name
	ContainerID string `json:"container_id,omitempty"` // container runtime ID
	User        string `json:"user,omitempty"`         // job owner (Slurm/PBS)
	Queue       string `json:"queue,omitempty"`        // Slurm partition / PBS queue
}

// Scheduler is the node as its workload scheduler sees it.
type Scheduler struct {
	Kind            string `json:"kind"`                       // kubernetes, slurm, pbs or none
	GPUsAllocatable *int   `json:"gpus_allocatable,omitempty"` // GPUs the scheduler can hand out on this node
	GPUsAllocated   *int   `json:"gpus_allocated,omitempty"`   // GPUs currently given to jobs/pods
	State           string `json:"state,omitempty"`            // scheduler node state, e.g. Slurm "MIXED", "DRAIN"
}

// Stat summarises a measurement over the window.
type Stat struct {
	Avg float64 `json:"avg"`
	Max float64 `json:"max"`
}

// IntStat is Stat for byte counts.
type IntStat struct {
	Avg uint64 `json:"avg"`
	Max uint64 `json:"max"`
}

// NewID returns a random RFC 4122 version 4 UUID.
func NewID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("crypto/rand failed: %v", err)) // never happens on supported platforms
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
