package collector

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"netwatch/config"
	"netwatch/store"
)

// Collector runs two nettop streams and one system sampler:
//
//   - external (-t external): everything not on loopback. Per connection it
//     knows the remote address, so traffic is split into internet / lan /
//     vpn / tailnet; bytes of connections that closed before nettop listed
//     them are "unclassified".
//   - loopback (-t loopback): processes on this Mac talking to each other.
//     Connection rows are read only so late-joining sockets can be
//     baselined (see baseline in stream.go).
//     Containers, VMs and their port forwards produce a lot of this; it is
//     stored as class "host" so it can be shown apart from real traffic.
//   - system (every 10 s): default-route, host-route and interface
//     counters: the totals the
//     per-process figures are checked against.
type Collector struct {
	db      *store.DB
	cfg     *config.Config
	classes *Classifier
	labels  *Labeler
	system  *SystemSampler
	ignore  map[string]bool

	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu        sync.Mutex
	traffic   map[tKey]*store.InOut
	endpoints map[eKey]*store.InOut
	sysAcc    map[string]*store.InOut // kind -> bytes
	destAcc   map[string]*store.InOut // internet address -> bytes
	stats     Stats
}

type proc struct {
	name string
	pid  int
}

type tKey struct {
	proc
	class, dir string
}

type eKey struct {
	proc
	remote     string
	port       int
	class, dir string
}

type Stats struct {
	Started        time.Time `json:"started"`
	Flushes        int64     `json:"flushes"`
	LastFlush      time.Time `json:"last_flush"`
	LastFlushError string    `json:"last_flush_error,omitempty"`
	ExternalBlocks int64     `json:"external_blocks"`
	LoopbackBlocks int64     `json:"loopback_blocks"`
	LastExternal   time.Time `json:"last_external"`
	LastLoopback   time.Time `json:"last_loopback"`
}

func New(db *store.DB, cfg *config.Config) *Collector {
	ignore := map[string]bool{}
	for _, p := range cfg.Collector.IgnoreProcesses {
		ignore[p] = true
	}
	return &Collector{
		db:        db,
		cfg:       cfg,
		classes:   NewClassifier(cfg.Network),
		labels:    NewLabeler(),
		system:    NewSystemSampler(NewClassifier(cfg.Network)),
		ignore:    ignore,
		traffic:   map[tKey]*store.InOut{},
		endpoints: map[eKey]*store.InOut{},
		sysAcc:    map[string]*store.InOut{},
		destAcc:   map[string]*store.InOut{},
		stats:     Stats{Started: time.Now()},
	}
}

func (c *Collector) Start() {
	reapOrphans()
	ctx, cancel := context.WithCancel(context.Background())
	c.cancel = cancel
	interval := strconv.Itoa(max(1, c.cfg.Collector.StreamInterval))
	recycle := 6 * time.Hour

	ext := make(chan Block, 4)
	lo := make(chan Block, 4)
	streams := []struct {
		s  *Stream
		ch chan Block
	}{
		{&Stream{Name: "external", PerFlows: true, Recycle: recycle,
			Args: []string{"-L", "0", "-s", interval, "-d", "-n", "-x", "-t", "external", "-J", "interface,bytes_in,bytes_out"}}, ext},
		{&Stream{Name: "loopback", PerFlows: true, Recycle: recycle,
			Args: []string{"-L", "0", "-s", interval, "-d", "-n", "-x", "-t", "loopback", "-J", "interface,bytes_in,bytes_out"}}, lo},
	}
	for _, s := range streams {
		c.wg.Add(1)
		go func() { defer c.wg.Done(); s.s.Run(ctx, s.ch) }()
	}

	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		for {
			select {
			case b := <-ext:
				c.addExternal(b)
			case b := <-lo:
				c.addLoopback(b)
			case <-ctx.Done():
				return
			}
		}
	}()

	c.wg.Add(1)
	go func() { defer c.wg.Done(); c.systemLoop(ctx) }()

	c.wg.Add(1)
	go func() { defer c.wg.Done(); c.flushLoop(ctx) }()

	if c.cfg.Collector.LookupASN {
		c.wg.Add(1)
		go func() { defer c.wg.Done(); c.lookupLoop(ctx) }()
	}
	log.Printf("[collector] started: nettop every %ss, flush every %ds", interval, c.cfg.Collector.FlushInterval)
}

