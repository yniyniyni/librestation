package mdns

import (
	"encoding/binary"
	"net"
	"strings"
	"testing"
)

var svc = Service{Name: "_librestation._tcp", Port: 8780, TXT: []string{"api=1", "path=/api/v1"}}
var testIP = net.IPv4(192, 168, 1, 168)

func question(name string, qtype uint16) []byte {
	q := make([]byte, 12)
	binary.BigEndian.PutUint16(q[4:], 1)
	q = appendName(q, name)
	q = binary.BigEndian.AppendUint16(q, qtype)
	return binary.BigEndian.AppendUint16(q, classIN)
}

func TestAnswersItsOwnQuestions(t *testing.T) {
	for _, q := range [][]byte{
		question("_librestation._tcp.local", typePTR),
		question("_LIBRESTATION._tcp.local", typePTR),
		question("_services._dns-sd._udp.local", typePTR),
		question("librestation-168._librestation._tcp.local", typeSRV),
		question("librestation-168.local", typeA),
	} {
		r := answer(q, svc, testIP)
		if r == nil {
			t.Fatalf("no answer to %x", q)
		}
		if binary.BigEndian.Uint16(r[6:]) != 4 {
			t.Fatalf("answers %d", binary.BigEndian.Uint16(r[6:]))
		}
		s := string(r)
		for _, want := range []string{"librestation-168", "api=1", "path=/api/v1"} {
			if !strings.Contains(s, want) {
				t.Fatalf("reply lacks %q", want)
			}
		}
		if !strings.Contains(s, string([]byte{192, 168, 1, 168})) {
			t.Fatal("reply lacks the address")
		}
	}
}

func TestStaysQuiet(t *testing.T) {
	response := question("_librestation._tcp.local", typePTR)
	binary.BigEndian.PutUint16(response[2:], 0x8400)
	for name, q := range map[string][]byte{
		"other service": question("_airplay._tcp.local", typePTR),
		"wrong type":    question("_librestation._tcp.local", typeA),
		"a response":    response,
		"short":         {1, 2, 3},
		"truncated":     question("_librestation._tcp.local", typePTR)[:20],
		"pointer loop":  append(append(make([]byte, 4), 0, 1, 0, 0, 0, 0, 0, 0), 0xc0, 12, 0, 12, 0, 1),
	} {
		if r := answer(q, svc, testIP); r != nil {
			t.Errorf("%s: answered", name)
		}
	}
}

func TestCompressedQuestion(t *testing.T) {
	q := make([]byte, 12)
	binary.BigEndian.PutUint16(q[4:], 2)
	q = appendName(q, "_other._tcp.local")
	q = binary.BigEndian.AppendUint16(q, typePTR)
	q = binary.BigEndian.AppendUint16(q, classIN)
	// "_librestation._tcp" + pointer to ".local"... of the first name
	q = append(q, 13)
	q = append(q, "_librestation"...)
	q = append(q, 4)
	q = append(q, "_tcp"...)
	q = append(q, 0xc0, byte(12+7+5)) // past "_other" and "_tcp": "local"
	q = binary.BigEndian.AppendUint16(q, typePTR)
	q = binary.BigEndian.AppendUint16(q, classIN)
	if answer(q, svc, testIP) == nil {
		t.Fatal("no answer")
	}
}
