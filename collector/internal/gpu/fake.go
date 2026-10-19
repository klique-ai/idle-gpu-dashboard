package gpu

import (
	"fmt"
	"hash/fnv"
	"math/rand"
	"sort"
	"strings"
	"time"

	"github.com/klique-ai/idle-gpu-dashboard/collector/internal/report"
)

// FakeModel describes a simulated GPU type.
type FakeModel struct {
	Name     string
	MemoryGB float64
	IdleW    float64
	MaxW     float64
}

// FakeModels are the GPU types --fake-gpu-model accepts.
var FakeModels = map[string]FakeModel{
	"a100": {"NVIDIA A100-SXM4-80GB", 80, 60, 400},
	"h100": {"NVIDIA H100 80GB HBM3", 80, 70, 700},
	"h200": {"NVIDIA H200", 141, 75, 700},
	"b200": {"NVIDIA B200", 180, 140, 1000},
	"l40s": {"NVIDIA L40S", 48, 35, 350},
}

// FakeModelNames lists the accepted model keys, sorted.
func FakeModelNames() []string {
	names := make([]string, 0, len(FakeModels))
	for k := range FakeModels {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

type fakeMode int

const (
	modeBusy fakeMode = iota // a job computing
	modeIdle                 // free: no process, no memory
	modeHeld                 // idle but a process keeps memory allocated (forgotten notebook)
)

type fakeGPU struct {
	mode    fakeMode
	until   time.Time
	level   float64 // busy utilization centre
	memFrac float64
	proc    Process
}

// Fake simulates GPUs moving between busy, idle and idle-with-memory-held
// phases lasting minutes, so dashboards show realistic idle patterns. Values
// are deterministic for a given seed.
type Fake struct {
	model     FakeModel
	devices   []Device
	gpus      []fakeGPU
	rng       *rand.Rand
	scheduler string
	now       func() time.Time
	nextPID   int
}

// NewFake simulates count GPUs of the given model. seed makes the run
// repeatable (the node name is a good choice); scheduler shapes the
// simulated process owners ("kubernetes", "slurm", "pbs" or "none").
func NewFake(count int, model FakeModel, seed, scheduler string) *Fake {
	h := fnv.New64a()
	h.Write([]byte(seed))
	f := &Fake{
		model:     model,
		rng:       rand.New(rand.NewSource(int64(h.Sum64()))),
		scheduler: scheduler,
		now:       time.Now,
		nextPID:   200000 + int(h.Sum64()%10000),
	}
	for i := 0; i < count; i++ {
		f.devices = append(f.devices, Device{
			Index:       i,
			UUID:        f.uuid(),
			Name:        model.Name,
			PCIBusID:    fmt.Sprintf("00000000:%02X:00.0", 0x18+i*0x10),
			MemoryTotal: uint64(model.MemoryGB * (1 << 30)),
		})
	}
	f.gpus = make([]fakeGPU, count)
	return f
}

func (f *Fake) Kind() string { return KindFake }

func (f *Fake) Inventory() (Inventory, error) {
	return Inventory{
		Devices: append([]Device(nil), f.devices...),
		Driver:  &report.GPUDriver{Version: "simulated"},
	}, nil
}

func (f *Fake) Sample() ([]Sample, error) {
	now := f.now()
	out := make([]Sample, len(f.devices))
	for i := range f.gpus {
		g := &f.gpus[i]
		if now.After(g.until) {
			f.transition(g, now)
		}
		total := f.devices[i].MemoryTotal
		smp := Sample{UUID: f.devices[i].UUID}
		var mem uint64
		var util []float64
		power, temp := f.model.IdleW*(0.9+0.2*f.rng.Float64()), 30+5*f.rng.Float64()
		switch g.mode {
		case modeBusy:
			// ~6 driver readings per 2 s poll, wobbling around the job's level
			for n := 0; n < 6; n++ {
				util = append(util, clamp(g.level+(f.rng.Float64()-0.5)*25, 5, 100))
			}
			mem = uint64(float64(total) * clamp(g.memFrac+(f.rng.Float64()-0.5)*0.02, 0, 1))
			power = f.model.IdleW + (f.model.MaxW-f.model.IdleW)*util[0]/100
			temp = 45 + 30*util[0]/100
		case modeHeld:
			util = []float64{0, 0, 0, 0, 0, 0}
			mem = uint64(float64(total) * g.memFrac)
		default:
			util = []float64{0, 0, 0, 0, 0, 0}
		}
		smp.Util, smp.MemoryUsed, smp.PowerWatts, smp.TempC = util, &mem, &power, &temp
		if g.mode != modeIdle {
			p := g.proc
			p.MemoryUsed = mem
			smp.Processes = []Process{p}
		}
		out[i] = smp
	}
	return out, nil
}

func (f *Fake) Close() {}

// transition picks the next phase: mostly busy, a fair share fully idle,
// sometimes idle with memory held.
func (f *Fake) transition(g *fakeGPU, now time.Time) {
	r := f.rng.Float64()
	minutes := func(lo, hi int) time.Duration {
		return time.Duration(lo+f.rng.Intn(hi-lo+1)) * time.Minute
	}
	first := g.until.IsZero()
	switch {
	case r < 0.55:
		g.mode, g.until = modeBusy, now.Add(minutes(5, 40))
		g.level, g.memFrac = 55+40*f.rng.Float64(), 0.55+0.4*f.rng.Float64()
	case r < 0.88:
		g.mode, g.until = modeIdle, now.Add(minutes(3, 30))
	default:
		g.mode, g.until = modeHeld, now.Add(minutes(5, 25))
		g.memFrac = 0.2 + 0.5*f.rng.Float64()
	}
	if first {
		// Stagger the first phase so GPUs don't all switch together.
		g.until = now.Add(time.Duration(f.rng.Int63n(int64(g.until.Sub(now)))) + time.Minute)
	}
	if g.mode != modeIdle {
		g.proc = f.process(g.mode)
	}
}

var (
	fakeUsers = []string{"alice", "bob", "carol", "dmitri", "eve"}
	fakeJobs  = []string{"pretrain-7b", "finetune-llama", "eval-suite", "embeddings", "notebook", "rl-sweep"}
)

func (f *Fake) process(mode fakeMode) Process {
	f.nextPID++
	user := fakeUsers[f.rng.Intn(len(fakeUsers))]
	job := fakeJobs[f.rng.Intn(len(fakeJobs))]
	if mode == modeHeld {
		job = "notebook"
	}
	p := Process{PID: f.nextPID, Name: "python3", User: user}
	switch f.scheduler {
	case "kubernetes":
		p.User = "root"
		p.Owner = &report.Owner{
			Kind:      "kubernetes",
			ID:        f.uuid()[4:],
			Name:      fmt.Sprintf("%s-%s", job, strings.ToLower(f.suffix())),
			Namespace: "team-" + user,
			Container: "main",
		}
	case "slurm":
		p.Owner = &report.Owner{Kind: "slurm", ID: fmt.Sprint(40000 + f.rng.Intn(9000)), Name: job, User: user, Queue: "gpu"}
	case "pbs":
		p.Owner = &report.Owner{Kind: "pbs", ID: fmt.Sprintf("%d.pbs-server", 1000+f.rng.Intn(9000)), Name: job, User: user, Queue: "workq"}
	}
	return p
}

func (f *Fake) uuid() string {
	b := make([]byte, 16)
	f.rng.Read(b)
	return fmt.Sprintf("GPU-%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func (f *Fake) suffix() string {
	const chars = "bcdfghjklmnpqrstvwxz2456789"
	b := make([]byte, 5)
	for i := range b {
		b[i] = chars[f.rng.Intn(len(chars))]
	}
	return string(b)
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
