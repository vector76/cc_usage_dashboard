package netbind

import (
	"net"
	"reflect"
	"testing"
)

func TestSelectBindAddrs(t *testing.T) {
	type ifaceData struct {
		name  string
		addrs []net.Addr
	}

	ipnet := func(s string, prefix int) *net.IPNet {
		ip := net.ParseIP(s)
		if ip == nil {
			t.Fatalf("invalid test ip %q", s)
		}
		bits := 32
		if ip.To4() == nil {
			bits = 128
		}
		return &net.IPNet{IP: ip, Mask: net.CIDRMask(prefix, bits)}
	}

	cases := []struct {
		name    string
		ifaces  []ifaceData
		cfg     BindConfig
		want    []string
		wantErr bool
	}{
		{
			name: "loopback only when no matching interfaces",
			want: []string{"127.0.0.1"},
		},
		{
			name: "docker bridge 172.17 included",
			ifaces: []ifaceData{{
				name:  "docker0",
				addrs: []net.Addr{ipnet("172.17.0.1", 16)},
			}},
			want: []string{"127.0.0.1", "172.17.0.1"},
		},
		{
			name: "docker desktop 192.168.65 included",
			ifaces: []ifaceData{{
				name:  "vEthernet (DockerNAT)",
				addrs: []net.Addr{ipnet("192.168.65.1", 24)},
			}},
			want: []string{"127.0.0.1", "192.168.65.1"},
		},
		{
			name: "10/8 included",
			ifaces: []ifaceData{{
				name:  "eth0",
				addrs: []net.Addr{ipnet("10.0.5.4", 8)},
			}},
			want: []string{"127.0.0.1", "10.0.5.4"},
		},
		{
			name: "wsl link-local 169.254 included",
			ifaces: []ifaceData{{
				name:  "wsl",
				addrs: []net.Addr{ipnet("169.254.5.5", 16)},
			}},
			want: []string{"127.0.0.1", "169.254.5.5"},
		},
		{
			name: "non-matching public ipv4 ignored",
			ifaces: []ifaceData{{
				name:  "eth0",
				addrs: []net.Addr{ipnet("8.8.8.8", 32)},
			}},
			want: []string{"127.0.0.1"},
		},
		{
			name: "ipv6 ignored",
			ifaces: []ifaceData{{
				name:  "eth0",
				addrs: []net.Addr{ipnet("fe80::1", 64)},
			}},
			want: []string{"127.0.0.1"},
		},
		{
			name: "multiple matching interfaces appended in order",
			ifaces: []ifaceData{
				{
					name:  "docker0",
					addrs: []net.Addr{ipnet("172.17.0.1", 16)},
				},
				{
					name:  "wsl",
					addrs: []net.Addr{ipnet("172.18.0.1", 16)},
				},
			},
			want: []string{"127.0.0.1", "172.17.0.1", "172.18.0.1"},
		},
		{
			name: "user override appended after detected",
			ifaces: []ifaceData{{
				name:  "docker0",
				addrs: []net.Addr{ipnet("172.17.0.1", 16)},
			}},
			cfg:  BindConfig{UserOverrides: []string{"192.0.2.42"}},
			want: []string{"127.0.0.1", "172.17.0.1", "192.0.2.42"},
		},
		{
			name: "fallback prepends 0.0.0.0",
			cfg:  BindConfig{EnableFallback: true},
			want: []string{"0.0.0.0", "127.0.0.1"},
		},
		{
			name: "duplicates collapsed preserving first occurrence",
			ifaces: []ifaceData{{
				name:  "docker0",
				addrs: []net.Addr{ipnet("172.17.0.1", 16)},
			}},
			cfg:  BindConfig{UserOverrides: []string{"127.0.0.1", "172.17.0.1"}},
			want: []string{"127.0.0.1", "172.17.0.1"},
		},
		{
			name:    "invalid override returns error",
			cfg:     BindConfig{UserOverrides: []string{"not-an-ip"}},
			wantErr: true,
		},
		{
			name: "ipaddr (not ipnet) form recognised",
			ifaces: []ifaceData{{
				name:  "eth0",
				addrs: []net.Addr{&net.IPAddr{IP: net.ParseIP("10.1.2.3")}},
			}},
			want: []string{"127.0.0.1", "10.1.2.3"},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			synthetic := make(map[string][]net.Addr, len(tc.ifaces))
			ifaces := make([]net.Interface, 0, len(tc.ifaces))
			for i, d := range tc.ifaces {
				synthetic[d.name] = d.addrs
				ifaces = append(ifaces, net.Interface{Index: i + 1, Name: d.name})
			}

			old := ifaceAddrs
			t.Cleanup(func() { ifaceAddrs = old })
			ifaceAddrs = func(iface net.Interface) ([]net.Addr, error) {
				return synthetic[iface.Name], nil
			}

			got, err := SelectBindAddrs(ifaces, tc.cfg)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil (got=%v)", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSelectBindAddrsRealInterfaces(t *testing.T) {
	ifaces, err := net.Interfaces()
	if err != nil {
		t.Skipf("net.Interfaces unavailable: %v", err)
	}
	got, err := SelectBindAddrs(ifaces, BindConfig{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) == 0 || got[0] != "127.0.0.1" {
		t.Fatalf("expected 127.0.0.1 first, got %v", got)
	}
}
