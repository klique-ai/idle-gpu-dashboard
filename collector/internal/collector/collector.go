// Package collector samples the node on a short interval and turns each
// reporting window into a report.Report.
package collector

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"runtime"
	"sync"
	"time"

	"github.com/klique-ai/idle-gpu-dashboard/collector/internal/gpu"
	"github.com/klique-ai/idle-gpu-dashboard/collector/internal/host"
	"github.com/klique-ai/idle-gpu-dashboard/collector/internal/owner"
	"github.com/klique-ai/idle-gpu-dashboard/collector/internal/report"
	"github.com/klique-ai/idle-gpu-dashboard/collector/internal/scheduler"
	"github.com/klique-ai/idle-gpu-dashboard/collector/internal/sender"
)

// Options configures a Collector.
type Options struct {
	Cluster        string
	NodeName       string
	Labels         map[string]string
	SampleInterval time.Duration
	ReportInterval time.Duration
	Version        string
	Redact         bool
	SchedulerKind  string
}

// Collector owns the sampling loop. Run it from a single goroutine; Status
// may be called concurrently.
type Collector struct {
	opt    Options
	gpus   gpu.Source
	host   *host.Reader
	owners *owner.Resolver
	sched  *scheduler.Watcher // nil when no scheduler integration
	out    sender.Sender
	log    *slog.Logger
	now    func() time.Time

	instanceID string
	startedAt  time.Time
	seq        uint64
	win        *window
	windowEnd  time.Time
	hostInfo   *host.Info

	mu     sync.Mutex
	status Status
	last   *report.Report // latest report, for /metrics; never modified once emitted
}

// Status is a summary for the health endpoint.
type Status struct {
	Version    string    `json:"version"`
	InstanceID string    `json:"instance_id"`
	StartedAt  time.Time `json:"started_at"`
	Cluster    string    `json:"cluster"`
	Node       string    `json:"node"`
	GPUSource  string    `json:"gpu_source"`
	GPUs       int       `json:"gpus"`
	Scheduler  string    `json:"scheduler"`
	LastSample time.Time `json:"last_sample"`
	LastReport time.Time `json:"last_report"`
	Reports    uint64    `json:"reports"`
	Errors     []string  `json:"errors,omitempty"` // from the last report
}

// New wires a Collector. sched may be nil.
func New(opt Options, gpus gpu.Source, hostReader *host.Reader, owners *owner.Resolver,
	sched *scheduler.Watcher, out sender.Sender, log *slog.Logger) *Collector {
	return &Collector{
		opt: opt, gpus: gpus, host: hostReader, owners: owners, sched: sched, out: out, log: log,
		now:        time.Now,
		instanceID: report.NewID(),
	}
}

// Run samples until ctx is cancelled, then emits the final partial window.
// With once set it emits a single report after one sample interval and
// returns.
//
// Sample ticks are aligned to the clock like the windows (:00, :02, ...), so
// a sample taken on a window boundary describes the interval that just ended
// and every full window holds the same number of samples.
func (c *Collector) Run(ctx context.Context, once bool) {
	start := c.now()
	c.startedAt = start
	c.win = newWindow(start)
	c.windowEnd = alignedEnd(start, c.opt.ReportInterval)
	next := alignedEnd(start, c.opt.SampleInterval)
	if once {
		c.windowEnd = start.Add(c.opt.SampleInterval)
		next = c.windowEnd
	}
	c.setStatus(func(s *Status) {
		s.Version, s.InstanceID, s.StartedAt = c.opt.Version, c.instanceID, start
		s.Cluster, s.Node, s.GPUSource, s.Scheduler = c.opt.Cluster, c.opt.NodeName, c.gpus.Kind(), c.opt.SchedulerKind
	})

	c.sample() // baseline for CPU and GPU utilization deltas
	timer := time.NewTimer(next.Sub(c.now()))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			c.sample() // cover the time since the last tick
			c.emit(c.now())
			return
		case <-timer.C:
		}
		c.sample()
		now := c.now()
		if !now.Before(c.windowEnd) {
			c.emit(c.windowEnd)
			if once {
				return
			}
			c.win = newWindow(c.windowEnd)
			c.windowEnd = alignedEnd(now, c.opt.ReportInterval)
		}
		next = next.Add(c.opt.SampleInterval)
		if !next.After(now) { // fell behind (slow driver call, suspended VM): skip missed ticks
			next = alignedEnd(now, c.opt.SampleInterval)
		}
		timer.Reset(next.Sub(now))
	}
}

// sample takes one reading of everything into the current window.
func (c *Collector) sample() {
	w := c.win
	w.samples++

	if hs, err := c.host.Sample(); err != nil {
		w.addError(&w.errors, "host: "+err.Error())
	} else {
		if hs.CPUOK {
			w.cpu.add(hs.CPUUtil)
		}
		w.mem.add(float64(hs.MemoryUsed))
	}

	samples, err := c.gpus.Sample()
	if err != nil {
		w.addError(&w.errors, "gpu: "+err.Error())
	}
	var snap scheduler.Snapshot
	if c.sched != nil {
		snap = c.sched.Snapshot()
	}
	invisible := 0
	total := 0
	for _, s := range samples {
		procs := make([]report.Process, 0, len(s.Processes))
		for _, p := range s.Processes {
			rp, visible := c.describe(p, snap)
			total++
			if !visible {
				invisible++
			}
			procs = append(procs, rp)
		}
		w.addGPU(s, procs)
	}
	if total > 0 && invisible == total {
		w.addError(&w.errors, "GPU processes are not visible in /proc, so their owners are unknown: "+
			"on Kubernetes enable hostPID; on hosts with /proc mounted hidepid=2, run the collector as root")
	}
	c.setStatus(func(s *Status) { s.LastSample = c.now(); s.GPUs = len(samples) })
}

