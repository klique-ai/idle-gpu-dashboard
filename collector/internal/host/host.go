// Package host reads machine-wide CPU and memory usage from procfs.
//
// Inside a container /proc/stat and /proc/meminfo still describe the whole
// host (they are not namespaced), so no extra mounts are needed on Kubernetes.
package host

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Info is static host inventory.
type Info struct {
	CPUModel     string
	LogicalCores int
	MemoryTotal  uint64
	Kernel       string
	OS           string
}

// Sample is host usage at one point in time. CPUUtil covers the time since
// the previous Sample call; CPUOK is false on the first call.
type Sample struct {
	CPUUtil    float64
	CPUOK      bool
	MemoryUsed uint64
}

// Reader reads procfs under ProcRoot (normally /proc). OSRelease is the
// os-release file describing the host OS.
type Reader struct {
	procRoot  string
	osRelease string

	prevTotal, prevIdle uint64
	havePrev            bool
}

// New returns a Reader. hostRoot is where the host's root filesystem is
// visible ("/" outside containers); it is only used to find os-release.
func New(procRoot, hostRoot string) *Reader {
	return &Reader{
		procRoot:  procRoot,
		osRelease: filepath.Join(hostRoot, "etc", "os-release"),
	}
}

// Info reads static inventory. Missing optional fields are left empty.
func (r *Reader) Info() (Info, error) {
	var info Info
	mem, err := r.meminfo()
	if err != nil {
		return info, err
	}
	info.MemoryTotal = mem["MemTotal"]
	info.CPUModel, info.LogicalCores = r.cpuinfo()
	if b, err := os.ReadFile(filepath.Join(r.procRoot, "sys", "kernel", "osrelease")); err == nil {
		info.Kernel = strings.TrimSpace(string(b))
	}
	info.OS = prettyName(r.osRelease)
	return info, nil
}

// Sample reads current CPU and memory usage.
func (r *Reader) Sample() (Sample, error) {
	var s Sample
	total, idle, err := r.cpuTimes()
	if err != nil {
		return s, err
	}
	if r.havePrev && total > r.prevTotal {
		dt, di := float64(total-r.prevTotal), float64(idle-r.prevIdle)
		s.CPUUtil = max(0, min(100, 100*(1-di/dt)))
		s.CPUOK = true
	}
	r.prevTotal, r.prevIdle, r.havePrev = total, idle, true

	mem, err := r.meminfo()
	if err != nil {
		return s, err
	}
	avail, ok := mem["MemAvailable"]
	if !ok { // kernels before 3.14
		avail = mem["MemFree"] + mem["Buffers"] + mem["Cached"]
	}
	if t := mem["MemTotal"]; t > avail {
		s.MemoryUsed = t - avail
	}
	return s, nil
}

// cpuTimes returns cumulative total and idle jiffies across all CPUs.
func (r *Reader) cpuTimes() (total, idle uint64, err error) {
	f, err := os.Open(filepath.Join(r.procRoot, "stat"))
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 5 || fields[0] != "cpu" {
			continue
		}
		// user nice system idle iowait irq softirq steal [guest guest_nice]
		// guest time is already counted in user/nice, so stop at steal.
		for i, v := range fields[1:min(len(fields), 9)] {
			n, err := strconv.ParseUint(v, 10, 64)
			if err != nil {
				return 0, 0, fmt.Errorf("parse /proc/stat: %w", err)
			}
			total += n
			if i == 3 || i == 4 { // idle, iowait
				idle += n
			}
		}
		return total, idle, nil
	}
	return 0, 0, errors.New("parse /proc/stat: no aggregate cpu line")
}

// meminfo returns /proc/meminfo values in bytes.
func (r *Reader) meminfo() (map[string]uint64, error) {
	b, err := os.ReadFile(filepath.Join(r.procRoot, "meminfo"))
	if err != nil {
		return nil, err
	}
	out := map[string]uint64{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		key, rest, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		n, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			continue
		}
		if len(fields) > 1 && fields[1] == "kB" {
			n *= 1024
		}
		out[key] = n
	}
	if out["MemTotal"] == 0 {
		return nil, errors.New("parse /proc/meminfo: no MemTotal")
	}
	return out, nil
}

// cpuinfo returns the CPU model name (x86; empty on most ARM kernels) and the
// number of logical CPUs on the host. /proc/cpuinfo lists every CPU even when
// the collector's own cgroup is restricted to a few.
func (r *Reader) cpuinfo() (model string, logical int) {
	b, err := os.ReadFile(filepath.Join(r.procRoot, "cpuinfo"))
	if err != nil {
		return "", 0
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		key, val, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(key) {
		case "processor":
			logical++
		case "model name":
			if model == "" {
				model = strings.TrimSpace(val)
			}
		}
	}
	return model, logical
}

func prettyName(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "PRETTY_NAME="); ok {
			return strings.Trim(v, `"'`)
		}
	}
	return ""
}
