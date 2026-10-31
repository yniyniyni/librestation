package main

import (
	"fmt"
	"net"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"
)

// pickDevice settles which speaker to talk to: the flag or ADB_DEVICE, else
// one adb already knows, else an address typed in or found on the network.
func pickDevice(u *ui, given string) (string, error) {
	if given != "" {
		return connect(given)
	}
	known := adbDevices()
	if len(known) == 1 && !u.interactive {
		return known[0], nil
	}
	if len(known) > 0 {
		u.say("adb уже видит: %s", strings.Join(known, ", "))
	}
	if !u.interactive {
		return "", fmt.Errorf("укажите колонку: -device <ip>:5555 или ADB_DEVICE")
	}
	def := ""
	if len(known) > 0 {
		def = known[0]
	}
	addr := u.ask("IP колонки (Enter без ответа — поискать в сети)", def)
	if addr == "" {
		u.say("Ищу устройства с открытым adb (порт 5555) в локальной сети…")
		found := scan(5555)
		if len(found) == 0 {
			return "", fmt.Errorf("в сети нет устройств с adb на порту 5555; включите adb по сети на колонке")
		}
		u.say("Нашлись: %s", strings.Join(found, ", "))
		addr = u.ask("Какое из них колонка", found[0])
	}
	return connect(addr)
}

func connect(addr string) (string, error) {
	if !strings.Contains(addr, ":") {
		addr += ":5555"
	}
	out, err := exec.Command("adb", "connect", addr).CombinedOutput()
	s := strings.TrimSpace(string(out))
	if err != nil || strings.Contains(s, "failed") || strings.Contains(s, "unable") {
		return "", fmt.Errorf("adb connect %s: %s", addr, s)
	}
	return addr, nil
}

// adbDevices lists serials adb has online.
func adbDevices() []string {
	out, err := exec.Command("adb", "devices").Output()
	if err != nil {
		return nil
	}
	var s []string
	for _, l := range strings.Split(string(out), "\n")[1:] {
		f := strings.Fields(l)
		if len(f) == 2 && f[1] == "device" {
			s = append(s, f[0])
		}
	}
	return s
}

// scan finds hosts with port open on the host's local IPv4 /24 networks.
func scan(port int) []string {
	var hosts []string
	for _, n := range localNets() {
		hosts = append(hosts, hostsOf(n)...)
	}
	var mu sync.Mutex
	var found []string
	sem := make(chan struct{}, 64)
	var wg sync.WaitGroup
	for _, h := range hosts {
		wg.Add(1)
		sem <- struct{}{}
		go func(h string) {
			defer wg.Done()
			defer func() { <-sem }()
			addr := net.JoinHostPort(h, fmt.Sprint(port))
			if c, err := net.DialTimeout("tcp", addr, 700*time.Millisecond); err == nil {
				c.Close()
				mu.Lock()
				found = append(found, h)
				mu.Unlock()
			}
		}(h)
	}
	wg.Wait()
	sort.Strings(found)
	return found
}

func localNets() []*net.IPNet {
	var nets []*net.IPNet
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil && !n.IP.IsLoopback() && !n.IP.IsLinkLocalUnicast() {
			nets = append(nets, n)
		}
	}
	return nets
}

// hostsOf lists the other addresses of n's /24 (wider networks are cut to
// the host's own /24, so a scan stays quick).
func hostsOf(n *net.IPNet) []string {
	ip := n.IP.To4()
	var hs []string
	for i := 1; i < 255; i++ {
		h := net.IPv4(ip[0], ip[1], ip[2], byte(i)).To4()
		if !h.Equal(ip) {
			hs = append(hs, h.String())
		}
	}
	return hs
}
