package main

import (
	"context"
	"testing"
)

func TestDNSServersFromProps(t *testing.T) {
	props := map[string]string{"net.dns1": "fd76:3287:3c84::1", "net.dns2": "192.168.1.1", "net.dns3": "junk", "net.dns4": "8.8.8.8"}
	got := dnsServers(func(p string) string { return props[p] })
	want := []string{"fd76:3287:3c84::1", "192.168.1.1", "8.8.8.8", "1.1.1.1"}
	if len(got) != len(want) {
		t.Fatalf("servers %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("servers %v, want %v", got, want)
		}
	}
}

// The dialer goes to port 53 of the first server, whatever address the
// resolver asked for (it would be [::1]:53 on Android).
func TestDNSDialerUsesServer(t *testing.T) {
	c, err := dnsDialer([]string{"127.0.0.1"})(context.Background(), "udp", "[::1]:53")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.RemoteAddr().String() != "127.0.0.1:53" {
		t.Fatalf("dialled %s", c.RemoteAddr())
	}
}
