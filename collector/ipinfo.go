package collector

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/netip"
	"strings"
	"time"

	"netwatch/store"
)

// Owner lookups for internet endpoints, so "18.155.192.79" reads as
// "AMAZON-02 (cloudfront.net)". Two DNS queries per address: the PTR record,
// and Team Cymru's IP-to-ASN service (origin.asn.cymru.com, then
// AS<n>.asn.cymru.com for the name). Results are cached for 30 days.
// Disable with collector.lookup_asn: false.

const ipInfoTTL = 30 * 24 * time.Hour

func (c *Collector) lookupLoop(ctx context.Context) {
	t := time.NewTicker(2 * time.Minute)
	defer t.Stop()
	for {
		ips, err := c.db.IPsToLookUp(time.Now().Add(-7*24*time.Hour).Unix(), time.Now().Add(-ipInfoTTL).Unix(), 50)
		if err != nil {
			log.Printf("[ipinfo] %v", err)
		}
		for _, ip := range ips {
			if ctx.Err() != nil {
				return
			}
			info := lookupIP(ctx, ip)
			if err := c.db.SaveIPInfo(info); err != nil {
				log.Printf("[ipinfo] save %s: %v", ip, err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func lookupIP(ctx context.Context, ip string) store.IPInfo {
	info := store.IPInfo{IP: ip}
	r := net.DefaultResolver
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	if names, err := r.LookupAddr(ctx, ip); err == nil && len(names) > 0 {
		info.PTR = strings.TrimSuffix(names[0], ".")
	}
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return info
	}
	q := cymruName(addr)
	if q == "" {
		return info
	}
	// "16509 | 18.155.192.0/23 | US | arin | 2019-10-07"
	txt, err := r.LookupTXT(ctx, q)
	if err != nil || len(txt) == 0 {
		return info
	}
	f := splitPipes(txt[0])
	if len(f) < 3 {
		return info
	}
	asn := strings.Fields(f[0]) // multi-origin prefixes list several
	if len(asn) == 0 {
		return info
	}
	info.ASN = "AS" + asn[0]
	info.Country = f[2]
	// "16509 | US | arin | 2000-05-04 | AMAZON-02 - Amazon.com, Inc., US"
	if txt, err := r.LookupTXT(ctx, info.ASN+".asn.cymru.com"); err == nil && len(txt) > 0 {
		if g := splitPipes(txt[0]); len(g) >= 5 {
			info.Org = cleanOrg(g[4])
		}
	}
	return info
}

// cleanOrg turns "AMAZON-02 - Amazon.com, Inc., US" into "Amazon.com, Inc.".
func cleanOrg(s string) string {
	if _, name, ok := strings.Cut(s, " - "); ok && name != "" {
		s = name
	}
	if i := strings.LastIndex(s, ", "); i >= 0 && len(s)-i == 4 {
		s = s[:i] // trailing ", US"
	}
	return strings.TrimSpace(s)
}

func splitPipes(s string) []string {
	parts := strings.Split(s, "|")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

func cymruName(a netip.Addr) string {
	a = a.Unmap()
	if a.Is4() {
		b := a.As4()
		return fmt.Sprintf("%d.%d.%d.%d.origin.asn.cymru.com", b[3], b[2], b[1], b[0])
	}
	if a.Is6() {
		b := a.As16()
		var sb strings.Builder
		for i := 15; i >= 0; i-- {
			fmt.Fprintf(&sb, "%x.%x.", b[i]&0xf, b[i]>>4)
		}
		return sb.String() + "origin6.asn.cymru.com"
	}
	return ""
}
