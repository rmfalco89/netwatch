# Netwatch

A per-process network monitor for macOS that answers two questions: **how much really went over the
internet**, and **who sent it where**. It runs as a daemon, reads `nettop` (no sudo), stores
per-minute data in SQLite and serves a web dashboard.

## Features

- **Measured totals** — internet traffic from the default route's own byte counters, so the
  headline number is what actually left through the router, whichever process sent it
- **Who** — traffic per program, split by destination: internet, LAN, VPN, Tailscale, and
  host-internal (loopback). Lima VMs, Colima/Docker and UTM VMs get their own rows
- **Where** — remote endpoints with their owner (reverse DNS + ASN), port, and whether this
  Mac fetched from them or served them
- **Honest accounting** — the share of measured traffic that is pinned on a process is shown,
  and bytes whose destination wasn't seen are labelled *unclassified* rather than guessed
- **Read-only dashboard** bound to localhost by default, no CORS, no write endpoints
- **Retention** — per-minute rows for 14 days, then hourly forever; endpoints for 180 days

## Install

```bash
git clone https://github.com/rmfalco89/netwatch.git
cd netwatch
go build -o netwatch .

./netwatch start                 # foreground
open http://localhost:8099

./netwatch install               # launchd agent: starts on login, restarts if it dies
./netwatch uninstall
```

## CLI

```
netwatch start       Run the daemon (foreground)
netwatch status      Today's internet traffic and the top uploaders
netwatch install     Install as a launchd agent
netwatch uninstall   Remove the launchd agent
```

## Dashboard

- **Overview** — measured internet / LAN / UTM / host-internal totals, how much of the internet
  traffic is attributed, a timeline, top uploaders and top destinations
- **Who** — every program with its traffic per class; click one for its processes,
  destinations and timeline
- **Where** — endpoints by upload, download or total, filtered by class
- **How it's measured** — what each number means, and the collector's health

## Configuration

Optional `netwatch.yaml` next to the binary (or `-c path`):

```yaml
collector:
  stream_interval: 5       # nettop update period, seconds
  flush_interval: 60       # write period, seconds
  ignore_processes: [kernel_task]
  lookup_asn: true         # PTR + Team Cymru DNS lookups for endpoint owners

network:                   # remote-address classes; anything else is "internet"
  lan: ["10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "169.254.0.0/16", "224.0.0.0/4", "fc00::/7", "fe80::/10", "ff00::/8"]
  vpn: ["10.8.0.0/24"]     # e.g. your WireGuard client subnet; checked before lan
  tailnet: ["100.64.0.0/10", "fd7a:115c:a1e0::/48"]

server:
  host: 127.0.0.1          # put a reverse proxy in front for remote access
  port: 8099

storage:
  path: ./data/netwatch.db
  retention:
    minute_days: 14
    endpoint_days: 180
```

## How it works

- **Two streaming `nettop` processes** in delta mode, one for every non-loopback interface and
  one for loopback, each on a pseudo-terminal so updates arrive as they happen. A long-running
  nettop keeps closed sockets in each process's figures; one-shot `nettop -L 1` samples do not,
  so they lose every connection that closes between samples.
- Each open connection's remote address classifies its bytes. Whatever a process moved beyond
  its listed connections (connections that opened and closed between two updates) is
  *unclassified*.
- A source that joins a stream late (a process nettop discovers after starting, or a socket
  whose traffic moves onto loopback) first reports its lifetime counters; that first value is
  taken as a baseline, not traffic.
- **Ground truth** each minute: the default routes' counters (`nettop -m route`) for internet
  traffic, and interface counters (`netstat -ib`) for everything on the wire, VPN tunnels and
  UTM VMs (`vmenet*`; a bridged VM never passes through the Mac's IP stack, so nothing else
  sees it). Route counters include TCP/IP headers and ACKs, so they run a few percent above
  the per-process payload counts.
- Processes are labelled from their command line: a Lima VM is its `limactl hostagent` plus
  its ssh tunnels, Colima is its `usernet` process plus the ssh multiplexer for published
  ports. Containers inside one VM can't be told apart from the host.

## Requirements

- macOS (`nettop`, `netstat`)
- Go 1.25+ to build

## License

MIT
