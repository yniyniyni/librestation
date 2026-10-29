package daemon

import (
	"testing"
	"time"
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
