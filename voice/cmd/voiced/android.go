package main

import (
	"context"
	"errors"
	"log"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"
)

// voiced is built for linux, not android, so Go looks for the resolver
// config and CA certificates where a Linux system keeps them. useAndroidNet
// points it at Android's instead.

// androidCerts is where Android keeps its CA certificates (PEM, one per file).
const androidCerts = "/system/etc/security/cacerts"

var fallbackDNS = []string{"8.8.8.8", "1.1.1.1"}

func useAndroidNet() {
	useAndroidCerts()
	useAndroidDNS()
}

// useAndroidCerts lets crypto/x509 find Android's CA certificates; it must
// run before the first TLS connection, when the system pool is loaded.
func useAndroidCerts() {
	if os.Getenv("SSL_CERT_DIR") != "" || os.Getenv("SSL_CERT_FILE") != "" {
		return
	}
	if _, err := os.Stat(androidCerts); err == nil {
		os.Setenv("SSL_CERT_DIR", androidCerts)
	}
}

// useAndroidDNS: Android has no /etc/resolv.conf, so Go's resolver asks
// [::1]:53, where nothing listens. It is pointed at the servers Android
// publishes in net.dns1..4, with public ones as a last resort.
func useAndroidDNS() {
	if _, err := os.Stat("/etc/resolv.conf"); err == nil {
		return
	}
	servers := dnsServers(getprop)
	log.Printf("dns: no /etc/resolv.conf, using %s", strings.Join(servers, ", "))
	net.DefaultResolver = &net.Resolver{PreferGo: true, Dial: dnsDialer(servers)}
}

func getprop(name string) string {
	out, err := exec.Command("getprop", name).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// dnsServers lists net.dns1..4 (IP addresses only) followed by fallbackDNS.
func dnsServers(prop func(string) string) []string {
	var s []string
	seen := map[string]bool{}
	add := func(ip string) {
		if net.ParseIP(ip) != nil && !seen[ip] {
			seen[ip] = true
			s = append(s, ip)
		}
	}
	for _, p := range []string{"net.dns1", "net.dns2", "net.dns3", "net.dns4"} {
		add(prop(p))
	}
	for _, ip := range fallbackDNS {
		add(ip)
	}
	return s
}

// dnsDialer tries the servers in order; the resolver's own address is ignored.
func dnsDialer(servers []string) func(ctx context.Context, network, _ string) (net.Conn, error) {
	return func(ctx context.Context, network, _ string) (net.Conn, error) {
		d := net.Dialer{Timeout: 2 * time.Second}
		var errs []error
		for _, ip := range servers {
			c, err := d.DialContext(ctx, network, net.JoinHostPort(ip, "53"))
			if err == nil {
				return c, nil
			}
			errs = append(errs, err)
		}
		return nil, errors.Join(errs...)
	}
}
