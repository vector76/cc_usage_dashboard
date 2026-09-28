// Package netbind selects the set of host addresses the trayapp HTTP
// server should listen on. See docs/architecture.md "Network and security"
// for the binding strategy.
package netbind

import (
	"fmt"
	"log/slog"
	"net"
)

// BindConfig controls SelectBindAddrs.
type BindConfig struct {
	// UserOverrides are explicit addresses appended to the resolved list.
	// Use this to add interfaces the auto-detection missed. An unspecified
	// address (0.0.0.0 or ::) is the one exception: it replaces the list,
	// since it already covers every interface.
	UserOverrides []string
}

// dockerWSLRanges are IPv4 CIDRs that commonly correspond to Docker bridge,
// Docker Desktop vEthernet, and WSL adapters.
var dockerWSLRanges = []string{
	"172.16.0.0/12",
	"192.168.65.0/24",
	"10.0.0.0/8",
	"169.254.0.0/16",
}

// ifaceAddrs is the indirection used to look up addresses for an interface.
// Tests override this to feed synthetic data through SelectBindAddrs.
var ifaceAddrs = func(iface net.Interface) ([]net.Addr, error) {
	return iface.Addrs()
}

// SelectBindAddrs computes the list of host addresses the HTTP server should
// bind to.
//
// The result always contains 127.0.0.1; any IPv4 address from ifaces whose IP
// falls inside a well-known Docker/WSL range is appended, followed by entries
// from cfg.UserOverrides. Duplicates are removed while preserving order.
//
// An unspecified override (0.0.0.0 or ::) stands alone instead: it already
// covers loopback and each adapter, and binding it beside a specific address
// on the same port fails with "address in use". 0.0.0.0 is the configured
// default (config.Load); the per-interface list above is what an explicit
// http.bind of specific addresses produces. Non-loopback callers must present
// the access token either way — see docs/architecture.md "Network and
// security".
func SelectBindAddrs(ifaces []net.Interface, cfg BindConfig) ([]string, error) {
	for _, ov := range cfg.UserOverrides {
		if ip := net.ParseIP(ov); ip != nil && ip.IsUnspecified() {
			return []string{ov}, nil
		}
	}

	nets := make([]*net.IPNet, 0, len(dockerWSLRanges))
	for _, c := range dockerWSLRanges {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			return nil, fmt.Errorf("netbind: parse cidr %q: %w", c, err)
		}
		nets = append(nets, n)
	}

	addrs := []string{"127.0.0.1"}

	for _, iface := range ifaces {
		// Skip interfaces that are administratively or operationally down —
		// Windows commonly reports unconfigured adapters with APIPA
		// (169.254.x.x) addresses that look fine on paper but error with
		// "address not valid in its context" when bound. The loopback iface
		// is also skipped because 127.0.0.1 is added directly above.
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		ifAddrs, err := ifaceAddrs(iface)
		if err != nil {
			slog.Warn("netbind: failed to read interface addrs",
				"iface", iface.Name, "err", err)
			continue
		}
		for _, a := range ifAddrs {
			ip := addrIP(a)
			v4 := ip.To4()
			if v4 == nil {
				continue
			}
			for _, n := range nets {
				if n.Contains(v4) {
					addrs = append(addrs, v4.String())
					break
				}
			}
		}
	}

	for _, ov := range cfg.UserOverrides {
		if net.ParseIP(ov) == nil {
			return nil, fmt.Errorf("netbind: invalid override %q", ov)
		}
		addrs = append(addrs, ov)
	}

	return dedupe(addrs), nil
}

// IsWildcard reports whether bindAddrs is a single unspecified address, as
// SelectBindAddrs returns when an override asks for every interface.
func IsWildcard(bindAddrs []string) bool {
	if len(bindAddrs) != 1 {
		return false
	}
	ip := net.ParseIP(bindAddrs[0])
	return ip != nil && ip.IsUnspecified()
}

// HostAddrs returns the addresses a client could legitimately name in its
// Host header. For specific binds that is just the bind list. Under a
// wildcard bind it is every address on every up interface, plus loopback —
// the wildcard itself is not a dialable address.
func HostAddrs(ifaces []net.Interface, bindAddrs []string) []string {
	if !IsWildcard(bindAddrs) {
		return bindAddrs
	}
	addrs := []string{"127.0.0.1", "::1"}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 {
			continue
		}
		ifAddrs, err := ifaceAddrs(iface)
		if err != nil {
			slog.Warn("netbind: failed to read interface addrs",
				"iface", iface.Name, "err", err)
			continue
		}
		for _, a := range ifAddrs {
			if ip := addrIP(a); ip != nil && !ip.IsUnspecified() {
				addrs = append(addrs, ip.String())
			}
		}
	}
	return dedupe(addrs)
}

func addrIP(a net.Addr) net.IP {
	switch v := a.(type) {
	case *net.IPNet:
		return v.IP
	case *net.IPAddr:
		return v.IP
	}
	return nil
}

func dedupe(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}
