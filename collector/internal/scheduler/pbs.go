package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// PBSSource asks the PBS server (OpenPBS / PBS Professional) about this node
// with pbsnodes and qstat. Draft: verified against documented JSON output,
// not yet against a live PBS cluster.
type PBSSource struct {
	nodeName string
	run      runFunc
}

// NewPBS returns a PBSSource for the given PBS vnode name.
func NewPBS(nodeName string) *PBSSource {
	return &PBSSource{nodeName: nodeName, run: runCommand}
}

func (p *PBSSource) Kind() string { return PBS }

type pbsNodes struct {
	Nodes map[string]struct {
		State              string         `json:"state"`
		Jobs               []string       `json:"jobs"` // "123.server/0"
		ResourcesAvailable map[string]any `json:"resources_available"`
		ResourcesAssigned  map[string]any `json:"resources_assigned"`
	} `json:"nodes"`
}

type pbsJobs struct {
	Jobs map[string]struct {
		JobName  string `json:"Job_Name"`
		JobOwner string `json:"Job_Owner"` // "alice@host"
		Queue    string `json:"queue"`
	} `json:"Jobs"`
}

// Query implements Source.
func (p *PBSSource) Query(ctx context.Context) (Snapshot, error) {
	snap := Snapshot{Workloads: map[string]Workload{}}

	out, err := p.run(ctx, "pbsnodes", "-v", p.nodeName, "-F", "json")
	if err != nil {
		return snap, err
	}
	var nodes pbsNodes
	if err := json.Unmarshal(out, &nodes); err != nil {
		return snap, fmt.Errorf("pbsnodes: decode: %w", err)
	}
	n, ok := nodes.Nodes[p.nodeName]
	if !ok {
		return snap, fmt.Errorf("pbsnodes: node %q not in output", p.nodeName)
	}
	snap.Node.State = n.State
	if v, ok := intResource(n.ResourcesAvailable, "ngpus"); ok {
		snap.Node.GPUsAllocatable = &v
	}
	assigned, _ := intResource(n.ResourcesAssigned, "ngpus")
	snap.Node.GPUsAllocated = &assigned

	seen := map[string]bool{}
	var ids []string
	for _, j := range n.Jobs {
		id, _, _ := strings.Cut(strings.TrimSpace(j), "/")
		if id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return snap, nil
	}
	out, err = p.run(ctx, "qstat", append([]string{"-f", "-F", "json"}, ids...)...)
	if err != nil {
		return snap, err
	}
	var jobs pbsJobs
	if err := json.Unmarshal(out, &jobs); err != nil {
		return snap, fmt.Errorf("qstat: decode: %w", err)
	}
	for id, j := range jobs.Jobs {
		user, _, _ := strings.Cut(j.JobOwner, "@")
		snap.Workloads[id] = Workload{Name: j.JobName, User: user, Queue: j.Queue}
	}
	return snap, nil
}

func intResource(m map[string]any, key string) (int, bool) {
	switch v := m[key].(type) {
	case float64:
		return int(v), true
	case string:
		var n int
		_, err := fmt.Sscanf(v, "%d", &n)
		return n, err == nil
	}
	return 0, false
}