// describe turns a GPU process into its report form, looking it up in procfs
// unless the source already named it.
func (c *Collector) describe(p gpu.Process, snap scheduler.Snapshot) (report.Process, bool) {
	rp := report.Process{PID: p.PID, Name: p.Name, User: p.User, MemoryUsedBytes: p.MemoryUsed, Owner: p.Owner}
	visible := true
	if rp.Name == "" {
		info, err := c.owners.Lookup(p.PID)
		switch {
		case err == nil:
			rp.Name, rp.User, rp.Cmdline, rp.Owner = info.Name, info.User, info.Cmdline, info.Owner
		case errors.Is(err, fs.ErrNotExist):
			visible = false
		}
	}
	if rp.Owner != nil {
		o := *rp.Owner // don't mutate the source's copy
		snap.Enrich(&o)
		rp.Owner = &o
	}
	return rp, visible
}

// emit turns the current window into a report and hands it to the sender.
func (c *Collector) emit(end time.Time) {
	w := c.win
	c.seq++
	r := &report.Report{
		Schema:    report.Schema,
		ReportID:  report.NewID(),
		CreatedAt: c.now().UTC(),
		Collector: report.Collector{
			Version:        c.opt.Version,
			InstanceID:     c.instanceID,
			StartedAt:      c.startedAt.UTC(),
			Seq:            c.seq,
			GPUSource:      c.gpus.Kind(),
			Redacted:       c.opt.Redact,
			DroppedReports: c.out.Stats().Dropped,
		},
		Cluster: c.opt.Cluster,
		Window: report.Window{
			Start:                 w.start.UTC(),
			End:                   end.UTC(),
			SampleIntervalSeconds: c.opt.SampleInterval.Seconds(),
			Samples:               w.samples,
		},
		GPUs:   []report.GPU{},
		Errors: append([]string(nil), w.errors...),
	}

	r.Node = report.Node{Name: c.opt.NodeName, Labels: c.opt.Labels, Arch: runtime.GOARCH}
	if c.hostInfo == nil {
		if info, err := c.host.Info(); err != nil {
			r.Errors = append(r.Errors, "host info: "+err.Error())
		} else {
			c.hostInfo = &info
		}
	}
	if hi := c.hostInfo; hi != nil {
		r.Node.Kernel, r.Node.OS = hi.Kernel, hi.OS
		r.Node.CPU = report.CPU{Model: hi.CPUModel, LogicalCores: hi.LogicalCores}
		r.Node.MemoryTotalBytes = hi.MemoryTotal
	}
	if cpu, mem := w.cpu.report(), w.mem.reportInt(); cpu != nil || mem != nil {
		r.Host = &report.Host{}
		if cpu != nil {
			r.Host.CPUUtilPercent = *cpu
		}
		if mem != nil {
			r.Host.MemoryUsedBytes = *mem
		}
	}

	r.Scheduler = report.Scheduler{Kind: c.opt.SchedulerKind}
	var snap scheduler.Snapshot
	if c.sched != nil {
		snap = c.sched.Snapshot()
		r.Scheduler = snap.Node
		if snap.NodeOS != "" {
			r.Node.OS = snap.NodeOS // the container's own os-release would be wrong
		}
		if snap.Err != "" {
			r.Errors = append(r.Errors, "scheduler: "+snap.Err)
		}
	}

	inv, err := c.gpus.Inventory()
	if err != nil {
		if !containsSuffix(r.Errors, err.Error()) {
			r.Errors = append(r.Errors, "gpu: "+err.Error())
		}
	}
	r.Node.GPUDriver = inv.Driver
	for _, d := range inv.Devices {
		g := w.gpuReport(d)
		if snap.GPUs != nil && !d.MIGEnabled {
			holder, held := snap.GPUs[d.Index]
			g.Allocated = &held
			if held {
				h := *holder
				g.AllocatedTo = &h
			}
		}
		r.GPUs = append(r.GPUs, g)
	}

	if c.opt.Redact {
		redact(r, c.opt.Cluster)
	}
	c.out.Enqueue(r)
	c.mu.Lock()
	c.last = r
	c.mu.Unlock()
	c.setStatus(func(s *Status) {
		s.LastReport, s.Reports, s.Errors = c.now(), c.seq, r.Errors
	})
	c.log.Debug("report queued", "seq", c.seq, "gpus", len(r.GPUs), "samples", w.samples, "errors", len(r.Errors))
}

func containsSuffix(list []string, s string) bool {
	for _, e := range list {
		if len(e) >= len(s) && e[len(e)-len(s):] == s {
			return true
		}
	}
	return false
}

func (c *Collector) setStatus(f func(*Status)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	f(&c.status)
}

// LastReport returns the latest report, or nil before the first one. Callers
// must not modify it.
func (c *Collector) LastReport() *report.Report {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.last
}

// Status returns a copy of the current status.
func (c *Collector) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.status
	s.Errors = append([]string(nil), s.Errors...)
	return s
}
