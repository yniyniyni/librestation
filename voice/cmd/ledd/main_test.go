package main

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// A second ledd must leave the first one's event socket alone, or the events
// from voiced go to a file nobody reads and the screen stops reacting.
func TestRunWithPortTakenKeepsSocket(t *testing.T) {
	first, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	sock := filepath.Join(t.TempDir(), "ledd.sock")
	if err := os.WriteFile(sock, []byte("first"), 0o666); err != nil {
		t.Fatal(err)
	}
	err = run([]string{"-listen", first.Addr().String(), "-sock", sock, "-assets", t.TempDir(), "-data", t.TempDir()})
	if !errors.Is(err, syscall.EADDRINUSE) {
		t.Fatalf("run = %v, want address in use", err)
	}
	b, err := os.ReadFile(sock)
	if err != nil || string(b) != "first" {
		t.Fatalf("socket file changed: %q, %v", b, err)
	}
}
