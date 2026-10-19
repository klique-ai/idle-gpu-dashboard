// Package scheduler adds the workload scheduler's view of the node: names for
// the pods/jobs found on GPUs, and how many GPUs the scheduler has handed out.
// Queries run in the background so a slow scheduler never delays sampling.
package scheduler

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/klique-ai/idle-gpu-dashboard/collector/internal/report"
)

// Scheduler kinds.
const (
	Kubernetes = "kubernetes"
	Slurm      = "slurm"
	PBS        = "pbs"
	None       = "none"
)

// Snapshot is the scheduler's view at one point in time.
type Snapshot struct {
	Node      report.Scheduler
	Workloads map[string]Workload   // by owner ID: pod UID or job ID
	GPUs      map[int]*report.Owner // by GPU index, when the scheduler says which job holds it
	NodeOS    string                // OS image reported by the scheduler (Kubernetes)
	Err       string                // last refresh problem, reported in the next reports
	At        time.Time
}

// Workload is a pod or job as named by the scheduler.
type Workload struct {
	Name       string
	Namespace  string
	User       string
	Queue      string
	Containers map[string]string // Kubernetes container ID -> container name
}

// Source queries one scheduler.
type Source interface {
	Kind() string
	Query(ctx context.Context) (Snapshot, error)
}

// Detect guesses the scheduler running this node.
func Detect(procRoot string) string {
	switch {
	case os.Getenv("KUBERNETES_SERVICE_HOST") != "":
		return Kubernetes
	case running(procRoot, "slurmd") || exists("/etc/slurm/slurm.conf") || exists("/etc/slurm-llnl/slurm.conf"):
		return Slurm
	case running(procRoot, "pbs_mom") || exists("/etc/pbs.conf"):
		return PBS
	}
	return None
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// running reports whether a process with this command name is visible.
func running(procRoot, comm string) bool {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.Name()[0] < '0' || e.Name()[0] > '9' {
			continue
		}
		b, err := os.ReadFile(filepath.Join(procRoot, e.Name(), "comm"))
		if err == nil && strings.TrimSpace(string(b)) == comm {
			return true
		}
	}
	return false
}

// Watcher refreshes a Source periodically and serves the latest Snapshot.
type Watcher struct {
	src      Source
	interval time.Duration
	timeout  time.Duration
	log      *slog.Logger

	mu   sync.RWMutex
	last Snapshot
}

// NewWatcher returns a Watcher; call Run to start it.
func NewWatcher(src Source, interval time.Duration, log *slog.Logger) *Watcher {
	return &Watcher{
		src:      src,
		interval: interval,
		timeout:  min(interval, 15*time.Second),
		log:      log,
		last:     Snapshot{Node: report.Scheduler{Kind: src.Kind()}},
	}
}

// Run refreshes until ctx is cancelled.
func (w *Watcher) Run(ctx context.Context) {
	t := time.NewTicker(w.interval)
	defer t.Stop()
	for {
		w.Refresh(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Refresh queries the scheduler once and stores the result.
func (w *Watcher) Refresh(ctx context.Context) {
	qctx, cancel := context.WithTimeout(ctx, w.timeout)
	defer cancel()
	snap, err := w.src.Query(qctx)
	snap.Node.Kind = w.src.Kind()
	snap.At = time.Now()
	w.mu.Lock()
	defer w.mu.Unlock()
	if err != nil {
		if w.last.Err != err.Error() {
			w.log.Warn("scheduler query failed", "scheduler", w.src.Kind(), "err", err)
		}
		// Keep the last good names; just flag the problem.
		w.last.Err = err.Error()
		return
	}
	if w.last.Err != "" {
		w.log.Info("scheduler query recovered", "scheduler", w.src.Kind())
	}
	w.last = snap
}

// Snapshot returns the latest view.
func (w *Watcher) Snapshot() Snapshot {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.last
}

// Enrich fills in names on an owner found from cgroups.
func (s Snapshot) Enrich(o *report.Owner) {
	if o == nil {
		return
	}
	w, ok := s.Workloads[o.ID]
	if !ok {
		return
	}
	o.Name, o.Namespace, o.Queue = w.Name, w.Namespace, w.Queue
	if o.User == "" {
		o.User = w.User
	}
	if name, ok := w.Containers[o.ContainerID]; ok {
		o.Container = name
	}
}
