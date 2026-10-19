package collector

import (
	"bufio"
	"context"
	"net/netip"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// System-wide ground truth, independent of any per-process attribution,
// sampled every few seconds:
//
//   - "internet": the byte counters of the default routes through physical
//     interfaces (`nettop -m route`). Everything that leaves via the router
//     is counted there, by any process.
//   - destinations: the cloned host route macOS keeps per remote address
//     (same output). It outlives the connection by tens of seconds, so even
//     a connection that opened and closed between two nettop updates leaves
//     its bytes here, with its destination though without its process.
//   - "if:<name>": interface counters from `netstat -ib`: physical en*,
//     VPN utun*, loopback, and vmenet* — one per UTM VM. A bridged UTM VM
//     has its own LAN address and never touches this Mac's IP stack, so
//     neither nettop nor the routes see it; its vmenet counter is the only
//     record of it (from the Mac's side: bytes in = the VM's upload).
//
// All are cumulative; tracker turns them into deltas.

type counter struct{ in, out int64 }

// tracker keeps the last reading per source. A source that went down was
// recreated (route re-cloned, utun recreated, reboot): its current value is
// the delta. A source missing from one reading keeps its last value for
// memory, because nettop occasionally omits a row: forgetting it at once
// would count its whole history again when it reappears (for the default
// route, every byte since boot).
type tracker struct {
	last   map[string]trackEntry
	memory time.Duration // 0 = never forget
	ready  bool
}

type trackEntry struct {
	c    counter
	seen time.Time
}

func newTracker(memory time.Duration) *tracker {
	return &tracker{last: map[string]trackEntry{}, memory: memory}
}

func (t *tracker) update(cur map[string]counter, now time.Time) map[string]counter {
	d, _ := t.updateNew(cur, now)
	return d
}

// updateNew also reports which sources were counted in full as new.
func (t *tracker) updateNew(cur map[string]counter, now time.Time) (map[string]counter, map[string]bool) {
	deltas := map[string]counter{}
	fresh := map[string]bool{}
	for key, c := range cur {
		e, seen := t.last[key]
		var d counter
		switch {
		case !seen:
			if t.ready { // appeared since the last reading: all of it is new
				d = c
				fresh[key] = true
			}
		default:
			d = counter{c.in - e.c.in, c.out - e.c.out}
			if d.in < 0 {
				d.in = c.in
			}
			if d.out < 0 {
				d.out = c.out
			}
		}
		t.last[key] = trackEntry{c, now}
		if d.in > 0 || d.out > 0 {
			deltas[key] = d
		}
	}
	if t.memory > 0 {
		for key, e := range t.last {
			if now.Sub(e.seen) > t.memory {
				delete(t.last, key)
			}
		}
	}
	t.ready = true
	return deltas, fresh
}

type SystemSampler struct {
	classes  *Classifier
	defaults *tracker // default routes and interfaces: few, never forgotten
	hosts    *tracker // host routes: many and short-lived
}

func NewSystemSampler(c *Classifier) *SystemSampler {
	return &SystemSampler{classes: c, defaults: newTracker(0), hosts: newTracker(2 * time.Minute)}
}

// Sample returns deltas since the previous call: kinds ("internet",
// "if:en0", ...) and internet destinations by remote address. Both are empty
// on the first call.
func (s *SystemSampler) Sample(ctx context.Context) (kinds, dests map[string]counter) {
	now := time.Now()
	fixed := map[string]counter{}
	hosts := map[string]counter{}
	if defaults, hostRoutes, err := readRoutes(ctx); err == nil {
		for name, c := range defaults {
			fixed["route:"+name] = c
		}
		for ip, c := range hostRoutes {
			if a, err := netip.ParseAddr(ip); err == nil && s.classes.Class(a) == ClassInternet {
				hosts[ip] = c
			}
		}
	}
	if ifs, err := readInterfaces(ctx); err == nil {
		for name, c := range ifs {
			fixed["if:"+name] = c
		}
	}

	kinds = map[string]counter{}
	for key, d := range s.defaults.update(fixed, now) {
		if strings.HasPrefix(key, "route:") {
			key = "internet"
		}
		acc := kinds[key]
		acc.in += d.in
		acc.out += d.out
		kinds[key] = acc
	}
	// A host route listed for the first time is counted in full, which is
	// right for a new destination but wrong for an old one nettop merely
	// omitted until now (1.1.1.1 once arrived carrying 1.4 GB of history). No
	// new destination can have moved more than the router did meanwhile.
	inet := kinds["internet"]
	dests, fresh := s.hosts.updateNew(hosts, now)
	for ip := range fresh {
		if d := dests[ip]; d.in > inet.in || d.out > inet.out {
			delete(dests, ip)
		}
	}
	return kinds, dests
}

// readRoutes returns the counters of default routes through physical
// interfaces ("default -> en0 -> 192.168.1.1") and of host routes ("1.1.1.1").
// Defaults through utun/ipsec are VPN tunnels, whose outer packets are
// already counted on en*.
func readRoutes(ctx context.Context) (defaults, hosts map[string]counter, err error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/bin/nettop", "-m", "route", "-L", "1", "-n", "-x", "-J", "bytes_in,bytes_out").Output()
	if err != nil {
		return nil, nil, err
	}
	defaults, hosts = map[string]counter{}, map[string]counter{}
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		f := strings.Split(sc.Text(), ",")
		if len(f) < 3 {
			continue
		}
		in, _ := strconv.ParseInt(f[1], 10, 64)
		o, _ := strconv.ParseInt(f[2], 10, 64)
		switch {
		case strings.HasPrefix(f[0], "default -> en"):
			defaults[f[0]] = counter{in, o}
		case !strings.ContainsAny(f[0], " /%"): // a bare address: a host route
			hosts[f[0]] = counter{in, o}
		}
	}
	return defaults, hosts, nil
}

// readInterfaces parses `netstat -ib` link rows. Rows without a link-layer
// address have one column fewer, so the counters are read from the end:
// ... Ibytes Opkts Oerrs Obytes Coll.
func readInterfaces(ctx context.Context) (map[string]counter, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/sbin/netstat", "-ib").Output()
	if err != nil {
		return nil, err
	}
	res := map[string]counter{}
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 10 || !strings.HasPrefix(f[2], "<Link#") {
			continue
		}
		name := strings.TrimSuffix(f[0], "*")
		if !interesting(name) {
			continue
		}
		in, err1 := strconv.ParseInt(f[len(f)-5], 10, 64)
		o, err2 := strconv.ParseInt(f[len(f)-2], 10, 64)
		if err1 != nil || err2 != nil || (in == 0 && o == 0) {
			continue
		}
		res[name] = counter{in, o}
	}
	return res, nil
}

func interesting(name string) bool {
	for _, p := range []string{"en", "utun", "vmenet", "lo0"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}
