package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"voice/internal/ycmd"
)

func TestAbandonedEnrollmentExpires(t *testing.T) {
	d, _ := newTest(t, nil, "", false)
	d.enrollIdle = 150 * time.Millisecond
	if _, err := d.startEnroll("тест"); err != nil {
		t.Fatal(err)
	}
	paused := func() int { d.mu.Lock(); defer d.mu.Unlock(); return d.paused }
	// activity keeps it alive past the original deadline
	for i := 0; i < 3; i++ {
		time.Sleep(80 * time.Millisecond)
		if _, err := d.currentEnroll(); err != nil {
			t.Fatalf("expired while active: %v", err)
		}
	}
	if paused() != 1 {
		t.Fatal("not paused")
	}
	time.Sleep(400 * time.Millisecond)
	if paused() != 0 {
		t.Fatalf("paused=%d after idle", paused())
	}
	if _, err := d.currentEnroll(); err == nil {
		t.Fatal("enrollment still open")
	}
}

// stockDaemon is a daemon with or without the Yandex model (gating only).
func stockDaemon(t *testing.T, dir string, model bool) *Daemon {
	t.Helper()
	opt := Options{DataDir: dir}
	if model {
		opt.Commands = &ycmd.Model{}
	}
	d, err := New(opt)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.waitIO)
	return d
}

func TestEnrollStockNameRejected(t *testing.T) {
	d := stockDaemon(t, t.TempDir(), true)
	_, err := d.startEnroll("Стоп ")
	if err == nil || err.Error() != "Это слово колонка уже знает сама — включи его в быстрых командах" {
		t.Fatalf("err %v", err)
	}
	if _, err := d.startEnroll("алекса"); err != nil {
		t.Fatal(err)
	}
}

func TestEnrollStockNameAllowedWithoutModel(t *testing.T) {
	d := stockDaemon(t, t.TempDir(), false)
	if _, err := d.startEnroll("Стоп "); err != nil {
		t.Fatal(err)
	}
}

// seedWords makes word dirs (with name.txt and a stray clip) and settings.
func seedWords(t *testing.T, dir string, names ...string) {
	t.Helper()
	words := map[string]any{}
	for _, n := range names {
		wd := filepath.Join(dir, "words", wordID(n))
		if err := os.MkdirAll(wd, 0o755); err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(wd, "name.txt"), []byte(n), 0o644)
		os.WriteFile(filepath.Join(wd, "false-1.wav"), []byte("x"), 0o644)
		words[wordID(n)] = map[string]any{"enabled": true}
	}
	b, _ := json.Marshal(map[string]any{"words": words})
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestMigrateStockWords(t *testing.T) {
	dir := t.TempDir()
	seedWords(t, dir, "стоп", "дальше", "alexa")
	d := stockDaemon(t, dir, true)
	ents, _ := os.ReadDir(filepath.Join(dir, "words"))
	if len(ents) != 1 || ents[0].Name() != "alexa" {
		t.Fatalf("words left: %v", ents)
	}
	if len(d.settings.Words) != 1 || d.settings.Words["alexa"] == nil {
		t.Fatalf("settings: %v", d.settings.Words)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "settings.json"))
	if strings.Contains(string(b), "стоп") || strings.Contains(string(b), "дальше") {
		t.Fatalf("saved settings: %s", b)
	}
}

func TestNoMigrationWithoutModel(t *testing.T) {
	dir := t.TempDir()
	seedWords(t, dir, "стоп", "alexa")
	d := stockDaemon(t, dir, false)
	ents, _ := os.ReadDir(filepath.Join(dir, "words"))
	if len(ents) != 2 || len(d.settings.Words) != 2 {
		t.Fatalf("words %v, settings %v", ents, d.settings.Words)
	}
}
