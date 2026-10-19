// Package owner finds out what a process is and which workload it belongs
// to, using only procfs. Every scheduler puts jobs in cgroups, and the cgroup
// path names the job: /proc/<pid>/cgroup is a scheduler-neutral way to map a
// GPU process to its pod, Slurm job, PBS job or container.
package owner

import (
	"bufio"
	"bytes"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/klique-ai/idle-gpu-dashboard/collector/internal/report"
)

// Owner kinds.
const (
	KindKubernetes = "kubernetes"
	KindSlurm      = "slurm"
	KindPBS        = "pbs"
	KindDocker     = "docker"
	KindPodman     = "podman"
	KindSystemd    = "systemd"
	KindSession    = "session"
)

// Process is what procfs tells us about a PID.
type Process struct {
	Name    string
	User    string
	Cmdline string
	Owner   *report.Owner
}

// Resolver reads process details from procfs. It caches user names; it is not
// safe for concurrent use.
type Resolver struct {
	procRoot       string
	includeCmdline bool
	users          map[string]string
	lookupUser     func(uid string) (*user.User, error)
}

// NewResolver returns a Resolver for procfs at procRoot.
func NewResolver(procRoot string, includeCmdline bool) *Resolver {
	return &Resolver{
		procRoot:       procRoot,
		includeCmdline: includeCmdline,
		users:          map[string]string{},
		lookupUser:     user.LookupId,
	}
}

// Lookup describes pid. It fails only when the process is not visible at all
// (exited, or in another PID namespace: on Kubernetes the collector needs
// hostPID for this).
func (r *Resolver) Lookup(pid int) (Process, error) {
	dir := filepath.Join(r.procRoot, strconv.Itoa(pid))
	comm, err := os.ReadFile(filepath.Join(dir, "comm"))
	if err != nil {
		return Process{}, err
	}
	p := Process{Name: strings.TrimSpace(string(comm))}
	if status, err := os.ReadFile(filepath.Join(dir, "status")); err == nil {
		if uid := realUID(status); uid != "" {
			p.User = r.userName(uid)
		}
	}
	// The kernel cuts comm at 15 characters; the program path in the command
	// line has the full name. Only argv[0] is used unless --include-cmdline.
	needFull := len(p.Name) == maxComm
	if needFull || r.includeCmdline {
		if b, err := os.ReadFile(filepath.Join(dir, "cmdline")); err == nil {
			if needFull {
				p.Name = fullName(p.Name, b)
			}
			if r.includeCmdline {
				p.Cmdline = cmdline(b)
			}
		}
	}
	if b, err := os.ReadFile(filepath.Join(dir, "cgroup")); err == nil {
		p.Owner = ParseCgroup(string(b))
	}
	return p, nil
}

func (r *Resolver) userName(uid string) string {
	if name, ok := r.users[uid]; ok {
		return name
	}
	name := uid // unknown inside containers whose /etc/passwd lacks host users
	if u, err := r.lookupUser(uid); err == nil {
		name = u.Username
	}
	r.users[uid] = name
	return name
}

func realUID(status []byte) string {
	sc := bufio.NewScanner(bytes.NewReader(status))
	for sc.Scan() {
		if rest, ok := strings.CutPrefix(sc.Text(), "Uid:"); ok {
			if f := strings.Fields(rest); len(f) > 0 {
				return f[0]
			}
		}
	}
	return ""
}

// maxComm is the longest name the kernel keeps in /proc/<pid>/comm.
const maxComm = 15

// fullName returns the untruncated program name from argv[0] when it starts
// with the truncated comm; otherwise comm is kept (renamed processes, kernel
// threads with an empty command line).
func fullName(comm string, cmdline []byte) string {
	argv0, _, _ := bytes.Cut(cmdline, []byte{0})
	base := filepath.Base(string(argv0))
	if len(base) > len(comm) && strings.HasPrefix(base, comm) {
		return base
	}
	return comm
}

const maxCmdline = 512

func cmdline(b []byte) string {
	s := strings.TrimSpace(strings.ReplaceAll(string(bytes.TrimRight(b, "\x00")), "\x00", " "))
	if len(s) > maxCmdline {
		s = s[:maxCmdline] + "…"
	}
	return s
}

