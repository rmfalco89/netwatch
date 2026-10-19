package collector

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

// Why a stream and not one-shot samples: `nettop -L 1` reports, per process,
// the bytes of the sockets open *right now*. A connection that closes takes
// its bytes with it, so one-shot totals fall (the old phantom-traffic bug)
// and a connection that opens and closes between samples is never seen at
// all. A long-running `nettop -d` keeps closed sockets in each process's
// figures, so its per-interval deltas are complete. Only the connection rows
// still miss sockets that closed between two updates; that remainder is
// reported as "unclassified".

// FlowDelta is one connection's bytes in one interval.
type FlowDelta struct {
	Local, Remote Endpoint
	Iface         string
	In, Out       int64
}

// ProcDelta is one process's bytes in one interval, with the connections
// nettop listed for it.
type ProcDelta struct {
	Name    string
	PID     int
	In, Out int64
	Flows   []FlowDelta
}

// Block is one nettop update.
type Block struct {
	At    time.Time
	Procs []ProcDelta
}

// Stream runs one nettop in delta logging mode and sends a Block per update.
// It restarts nettop if it exits, and every `recycle` as a guard against a
// long-lived nettop growing without bound. The first block of every run is
// cumulative since the socket was opened, not a delta, so it is dropped.
type Stream struct {
	Name     string // for logs
	Args     []string
	Recycle  time.Duration
	PerFlows bool
}

func (s *Stream) Run(ctx context.Context, out chan<- Block) {
	backoff := time.Second
	for ctx.Err() == nil {
		started := time.Now()
		err := s.runOnce(ctx, out)
		if ctx.Err() != nil {
			return
		}
		if time.Since(started) > time.Minute {
			backoff = time.Second
		}
		log.Printf("[stream %s] nettop ended (%v), restarting in %s", s.Name, err, backoff)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return
		}
		if backoff < time.Minute {
			backoff *= 2
		}
	}
}

func (s *Stream) runOnce(parent context.Context, out chan<- Block) error {
	ctx, cancel := context.WithTimeout(parent, s.Recycle)
	defer cancel()

	// nettop block-buffers when writing to a pipe, which delivers updates
	// in late bursts, so it runs on a pseudo-terminal and writes each line as
	// it goes. We start nettop itself (not a wrapper such as script(1)), so
	// cancelling kills the real process: an orphaned nettop whose terminal
	// went away spins at full CPU.
	cmd := exec.CommandContext(ctx, "/usr/bin/nettop", s.Args...)
	cmd.WaitDelay = 5 * time.Second
	tty, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 50, Cols: 500})
	if err != nil {
		return err
	}
	defer tty.Close()
	stdout := io.Reader(tty)

	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	var cur *Block
	blocks := 0
	base := newBaseline(time.Now())
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r") // the pty ends lines with \r\n
		if strings.HasPrefix(line, ",") {          // header: a new update begins
			if cur != nil {
				base.apply(cur, blocks == 0)
				if blocks > 0 {
					select {
					case out <- *cur:
					case <-ctx.Done():
					}
				}
				blocks++
			}
			cur = &Block{At: time.Now()}
			continue
		}
		if cur == nil || line == "" {
			continue
		}
		parseRow(cur, line, s.PerFlows)
	}
	err = cmd.Wait() // reading the pty ends with EIO once nettop exits
	if err == nil {
		err = sc.Err()
	}
	if ctx.Err() == context.DeadlineExceeded && parent.Err() == nil {
		return fmt.Errorf("recycled after %s", s.Recycle)
	}
	return err
}

// parseRow handles both shapes nettop emits with -J interface,bytes_in,bytes_out:
//
//	Python.86847,,52428800,52428800,                       process row
//	tcp4 192.168.1.10:64501<->18.155.192.79:443,en0,61527,6701,  connection row
//
// and the -P shape without the interface column.
func parseRow(b *Block, line string, perFlows bool) {
	f := strings.Split(line, ",")
	if len(f) < 3 {
		return
	}
	first := f[0]
	isFlow := strings.Contains(first, "<->")
	var iface, inStr, outStr string
	if perFlows {
		if len(f) < 4 {
			return
		}
		iface, inStr, outStr = f[1], f[2], f[3]
	} else {
		inStr, outStr = f[1], f[2]
	}
	in, _ := strconv.ParseInt(strings.TrimSpace(inStr), 10, 64)
	out, _ := strconv.ParseInt(strings.TrimSpace(outStr), 10, 64)

	if !isFlow {
		name, pid, err := parseProcessField(first)
		if err != nil {
			return
		}
		b.Procs = append(b.Procs, ProcDelta{Name: name, PID: pid, In: in, Out: out})
		return
	}
	if !perFlows || len(b.Procs) == 0 || (in == 0 && out == 0) {
		return
	}
	local, remote, ok := parseFlow(first)
	if !ok {
		return
	}
	p := &b.Procs[len(b.Procs)-1]
	p.Flows = append(p.Flows, FlowDelta{Local: local, Remote: remote, Iface: iface, In: in, Out: out})
}

// baseline guards against nettop reporting a source's lifetime counters as
// if they were one interval's delta. That happens when a source enters the
// stream late: nettop keeps discovering existing processes for a while after
// it starts, and a socket can move into a stream's interface filter
// (dnsmasq's socket is bound to the LAN address, but local clients reach it
// over loopback, so it joins the loopback stream 10-25 s in carrying 86 MB
// of history). The first values of such a source are a baseline, not traffic:
//
//   - a process first listed after the first update, but running since
//     before this nettop started, is dropped for that update;
//   - an unconnected socket (remote "*") first listed after the first update
//     is dropped for that update. Connected sockets that appear late are new
//     connections, and their bytes are real.
type baseline struct {
	started time.Time
	pids    map[int]bool
	sockets map[string]bool
}

func newBaseline(started time.Time) *baseline {
	return &baseline{started: started, pids: map[int]bool{}, sockets: map[string]bool{}}
}

func (b *baseline) apply(blk *Block, first bool) {
	kept := blk.Procs[:0]
	for _, p := range blk.Procs {
		newPID := !b.pids[p.PID]
		b.pids[p.PID] = true
		flows := p.Flows[:0]
		for _, f := range p.Flows {
			if !f.Remote.Addr.IsValid() {
				key := fmt.Sprintf("%d|%s|%d", p.PID, f.Local.Addr, f.Local.Port)
				if !b.sockets[key] {
					b.sockets[key] = true
					if !first && !newPID {
						p.In -= f.In
						p.Out -= f.Out
						continue
					}
				}
			}
			flows = append(flows, f)
		}
		p.Flows = flows
		if newPID && !first {
			if st, ok := processStart(p.PID); ok && st.Before(b.started) {
				continue
			}
		}
		kept = append(kept, p)
	}
	blk.Procs = kept
}

// processStart returns when a process started (false if it has exited).
func processStart(pid int) (time.Time, bool) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || kp.Proc.P_pid != int32(pid) {
		return time.Time{}, false
	}
	tv := kp.Proc.P_starttime
	return time.Unix(tv.Sec, int64(tv.Usec)*1000), true
}
