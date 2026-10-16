package collector

import (
	"sort"
	"time"

	"github.com/clearml/klique-idle-dashboard/collector/internal/gpu"
	"github.com/clearml/klique-idle-dashboard/collector/internal/report"
)

// stat accumulates avg/max of a series.
type stat struct {
	n        int
	sum, max float64
}

func (s *stat) add(v float64) {
	if s.n == 0 || v > s.max {
		s.max = v
	}
	s.sum += v
	s.n++
}

func (s *stat) report() *report.Stat {
	if s.n == 0 {
		return nil
	}
	return &report.Stat{Avg: round2(s.sum / float64(s.n)), Max: round2(s.max)}
}

func (s *stat) reportInt() *report.IntStat {
	if s.n == 0 {
		return nil
	}
	return &report.IntStat{Avg: uint64(s.sum/float64(s.n) + 0.5), Max: uint64(s.max)}
}

func round2(v float64) float64 {
	return float64(int64(v*100+0.5)) / 100
}

// gpuWindow accumulates one GPU over a window.
type gpuWindow struct {
	util, mem, power, temp stat
	procs                  map[int]*report.Process // by PID
	errors                 []string
}

// window accumulates everything between two reports.
type window struct {
	start   time.Time
	samples int
	cpu     stat
	mem     stat
	gpus    map[string]*gpuWindow // by UUID
	errors  []string
}

func newWindow(start time.Time) *window {
	return &window{start: start, gpus: map[string]*gpuWindow{}}
}

func (w *window) addError(list *[]string, msg string) {
	for _, e := range *list {
		if e == msg {
			return
		}
	}
	*list = append(*list, msg)
}

func (w *window) gpu(uuid string) *gpuWindow {
	g, ok := w.gpus[uuid]
	if !ok {
		g = &gpuWindow{procs: map[int]*report.Process{}}
		w.gpus[uuid] = g
	}
	return g
}

// addGPU records one GPU sample; procs carries already-resolved process details.
func (w *window) addGPU(s gpu.Sample, procs []report.Process) {
	g := w.gpu(s.UUID)
	for _, v := range s.Util {
		g.util.add(v)
	}
	if s.MemoryUsed != nil {
		g.mem.add(float64(*s.MemoryUsed))
	}
	if s.PowerWatts != nil {
		g.power.add(*s.PowerWatts)
	}
	if s.TempC != nil {
		g.temp.add(*s.TempC)
	}
	for _, e := range s.Errors {
		w.addError(&g.errors, e)
	}
	for i := range procs {
		p := procs[i]
		if prev, ok := g.procs[p.PID]; ok {
			prev.MemoryUsedBytes = max(prev.MemoryUsedBytes, p.MemoryUsedBytes)
			continue
		}
		g.procs[p.PID] = &p
	}
}

// gpuReport builds the per-GPU part of a report from inventory + window data.
func (w *window) gpuReport(d gpu.Device) report.GPU {
	out := report.GPU{
		Index:            d.Index,
		UUID:             d.UUID,
		Name:             d.Name,
		PCIBusID:         d.PCIBusID,
		MemoryTotalBytes: d.MemoryTotal,
		MIGEnabled:       d.MIGEnabled,
		Processes:        []report.Process{},
	}
	g, ok := w.gpus[d.UUID]
	if !ok {
		return out
	}
	out.UtilPercent = g.util.report()
	out.MemoryUsedBytes = g.mem.reportInt()
	out.PowerWatts = g.power.report()
	out.TemperatureC = g.temp.report()
	out.Errors = g.errors
	for _, p := range g.procs {
		out.Processes = append(out.Processes, *p)
	}
	sort.Slice(out.Processes, func(i, j int) bool { return out.Processes[i].PID < out.Processes[j].PID })
	return out
}

// alignedEnd returns the end of the window containing t: the next multiple of
// interval (counted from a fixed reference time, so for intervals that divide
// a day this is wall-clock aligned: :00, :10, ...). Windows then line up
// across nodes.
func alignedEnd(t time.Time, interval time.Duration) time.Time {
	return t.Truncate(interval).Add(interval)
}
