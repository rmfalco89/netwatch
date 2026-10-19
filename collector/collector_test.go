package collector

import (
	"net/netip"
	"os"
	"testing"
	"time"

	"netwatch/config"
	"netwatch/store"
)

func TestParseRowExternal(t *testing.T) {
	b := &Block{}
	for _, l := range []string{
		"limactl.35352,,61527075,6701946,",
		"tcp4 192.168.1.10:64501<->18.155.192.79:443,en0,61000000,6700000,",
		"tcp4 192.168.1.10:443<->192.168.1.20:55699,en0,500,1500,",
		"tcp6 fe80::1%lo0.8021<->*.*,lo0,,,",
		"tcp4 127.0.0.1:55964<->*:*,lo0,,,",
		"udp4 *:67<->*:*,,1200,0,",
		"ssh.35386,,10,20,",
		"udp6 2601:646::1.53822<->2600:1700::5.41641,en0,7,9,",
	} {
		parseRow(b, l, true)
	}
	if len(b.Procs) != 2 {
		t.Fatalf("procs = %d, want 2", len(b.Procs))
	}
	p := b.Procs[0]
	if p.Name != "limactl" || p.PID != 35352 || p.In != 61527075 || p.Out != 6701946 {
		t.Errorf("proc = %+v", p)
	}
	if len(p.Flows) != 3 {
		t.Fatalf("flows = %d, want 3 (zero-byte rows skipped)", len(p.Flows))
	}
	if f := p.Flows[2]; f.Local.Port != 67 || f.Remote.Addr.IsValid() || f.In != 1200 {
		t.Errorf("unconnected udp = %+v", f)
	}
	if p.Flows[0].Remote.Addr != netip.MustParseAddr("18.155.192.79") || p.Flows[0].Remote.Port != 443 || p.Flows[0].Iface != "en0" {
		t.Errorf("flow0 = %+v", p.Flows[0])
	}
	if f := b.Procs[1].Flows; len(f) != 1 || f[0].Local.Port != 53822 || f[0].Remote.Port != 41641 {
		t.Errorf("v6 flow = %+v", f)
	}
}

func TestParseRowPerProcess(t *testing.T) {
	b := &Block{}
	parseRow(b, "com.apple.WebKit.Networking.812,123,456,", false)
	if len(b.Procs) != 1 || b.Procs[0].Name != "com.apple.WebKit.Networking" || b.Procs[0].PID != 812 || b.Procs[0].Out != 456 {
		t.Errorf("got %+v", b.Procs)
	}
}

func TestClassify(t *testing.T) {
	c := NewClassifier(config.NetworkConfig{
		LAN:     config.Default().Network.LAN,
		VPN:     []string{"10.8.0.0/24"},
		Tailnet: config.Default().Network.Tailnet,
	})
	for ip, want := range map[string]string{
		"18.155.192.79":   ClassInternet,
		"192.168.1.20":    ClassLAN,
		"10.8.0.3":        ClassVPN, // inside 10/8 but VPN wins
		"100.100.1.2":     ClassTailnet,
		"127.0.0.1":       ClassHost,
		"::1":             ClassHost,
		"fe80::1":         ClassLAN,
		"224.0.0.251":     ClassLAN,
		"::ffff:8.8.8.8":  ClassInternet,
		"2606:4700::6810": ClassInternet,
	} {
		if got := c.Class(netip.MustParseAddr(ip)); got != want {
			t.Errorf("%s: got %s, want %s", ip, got, want)
		}
	}
}

func TestDirection(t *testing.T) {
	ep := func(port int) Endpoint { return Endpoint{Port: port} }
	if d, p := direction(ep(443), ep(55699)); d != DirServed || p != 443 {
		t.Errorf("served: %s %d", d, p)
	}
	if d, p := direction(ep(64501), ep(443)); d != DirFetched || p != 443 {
		t.Errorf("fetched: %s %d", d, p)
	}
}

func TestUnconnectedSockets(t *testing.T) {
	c := New(nil, config.Default())
	c.addExternal(Block{Procs: []ProcDelta{{Name: "bootpd", PID: 9, In: 1500, Flows: []FlowDelta{
		{Local: Endpoint{Port: 67}, In: 1200},   // DHCP: LAN by port
		{Local: Endpoint{Port: 41641}, In: 100}, // unknown: stays unclassified
	}}}})
	got := map[string]int64{}
	for k, v := range c.traffic {
		got[k.class] += v.In
	}
	if got[ClassLAN] != 1200 || got[ClassUnclassified] != 300 {
		t.Errorf("got %v", got)
	}
}

func TestResidualIsUnclassified(t *testing.T) {
	c := New(nil, config.Default())
	c.addExternal(Block{At: time.Now(), Procs: []ProcDelta{{
		Name: "curl", PID: 7, In: 1000, Out: 300,
		Flows: []FlowDelta{{
			Local:  Endpoint{netip.MustParseAddr("192.168.1.10"), 60000},
			Remote: Endpoint{netip.MustParseAddr("1.1.1.1"), 443},
			In:     600, Out: 300,
		}},
	}}})
	got := map[string]store.InOut{}
	for k, v := range c.traffic {
		got[k.class+"/"+k.dir] = *v
	}
	if got["internet/fetched"] != (store.InOut{In: 600, Out: 300}) {
		t.Errorf("internet = %+v", got["internet/fetched"])
	}
	if got["unclassified/"] != (store.InOut{In: 400, Out: 0}) {
		t.Errorf("unclassified = %+v", got["unclassified/"])
	}
}

