package collector

import (
	"log"
	"net/netip"
	"strconv"
	"strings"

	"netwatch/config"
)

// Traffic classes. "internet", "lan", "vpn" and "tailnet" come from a
// connection's remote address. "unclassified" is the part of a process's
// off-host traffic whose connection closed before nettop listed it, and
// "host" is loopback: processes on this Mac talking to each other
// (containers, VMs, port forwards), which never touches the network.
const (
	ClassInternet     = "internet"
	ClassLAN          = "lan"
	ClassVPN          = "vpn"
	ClassTailnet      = "tailnet"
	ClassUnclassified = "unclassified"
	ClassHost         = "host"
)

// Directions. "served" means a remote peer connected to a port this Mac
// listens on (so bytes out are something being downloaded from here);
// "fetched" means this Mac opened the connection.
const (
	DirServed  = "served"
	DirFetched = "fetched"
)

// First port of macOS's ephemeral range: a local port below it is a
// listening service, at or above it an outbound connection.
const ephemeralStart = 49152

type Classifier struct {
	lan, vpn, tailnet []netip.Prefix
}

func NewClassifier(cfg config.NetworkConfig) *Classifier {
	return &Classifier{
		lan:     parsePrefixes(cfg.LAN),
		vpn:     parsePrefixes(cfg.VPN),
		tailnet: parsePrefixes(cfg.Tailnet),
	}
}

func parsePrefixes(list []string) []netip.Prefix {
	var out []netip.Prefix
	for _, s := range list {
		p, err := netip.ParsePrefix(strings.TrimSpace(s))
		if err != nil {
			log.Printf("[config] ignoring bad prefix %q: %v", s, err)
			continue
		}
		out = append(out, p.Masked())
	}
	return out
}

func contains(list []netip.Prefix, a netip.Addr) bool {
	for _, p := range list {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// Class returns the traffic class of a remote address. VPN and tailnet are
// checked first because they usually sit inside private ranges.
func (c *Classifier) Class(a netip.Addr) string {
	a = a.Unmap()
	switch {
	case a.IsLoopback():
		return ClassHost
	case contains(c.vpn, a):
		return ClassVPN
	case contains(c.tailnet, a):
		return ClassTailnet
	case contains(c.lan, a), a.IsPrivate(), a.IsLinkLocalUnicast(), a.IsMulticast():
		return ClassLAN
	default:
		return ClassInternet
	}
}

// Endpoint is one side of a nettop connection row.
type Endpoint struct {
	Addr netip.Addr
	Port int
}

// parseFlow parses nettop's connection field, e.g.
// "tcp4 192.168.1.10:64501<->18.155.192.79:443" or
// "tcp6 fe80::1%lo0.8021<->*.*". IPv6 rows separate the port with a dot.
func parseFlow(s string) (local, remote Endpoint, ok bool) {
	proto, rest, found := strings.Cut(strings.TrimSpace(s), " ")
	if !found {
		return local, remote, false
	}
	l, r, found := strings.Cut(rest, "<->")
	if !found {
		return local, remote, false
	}
	v6 := strings.HasSuffix(proto, "6")
	local, ok = parseEndpoint(l, v6)
	if !ok {
		return local, remote, false
	}
	// An unconnected (e.g. listening UDP) socket has remote "*:*": the
	// remote stays invalid and the caller decides by local port.
	remote, _ = parseEndpoint(r, v6)
	return local, remote, true
}

// Unconnected sockets on these local ports carry protocols that never leave
// the LAN: DHCP, NetBIOS, SSDP, mDNS, LLMNR, WS-Discovery.
var lanOnlyPorts = map[int]bool{67: true, 68: true, 137: true, 138: true, 1900: true, 3702: true, 5353: true, 5355: true}

func lanOnlyPort(p int) bool { return lanOnlyPorts[p] }

func parseEndpoint(s string, v6 bool) (Endpoint, bool) {
	sep := ":"
	if v6 {
		sep = "."
	}
	i := strings.LastIndex(s, sep)
	if i < 0 {
		return Endpoint{}, false
	}
	host, portStr := s[:i], s[i+1:]
	if j := strings.Index(host, "%"); j >= 0 {
		host = host[:j]
	}
	port, _ := strconv.Atoi(portStr)
	if host == "*" { // unbound side of a listening/unconnected socket
		return Endpoint{Port: port}, true
	}
	a, err := netip.ParseAddr(host)
	if err != nil {
		return Endpoint{}, false
	}
	return Endpoint{Addr: a, Port: port}, true
}

// direction and the port worth reporting: the local service port for
// served traffic, the remote port for fetched traffic.
func direction(local, remote Endpoint) (string, int) {
	if local.Port > 0 && local.Port < ephemeralStart {
		return DirServed, local.Port
	}
	return DirFetched, remote.Port
}
