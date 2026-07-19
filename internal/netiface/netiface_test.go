package netiface

import (
	"net"
	"testing"
)

func TestInterfacePreferredAddr(t *testing.T) {
	ipv4 := net.ParseIP("192.168.37.47")
	ipv6ULA1 := net.ParseIP("fdf4:f257:6790:0:ae86:1eba:c7fe:4f9e")
	ipv6ULA2 := net.ParseIP("fdf4:f257:6790:0:bca4:8c46:7605:227c")

	tests := []struct {
		name  string
		addrs []net.IP
		want  net.IP
	}{
		{
			// Real case hit in production: on Windows, net.Interfaces() listed
			// this adapter's IPv6 addresses before its single IPv4 address.
			// Addrs[0] picked the IPv6 one, which then got baked into
			// agent.json as local_addr and passed as ping's "-S" for every
			// check - including IPv4 targets, which Windows ping rejects
			// outright with "different address family" before sending a
			// single packet.
			name:  "ipv6 listed before ipv4 - picks ipv4",
			addrs: []net.IP{ipv6ULA1, ipv6ULA2, ipv4},
			want:  ipv4,
		},
		{
			name:  "ipv4 already first - picks ipv4",
			addrs: []net.IP{ipv4, ipv6ULA1},
			want:  ipv4,
		},
		{
			name:  "ipv6 only - falls back to first address",
			addrs: []net.IP{ipv6ULA1, ipv6ULA2},
			want:  ipv6ULA1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ifc := Interface{Name: "eth-test", Addrs: tt.addrs}
			got := ifc.PreferredAddr()
			if !got.Equal(tt.want) {
				t.Errorf("PreferredAddr() = %v, want %v", got, tt.want)
			}
		})
	}
}