func TestLabels(t *testing.T) {
	l := NewLabeler()
	l.vmByPort = map[int]string{2222: "dev-vm"}
	for _, tc := range []struct{ name, cmd, want string }{
		{"limactl", "/opt/homebrew/bin/limactl hostagent --pidfile /Users/x/.lima/dev-vm/ha.pid --socket s dev-vm", "VM dev-vm"},
		{"limactl", "/opt/homebrew/bin/limactl usernet -p /Users/x/.colima/_lima/_networks/user-v2/usernet_user-v2.pid", "Docker (Colima VM)"},
		{"limactl", "/opt/homebrew/bin/limactl hostagent --pidfile /Users/x/.colima/_lima/colima/ha.pid colima", "Docker (Colima VM)"},
		{"ssh", "ssh: /Users/x/.colima/_lima/colima/ssh.sock [mux]", "Docker (Colima VM)"},
		{"ssh", "/usr/bin/ssh -N -L 192.168.1.10:2222:127.0.0.1:22 -p 2222 -o X=y 127.0.0.1", "VM dev-vm"},
		{"ssh", "ssh me@192.168.1.20", "ssh"},
		{"Python", "/Users/x/services/myapp/venv/bin/python -m uvicorn app:app", "Python (myapp)"},
		{"Google Chrome Helper", "/Applications/Google Chrome.app/Contents/Frameworks/x", "Google Chrome"},
	} {
		if got := l.label(tc.name, tc.cmd); got != tc.want {
			t.Errorf("%s %q: got %q, want %q", tc.name, tc.cmd, got, tc.want)
		}
	}
}

func TestCymruName(t *testing.T) {
	if got := cymruName(netip.MustParseAddr("18.155.192.79")); got != "79.192.155.18.origin.asn.cymru.com" {
		t.Error(got)
	}
	if got := cymruName(netip.MustParseAddr("2001:db8::1")); got != "1.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.8.b.d.0.1.0.0.2.origin6.asn.cymru.com" {
		t.Error(got)
	}
}

func TestCleanOrg(t *testing.T) {
	for in, want := range map[string]string{
		"AMAZON-02 - Amazon.com, Inc., US": "Amazon.com, Inc.",
		"ANTHROPIC - Anthropic, PBC, US":   "Anthropic, PBC",
		"CLOUDFLARENET, US":                "CLOUDFLARENET",
		"GOOGLE":                           "GOOGLE",
	} {
		if got := cleanOrg(in); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
}

func TestBaselineDropsLateSources(t *testing.T) {
	b := newBaseline(time.Now())
	me := os.Getpid() // started before the baseline: an "old" process
	b.apply(&Block{Procs: []ProcDelta{{Name: "a", PID: me, In: 10}}}, true)

	// An unconnected socket of a known process joins late with its history.
	blk := &Block{Procs: []ProcDelta{{Name: "a", PID: me, In: 86_000_100, Flows: []FlowDelta{
		{Local: Endpoint{netip.MustParseAddr("192.168.1.10"), 53}, In: 86_000_000},
		{Local: Endpoint{netip.MustParseAddr("127.0.0.1"), 60000}, Remote: Endpoint{netip.MustParseAddr("127.0.0.1"), 80}, In: 100},
	}}}}
	b.apply(blk, false)
	if p := blk.Procs[0]; p.In != 100 || len(p.Flows) != 1 {
		t.Errorf("late socket not baselined: %+v", p)
	}
	// Next update: the socket is known, its delta counts.
	blk = &Block{Procs: []ProcDelta{{Name: "a", PID: me, In: 500, Flows: []FlowDelta{
		{Local: Endpoint{netip.MustParseAddr("192.168.1.10"), 53}, In: 500},
	}}}}
	b.apply(blk, false)
	if blk.Procs[0].In != 500 {
		t.Errorf("known socket dropped: %+v", blk.Procs[0])
	}

	// An old process first listed late is a baseline; launchd (PID 1) qualifies.
	blk = &Block{Procs: []ProcDelta{{Name: "launchd", PID: 1, In: 5_000_000}}}
	b.apply(blk, false)
	if len(blk.Procs) != 0 {
		t.Errorf("old late process kept: %+v", blk.Procs)
	}
}

func TestTrackerBlipAndRecreate(t *testing.T) {
	tr := newTracker(0)
	now := time.Now()
	tr.update(map[string]counter{"default": {340e9, 28e9}}, now) // baseline
	if d := tr.update(map[string]counter{"default": {340e9 + 500, 28e9 + 100}}, now); d["default"] != (counter{500, 100}) {
		t.Errorf("delta = %+v", d)
	}
	tr.update(map[string]counter{}, now) // row missing from one reading
	if d := tr.update(map[string]counter{"default": {340e9 + 700, 28e9 + 100}}, now); d["default"] != (counter{200, 0}) {
		t.Errorf("after blip = %+v, want only the new 200 bytes", d)
	}
	if d := tr.update(map[string]counter{"default": {1000, 50}}, now); d["default"] != (counter{1000, 50}) {
		t.Errorf("recreated route = %+v", d)
	}
	if d := tr.update(map[string]counter{"new": {42, 7}}, now); d["new"] != (counter{42, 7}) {
		t.Errorf("new source = %+v", d)
	}

	hosts := newTracker(time.Minute)
	hosts.update(map[string]counter{"1.1.1.1": {10, 10}}, now)
	hosts.update(map[string]counter{}, now.Add(2*time.Minute)) // expired and forgotten
	if d := hosts.update(map[string]counter{"1.1.1.1": {5, 5}}, now.Add(3*time.Minute)); d["1.1.1.1"] != (counter{5, 5}) {
		t.Errorf("re-cloned host route = %+v", d)
	}
}