var (
	podUIDRe      = regexp.MustCompile(`pod([0-9a-f]{8}[-_][0-9a-f]{4}[-_][0-9a-f]{4}[-_][0-9a-f]{4}[-_][0-9a-f]{12})`)
	slurmJobRe    = regexp.MustCompile(`/job_(\d+)(?:/|$)`)
	pbsJobRe      = regexp.MustCompile(`^\d+(?:\[\d*\])?\.[A-Za-z0-9_.-]+$`)
	containerIDRe = regexp.MustCompile(`^(?:[a-z-]+-)?([0-9a-f]{64})(?:\.scope)?$`)
	sessionRe     = regexp.MustCompile(`^session-[^/]+\.scope$`)
	userSliceRe   = regexp.MustCompile(`/user-(\d+)\.slice`)
)

// ParseCgroup maps the contents of /proc/<pid>/cgroup to the workload that
// owns the process, or nil when the path names no known workload. It handles
// cgroup v1 and v2, the cgroupfs and systemd drivers, and paths made relative
// by a cgroup namespace ("/../../kubepods-...").
func ParseCgroup(content string) *report.Owner {
	for _, path := range cgroupPaths(content) {
		if o := parsePath(path); o != nil {
			return o
		}
	}
	return nil
}

// cgroupPaths returns the cgroup paths, most informative first: the unified
// (v2) hierarchy, then v1 controllers that always carry the job layout.
func cgroupPaths(content string) []string {
	var first, rest []string
	for _, line := range strings.Split(strings.TrimSpace(content), "\n") {
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 || parts[2] == "" || parts[2] == "/" {
			continue
		}
		switch {
		case parts[0] == "0" && parts[1] == "":
			first = append([]string{parts[2]}, first...)
		case strings.Contains(parts[1], "memory"), strings.Contains(parts[1], "cpu"),
			strings.Contains(parts[1], "devices"), parts[1] == "name=systemd":
			first = append(first, parts[2])
		default:
			rest = append(rest, parts[2])
		}
	}
	return append(first, rest...)
}

func parsePath(path string) *report.Owner {
	segs := strings.Split(strings.Trim(path, "/"), "/")
	last := segs[len(segs)-1]

	if strings.Contains(path, "kubepods") {
		if m := podUIDRe.FindStringSubmatch(path); m != nil {
			o := &report.Owner{Kind: KindKubernetes, ID: strings.ReplaceAll(m[1], "_", "-")}
			if c := containerIDRe.FindStringSubmatch(last); c != nil {
				o.ContainerID = c[1]
			}
			return o
		}
	}
	if strings.Contains(path, "slurm") {
		if m := slurmJobRe.FindStringSubmatch(path); m != nil {
			return &report.Owner{Kind: KindSlurm, ID: m[1]}
		}
	}
	if strings.Contains(path, "pbs") {
		for _, s := range segs {
			if pbsJobRe.MatchString(s) {
				return &report.Owner{Kind: KindPBS, ID: s}
			}
		}
	}
	for i, s := range segs {
		switch {
		case s == "docker" && i+1 < len(segs) && containerIDRe.MatchString(segs[i+1]):
			return &report.Owner{Kind: KindDocker, ContainerID: segs[i+1]}
		case strings.HasPrefix(s, "docker-") && containerIDRe.MatchString(s):
			return &report.Owner{Kind: KindDocker, ContainerID: containerIDRe.FindStringSubmatch(s)[1]}
		case strings.HasPrefix(s, "libpod-") && containerIDRe.MatchString(s):
			return &report.Owner{Kind: KindPodman, ContainerID: containerIDRe.FindStringSubmatch(s)[1]}
		}
	}
	switch {
	case sessionRe.MatchString(last):
		o := &report.Owner{Kind: KindSession, ID: strings.TrimSuffix(last, ".scope")}
		if m := userSliceRe.FindStringSubmatch(path); m != nil {
			o.User = m[1]
		}
		return o
	case strings.HasSuffix(last, ".service"):
		return &report.Owner{Kind: KindSystemd, ID: last}
	}
	return nil
}