// reapOrphans kills streaming nettops left behind by a previous run that
// died without stopping them (their parent is then launchd, PID 1).
func reapOrphans() {
	for pid, cmd := range commandLines(context.Background()) {
		if strings.HasPrefix(cmd, "/usr/bin/nettop -L 0 -s ") && parentPID(pid) == 1 {
			if p, err := os.FindProcess(pid); err == nil {
				p.Kill()
				log.Printf("[collector] killed orphaned nettop %d", pid)
			}
		}
	}
}

func parentPID(pid int) int {
	out, err := exec.Command("/bin/ps", "-o", "ppid=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0
	}
	ppid, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	return ppid
}

func (c *Collector) Stop() {
	if c.cancel != nil {
		c.cancel()
	}
	c.wg.Wait()
	c.flush(time.Now()) // keep the partial minute
	log.Println("[collector] stopped")
}

func (c *Collector) Stats() Stats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stats
}

func (c *Collector) addTraffic(k tKey, in, out int64) {
	if in <= 0 && out <= 0 {
		return
	}
	v := c.traffic[k]
	if v == nil {
		v = &store.InOut{}
		c.traffic[k] = v
	}
	v.In += max(in, 0)
	v.Out += max(out, 0)
}

func (c *Collector) addExternal(b Block) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stats.ExternalBlocks++
	c.stats.LastExternal = b.At
	for _, p := range b.Procs {
		if c.ignore[p.Name] {
			continue
		}
		pr := proc{p.Name, p.PID}
		var seenIn, seenOut int64
		for _, f := range p.Flows {
			if !f.Remote.Addr.IsValid() {
				// Unconnected socket: only local-only protocols are
				// classifiable; the rest stays in the residual.
				if lanOnlyPort(f.Local.Port) {
					c.addTraffic(tKey{pr, ClassLAN, ""}, f.In, f.Out)
					seenIn += f.In
					seenOut += f.Out
				}
				continue
			}
			class := c.classes.Class(f.Remote.Addr)
			dir, port := direction(f.Local, f.Remote)
			c.addTraffic(tKey{pr, class, dir}, f.In, f.Out)
			seenIn += f.In
			seenOut += f.Out
			if class == ClassHost {
				continue
			}
			k := eKey{pr, f.Remote.Addr.Unmap().String(), port, class, dir}
			v := c.endpoints[k]
			if v == nil {
				v = &store.InOut{}
				c.endpoints[k] = v
			}
			v.In += f.In
			v.Out += f.Out
		}
		// What the process moved minus what its listed connections account
		// for: connections that opened and/or closed between two updates.
		c.addTraffic(tKey{pr, ClassUnclassified, ""}, p.In-seenIn, p.Out-seenOut)
	}
}

func (c *Collector) addLoopback(b Block) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stats.LoopbackBlocks++
	c.stats.LastLoopback = b.At
	for _, p := range b.Procs {
		if c.ignore[p.Name] {
			continue
		}
		c.addTraffic(tKey{proc{p.Name, p.PID}, ClassHost, ""}, p.In, p.Out)
	}
}

