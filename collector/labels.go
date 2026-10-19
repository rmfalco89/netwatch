package collector

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Labels turn a PID into something a person recognises. Several processes
// can share a label, which is the point: a Lima VM is its hostagent plus the
// ssh tunnels that forward its ports, and Colima/Docker is its usernet
// process plus the ssh multiplexer that carries the published ports.
//
// Labels are resolved from the command line when a PID is first seen and
// stored with every row, so history keeps its meaning after the PID is gone.

type Labeler struct {
	mu       sync.Mutex
	byPID    map[int]labelEntry
	vmByPort map[int]string // Lima VM ssh port -> VM name
	portsAt  time.Time
}

type labelEntry struct {
	label string
	seen  time.Time
}

func NewLabeler() *Labeler {
	return &Labeler{byPID: map[int]labelEntry{}, vmByPort: map[int]string{}}
}

// Resolve returns labels for the given processes, running `ps` once for any
// PIDs not seen before.
func (l *Labeler) Resolve(ctx context.Context, procs map[int]string) map[int]string {
	l.mu.Lock()
	defer l.mu.Unlock()

	var missing []int
	for pid := range procs {
		if _, ok := l.byPID[pid]; !ok {
			missing = append(missing, pid)
		}
	}
	if len(missing) > 0 {
		cmds := commandLines(ctx)
		l.refreshVMPorts(ctx)
		for _, pid := range missing {
			l.byPID[pid] = labelEntry{label: l.label(procs[pid], cmds[pid])}
		}
	}
	now := time.Now()
	out := make(map[int]string, len(procs))
	for pid := range procs {
		e := l.byPID[pid]
		e.seen = now
		l.byPID[pid] = e
		out[pid] = e.label
	}
	// Forget PIDs idle for an hour, so a reused PID gets relabelled.
	for pid, e := range l.byPID {
		if now.Sub(e.seen) > time.Hour {
			delete(l.byPID, pid)
		}
	}
	return out
}

var (
	reSSHPort  = regexp.MustCompile(`\s-p\s+(\d+)\b`)
	reSSHFwd   = regexp.MustCompile(`\s-[LR]\s+\S+`)
	reHomePath = regexp.MustCompile(`/(?:services|projects|docker-services)/([^/\s]+)/`)
	reAppPath  = regexp.MustCompile(`/([^/]+)\.app/`)
)

func (l *Labeler) label(name, cmd string) string {
	switch name {
	case "limactl":
		fields := strings.Fields(cmd)
		if strings.Contains(cmd, " hostagent ") && len(fields) > 0 {
			return vmLabel(fields[len(fields)-1])
		}
		if strings.Contains(cmd, " usernet ") && strings.Contains(cmd, "/.colima/") {
			return vmLabel("colima")
		}
		return "Lima"
	case "ssh":
		if strings.Contains(cmd, "/.colima/") {
			return vmLabel("colima") // the mux that carries Docker's published ports
		}
		if reSSHFwd.MatchString(" " + cmd) {
			if m := reSSHPort.FindStringSubmatch(" " + cmd); m != nil {
				port, _ := strconv.Atoi(m[1])
				if vm, ok := l.vmByPort[port]; ok {
					return vmLabel(vm) // a LAN tunnel into the VM
				}
			}
			return "ssh tunnels"
		}
		return "ssh"
	case "wireguard-go":
		return "WireGuard VPN"
	case "IPNExtension", "tailscaled", "Tailscale":
		return "Tailscale"
	case "QEMULauncher":
		return "UTM VMs"
	}
	if m := reHomePath.FindStringSubmatch(cmd); m != nil {
		return name + " (" + m[1] + ")"
	}
	if m := reAppPath.FindStringSubmatch(cmd); m != nil && m[1] != name {
		return m[1]
	}
	return name
}

func vmLabel(vm string) string {
	if vm == "colima" {
		return "Docker (Colima VM)"
	}
	return "VM " + vm
}

func (l *Labeler) refreshVMPorts(ctx context.Context) {
	if time.Since(l.portsAt) < 5*time.Minute {
		return
	}
	l.portsAt = time.Now()
	limactl := "/opt/homebrew/bin/limactl"
	if _, err := os.Stat(limactl); err != nil {
		if p, err := exec.LookPath("limactl"); err == nil {
			limactl = p
		} else {
			return
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, limactl, "list", "--format", "{{.Name}} {{.SSHLocalPort}}")
	cmd.Env = append(os.Environ(), "HOME="+homeDir())
	out, err := cmd.Output()
	if err != nil {
		return
	}
	m := map[int]string{}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 {
			if p, err := strconv.Atoi(f[1]); err == nil && p > 0 {
				m[p] = f[0]
			}
		}
	}
	l.vmByPort = m
}

func homeDir() string {
	h, _ := os.UserHomeDir()
	return filepath.Clean(h)
}

// commandLines maps every PID to its full command line.
func commandLines(ctx context.Context) map[int]string {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/bin/ps", "-axww", "-o", "pid=,command=").Output()
	res := map[int]string{}
	if err != nil {
		return res
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		pidStr, cmd, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		if pid, err := strconv.Atoi(pidStr); err == nil {
			res[pid] = strings.TrimSpace(cmd)
		}
	}
	return res
}
