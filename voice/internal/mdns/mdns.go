// Package mdns answers multicast DNS questions for one service, so a phone in
// the same network finds the speaker. It only answers: nothing is sent unless
// someone asks, so an idle network costs one blocked read.
package mdns

import (
	"context"
	"encoding/binary"
	"fmt"
	"log"
	"net"
	"strings"
	"syscall"
	"time"
)

const (
	metaQuery = "_services._dns-sd._udp.local"
	ttl       = 120 // seconds
	maxPacket = 9000

	typeA   = 1
	typePTR = 12
	typeTXT = 16
	typeSRV = 33
	classIN = 1
	// cacheFlush marks a record of ours alone (RFC 6762, 10.2).
	cacheFlush = 0x8000
	anyType    = 255

	// rejoin renews the group membership, which a Wi-Fi reconnect drops.
	rejoin = 5 * time.Minute
)

var group = &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: 5353}

// Service is what is announced; Name is like "_librestation._tcp".
type Service struct {
	Name string
	Port int
	TXT  []string
}

// Serve answers until ctx ends. It never fails the caller: without a network
// or the port it logs once and keeps trying.
func Serve(ctx context.Context, s Service) {
	logged := ""
	for ctx.Err() == nil {
		err := serveOnce(ctx, s)
		if err != nil && err.Error() != logged {
			logged = err.Error()
			log.Printf("mdns: %v", err)
		}
		select {
		case <-ctx.Done():
		case <-time.After(30 * time.Second):
		}
	}
}

// serveOnce joins the group on the current interface and answers for a while;
// nil means it is time to rejoin.
func serveOnce(ctx context.Context, s Service) error {
	iface, ip, err := lanInterface()
	if err != nil {
		return err
	}
	c, err := net.ListenMulticastUDP("udp4", iface, group)
	if err != nil {
		return err
	}
	defer c.Close()
	setMulticastTTL(c)
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	c.SetReadDeadline(time.Now().Add(rejoin))
	buf := make([]byte, maxPacket)
	for {
		n, from, err := c.ReadFromUDP(buf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				return nil
			}
			return err
		}
		if reply := answer(buf[:n], s, ip); reply != nil {
			to := group
			if from.Port != group.Port {
				to = from // a one-shot query expects a direct reply
			}
			if _, err := c.WriteToUDP(reply, to); err != nil {
				return err
			}
		}
	}
}

func setMulticastTTL(c *net.UDPConn) {
	if rc, err := c.SyscallConn(); err == nil {
		rc.Control(func(fd uintptr) {
			syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IP, syscall.IP_MULTICAST_TTL, 255)
		})
	}
}

// lanInterface picks the first running multicast interface with an IPv4
// address.
func lanInterface() (*net.Interface, net.IP, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, nil, err
	}
	for i := range ifaces {
		f := &ifaces[i]
		if f.Flags&net.FlagUp == 0 || f.Flags&net.FlagLoopback != 0 || f.Flags&net.FlagMulticast == 0 {
			continue
		}
		addrs, err := f.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil && n.IP.IsPrivate() {
				return f, n.IP.To4(), nil
			}
		}
	}
	return nil, nil, fmt.Errorf("нет сети с адресом IPv4")
}

// answer returns the reply to a query, or nil when it is not about s.
func answer(query []byte, s Service, ip net.IP) []byte {
	if len(query) < 12 || binary.BigEndian.Uint16(query[2:])&0x8000 != 0 { // a response, not a query
		return nil
	}
	id := fmt.Sprintf("librestation-%d", ip.To4()[3])
	service := s.Name + ".local"
	instance := id + "." + service
	host := id + ".local"

	asked := false
	off := 12
	for range int(binary.BigEndian.Uint16(query[4:])) {
		name, next, ok := readName(query, off)
		if !ok || next+4 > len(query) {
			return nil
		}
		qtype := binary.BigEndian.Uint16(query[next:])
		off = next + 4
		name = strings.ToLower(name)
		switch {
		case name == service || name == metaQuery:
			asked = asked || qtype == typePTR || qtype == anyType
		case name == strings.ToLower(instance):
			asked = asked || qtype == typeSRV || qtype == typeTXT || qtype == anyType
		case name == strings.ToLower(host):
			asked = asked || qtype == typeA || qtype == anyType
		}
	}
	if !asked {
		return nil
	}

	out := make([]byte, 12, 512)
	binary.BigEndian.PutUint16(out[2:], 0x8400) // response, authoritative
	binary.BigEndian.PutUint16(out[6:], 4)      // answers
	out = record(out, service, typePTR, classIN, appendName(nil, instance))
	srv := binary.BigEndian.AppendUint16(make([]byte, 4), uint16(s.Port))
	out = record(out, instance, typeSRV, classIN|cacheFlush, appendName(srv, host))
	var txt []byte
	for _, t := range s.TXT {
		txt = append(append(txt, byte(len(t))), t...)
	}
	out = record(out, instance, typeTXT, classIN|cacheFlush, txt)
	return record(out, host, typeA, classIN|cacheFlush, ip.To4())
}

func record(out []byte, name string, rtype, class uint16, data []byte) []byte {
	out = appendName(out, name)
	out = binary.BigEndian.AppendUint16(out, rtype)
	out = binary.BigEndian.AppendUint16(out, class)
	out = binary.BigEndian.AppendUint32(out, ttl)
	out = binary.BigEndian.AppendUint16(out, uint16(len(data)))
	return append(out, data...)
}

func appendName(out []byte, name string) []byte {
	for _, l := range strings.Split(name, ".") {
		out = append(append(out, byte(len(l))), l...)
	}
	return append(out, 0)
}

// readName reads a possibly compressed name at off; next is the offset after
// it in the message.
func readName(b []byte, off int) (name string, next int, ok bool) {
	var labels []string
	next = -1
	for jumps := 0; jumps < 16; {
		if off >= len(b) {
			return "", 0, false
		}
		l := int(b[off])
		switch {
		case l == 0:
			if next < 0 {
				next = off + 1
			}
			return strings.Join(labels, "."), next, true
		case l&0xc0 == 0xc0:
			if off+2 > len(b) {
				return "", 0, false
			}
			if next < 0 {
				next = off + 2
			}
			off = int(binary.BigEndian.Uint16(b[off:]) & 0x3fff)
			jumps++
		case l > 63 || off+1+l > len(b):
			return "", 0, false
		default:
			labels = append(labels, string(b[off+1:off+1+l]))
			off += 1 + l
		}
	}
	return "", 0, false
}