// systemLoop samples routes and interfaces every 10 s: host routes outlive
// their connection by tens of seconds, so this catches them all.
func (c *Collector) systemLoop(ctx context.Context) {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		kinds, dests := c.system.Sample(ctx)
		c.mu.Lock()
		for k, v := range kinds {
			accumulate(c.sysAcc, k, v)
		}
		for k, v := range dests {
			accumulate(c.destAcc, k, v)
		}
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func accumulate(m map[string]*store.InOut, k string, v counter) {
	a := m[k]
	if a == nil {
		a = &store.InOut{}
		m[k] = a
	}
	a.In += v.in
	a.Out += v.out
}

// flushLoop writes once per FlushInterval, aligned to the wall clock, and
// rolls up old rows once an hour.
func (c *Collector) flushLoop(ctx context.Context) {
	every := time.Duration(max(10, c.cfg.Collector.FlushInterval)) * time.Second
	c.rollup()
	lastRollup := time.Now()
	for {
		next := time.Now().Truncate(every).Add(every)
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Until(next)):
		}
		c.flush(next)
		if time.Since(lastRollup) > time.Hour {
			c.rollup()
			lastRollup = time.Now()
		}
	}
}

func (c *Collector) rollup() {
	r := c.cfg.Storage.Retention
	if err := c.db.Rollup(max(1, r.MinuteDays), max(1, r.EndpointDays)); err != nil {
		log.Printf("[collector] rollup: %v", err)
	}
}

func (c *Collector) flush(at time.Time) {
	// Rows are stamped with the start of the minute they cover.
	ts := at.Add(-time.Second).Truncate(time.Minute).Unix()
	hour := ts / 3600 * 3600

	c.mu.Lock()
	traffic, endpoints, sys, dests := c.traffic, c.endpoints, c.sysAcc, c.destAcc
	c.traffic, c.endpoints = map[tKey]*store.InOut{}, map[eKey]*store.InOut{}
	c.sysAcc, c.destAcc = map[string]*store.InOut{}, map[string]*store.InOut{}
	c.mu.Unlock()

	names := map[int]string{}
	for k := range traffic {
		names[k.pid] = k.name
	}
	labels := c.labels.Resolve(context.Background(), names)

	// Several PIDs of one process collapse into one row per label.
	tRows := map[[4]string]*store.TrafficRow{}
	for k, v := range traffic {
		key := [4]string{labels[k.pid], k.name, k.class, k.dir}
		r := tRows[key]
		if r == nil {
			r = &store.TrafficRow{TS: ts, Label: key[0], Process: k.name, Class: k.class, Dir: k.dir}
			tRows[key] = r
		}
		r.BytesIn += v.In
		r.BytesOut += v.Out
	}
	eRows := map[string]*store.EndpointRow{}
	for k, v := range endpoints {
		lbl := labels[k.pid]
		if lbl == "" {
			lbl = k.name
		}
		key := fmt.Sprintf("%s|%s|%d|%s|%s", lbl, k.remote, k.port, k.class, k.dir)
		r := eRows[key]
		if r == nil {
			r = &store.EndpointRow{TS: hour, Label: lbl, Remote: k.remote, Port: k.port, Class: k.class, Dir: k.dir}
			eRows[key] = r
		}
		r.BytesIn += v.In
		r.BytesOut += v.Out
	}

	var tr []store.TrafficRow
	for _, r := range tRows {
		tr = append(tr, *r)
	}
	var er []store.EndpointRow
	for _, r := range eRows {
		er = append(er, *r)
	}
	var sr []store.SystemRow
	for kind, v := range sys {
		sr = append(sr, store.SystemRow{TS: ts, Kind: kind, BytesIn: v.In, BytesOut: v.Out})
	}
	var dr []store.DestinationRow
	for ip, v := range dests {
		dr = append(dr, store.DestinationRow{TS: hour, Remote: ip, BytesIn: v.In, BytesOut: v.Out})
	}

	err := c.db.Write(tr, er, sr, dr)
	c.mu.Lock()
	c.stats.Flushes++
	c.stats.LastFlush = at
	c.stats.LastFlushError = ""
	if err != nil {
		c.stats.LastFlushError = err.Error()
		log.Printf("[collector] write: %v", err)
	}
	c.mu.Unlock()
}
