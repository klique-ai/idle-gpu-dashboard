package scheduler

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"github.com/clearml/klique-idle-dashboard/collector/internal/report"
)

// runFunc runs a command and returns its stdout; replaced in tests.
type runFunc func(ctx context.Context, name string, args ...string) ([]byte, error)

func runCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 300 {
			msg = msg[:300]
		}
		if msg != "" {
			err = fmt.Errorf("%w: %s", err, msg)
		}
		return nil, fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return stdout.Bytes(), nil
}

// SlurmSource asks the Slurm controller about this node with the standard
// client tools (scontrol, squeue), which every Slurm compute node has.
type SlurmSource struct {
	nodeName string
	run      runFunc
}

// NewSlurm returns a SlurmSource for the given Slurm node name.
func NewSlurm(nodeName string) *SlurmSource {
	return &SlurmSource{nodeName: nodeName, run: runCommand}
}

func (s *SlurmSource) Kind() string { return Slurm }

// Query implements Source.
func (s *SlurmSource) Query(ctx context.Context) (Snapshot, error) {
	snap := Snapshot{Workloads: map[string]Workload{}, GPUs: map[int]*report.Owner{}}

	out, err := s.run(ctx, "scontrol", "show", "node", s.nodeName, "--details", "--oneliner")
	if err != nil {
		return snap, err
	}
	node := parseKV(string(out))
	snap.Node.State = node["State"]
	// Gres/GresUsed are always present; TRES only lists GPUs when the site
	// sets AccountingStorageTRES=gres/gpu.
	if n, ok := gresGPUs(node["Gres"]); ok {
		snap.Node.GPUsAllocatable = &n
	} else if n, ok := tresGPUs(node["CfgTRES"]); ok {
		snap.Node.GPUsAllocatable = &n
	}
	if snap.Node.GPUsAllocatable != nil {
		n, ok := gresGPUs(node["GresUsed"])
		if !ok {
			n, _ = tresGPUs(node["AllocTRES"])
		}
		snap.Node.GPUsAllocated = &n
	}

	// %A is the numeric job ID used in cgroup paths (squeue's %i shows
	// array jobs as 123_4 instead).
	out, err = s.run(ctx, "squeue", "--noheader", "--nodelist", s.nodeName, "--states", "RUNNING", "--format", "%A|%j|%u|%P")
	if err != nil {
		return snap, err
	}
	var jobIDs []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.SplitN(line, "|", 4)
		if len(f) != 4 {
			continue
		}
		snap.Workloads[f[0]] = Workload{Name: f[1], User: f[2], Queue: f[3]}
		jobIDs = append(jobIDs, f[0])
	}

	// Which GPU indexes each job holds on this node.
	for _, id := range jobIDs {
		out, err := s.run(ctx, "scontrol", "show", "job", id, "--details", "--oneliner")
		if err != nil {
			continue // the job may have just ended
		}
		w := snap.Workloads[id]
		for _, idx := range jobGPUIndexes(string(out), s.nodeName) {
			snap.GPUs[idx] = &report.Owner{Kind: Slurm, ID: id, Name: w.Name, User: w.User, Queue: w.Queue}
		}
	}
	return snap, nil
}

// parseKV splits scontrol's one-line "Key=Value Key=Value" output.
func parseKV(s string) map[string]string {
	out := map[string]string{}
	for _, f := range strings.Fields(s) {
		if k, v, ok := strings.Cut(f, "="); ok {
			out[k] = v
		}
	}
	return out
}

// gresGPUs sums the GPU counts in a Gres or GresUsed field such as
// "gpu:4", "gpu:h100:8(S:0-1)", "gpu:a100:2(IDX:0-1),gpu:v100:1(IDX:4),mps:100"
// or "gpu:(null):0(IDX:N/A)".
func gresGPUs(field string) (int, bool) {
	if field == "" || field == "(null)" {
		return 0, false
	}
	total, found := 0, false
	depth, start := 0, 0
	for i := 0; i <= len(field); i++ {
		if i < len(field) {
			switch field[i] {
			case '(':
				depth++
				continue
			case ')':
				depth--
				continue
			case ',':
				if depth > 0 {
					continue
				}
			default:
				continue
			}
		}
		item := field[start:i]
		start = i + 1
		// Drop a trailing "(S:0-1)" / "(IDX:...)"; "(null)" inside the type stays.
		if strings.HasSuffix(item, ")") {
			if p := strings.LastIndexByte(item, '('); p >= 0 {
				item = item[:p]
			}
		}
		parts := strings.Split(item, ":")
		if parts[0] != "gpu" || len(parts) < 2 {
			continue
		}
		if n, err := strconv.Atoi(parts[len(parts)-1]); err == nil {
			total += n
			found = true
		}
	}
	return total, found
}

var tresGPURe = regexp.MustCompile(`(?:^|,)gres/gpu=(\d+)`)

// tresGPUs reads the GPU count from a TRES string such as
// "cpu=64,mem=500G,billing=64,gres/gpu=8".
func tresGPUs(tres string) (int, bool) {
	m := tresGPURe.FindStringSubmatch(tres)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	return n, err == nil
}

// Matches gpu:h100:2(IDX:0-1), gpu:2(IDX:0,2) and gpu(IDX:3) across Slurm versions.
var gresIdxRe = regexp.MustCompile(`gpu[^\s(]*\(IDX:([0-9,-]+)\)`)

// jobGPUIndexes returns the GPU indexes a job holds on node, from
// "scontrol show job -d" lines like
// "Nodes=gpu01 CPU_IDs=0-15 Mem=64000 GRES=gpu:h100:2(IDX:0-1)".
// The detail line for each node starts with "Nodes=".
func jobGPUIndexes(out, node string) []int {
	var idx []int
	for _, seg := range strings.Split(out, " Nodes=")[1:] {
		nodes, _, _ := strings.Cut(seg, " ")
		if !nodeListContains(nodes, node) {
			continue
		}
		for _, m := range gresIdxRe.FindAllStringSubmatch(seg, -1) {
			idx = append(idx, expandRanges(m[1])...)
		}
	}
	return idx
}

// nodeListContains checks a Slurm hostlist like "gpu[01-04,07]" or "gpu01".
func nodeListContains(list, node string) bool {
	if list == node {
		return true
	}
	prefix, ranges, ok := strings.Cut(list, "[")
	if !ok || !strings.HasPrefix(node, prefix) {
		return false
	}
	suffix := strings.TrimPrefix(node, prefix)
	for _, r := range strings.Split(strings.TrimSuffix(ranges, "]"), ",") {
		lo, hi, isRange := strings.Cut(r, "-")
		if !isRange {
			hi = lo
		}
		n, err := strconv.Atoi(suffix)
		a, errA := strconv.Atoi(lo)
		b, errB := strconv.Atoi(hi)
		if err == nil && errA == nil && errB == nil && n >= a && n <= b && len(suffix) == len(lo) {
			return true
		}
	}
	return false
}

// expandRanges turns "0-2,5" into [0 1 2 5].
func expandRanges(s string) []int {
	var out []int
	for _, part := range strings.Split(s, ",") {
		lo, hi, isRange := strings.Cut(part, "-")
		a, err := strconv.Atoi(lo)
		if err != nil {
			continue
		}
		b := a
		if isRange {
			if b, err = strconv.Atoi(hi); err != nil {
				continue
			}
		}
		for i := a; i <= b; i++ {
			out = append(out, i)
		}
	}
	return out
}
