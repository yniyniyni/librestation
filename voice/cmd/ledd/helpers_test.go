package main

import (
	"os"
	"testing"

	"voice/internal/led/hw"
)

func writeFile(p string) error { return os.WriteFile(p, nil, 0o644) }

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func writeLux(t *testing.T, p, v string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(v), 0o644); err != nil {
		t.Fatal(err)
	}
}

func hwPathsFor(bl, lux string) hw.Paths { return hw.Paths{Backlight: bl, Lux: lux} }
