package daemon

import (
	"encoding/binary"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRecordWritesWAV(t *testing.T) {
	d, _ := newTest(t, nil, "", false)
	srv := httptest.NewServer(d.Handler())
	defer srv.Close()
	if r := post(t, srv.URL+"/api/record", `{"seconds":1}`); r.StatusCode != 200 {
		t.Fatalf("status %d", r.StatusCode)
	}
	blk := make([]int16, 960*16)
	for i := 0; i < 60; i++ { // 1.2 s offered, 1 s kept
		for j := range blk {
			blk[j] = int16(i*16 + j%16)
		}
		d.onRaw(blk)
	}
	st := d.recordStatus()
	if st.Active || !st.Done || st.Path == "" {
		t.Fatalf("status %+v", st)
	}
	b, err := os.ReadFile(st.Path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b[:4]) != "RIFF" || binary.LittleEndian.Uint16(b[22:]) != 16 || binary.LittleEndian.Uint32(b[24:]) != 48000 {
		t.Fatal("bad header")
	}
	if got, want := len(b), 44+48000*16*2; got != want {
		t.Fatalf("file %d bytes, want %d", got, want)
	}
	if binary.LittleEndian.Uint32(b[40:]) != 48000*16*2 {
		t.Fatal("data size not patched")
	}
	// channel order survives: block 1, channel 3
	if v := int16(binary.LittleEndian.Uint16(b[44+2*(960*16+3):])); v != 16+3 {
		t.Fatalf("sample %d", v)
	}
}

func TestRecordRejectsBadSeconds(t *testing.T) {
	d, _ := newTest(t, nil, "", false)
	srv := httptest.NewServer(d.Handler())
	defer srv.Close()
	for _, body := range []string{`{"seconds":0}`, `{"seconds":61}`, `{"seconds":-3}`, `{}`, `nope`} {
		if r := post(t, srv.URL+"/api/record", body); r.StatusCode != 400 {
			t.Fatalf("%s: status %d", body, r.StatusCode)
		}
	}
	if d.recordStatus().Active {
		t.Fatal("must not be active")
	}
}

func TestRecordKeepsTenFiles(t *testing.T) {
	d, _ := newTest(t, nil, "", false)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	n := 0
	d.now = func() time.Time { n++; return base.Add(time.Duration(n) * time.Second) }
	blk := make([]int16, 960*16)
	for i := 0; i < 12; i++ {
		if err := d.startRecording(1); err != nil {
			t.Fatal(err)
		}
		for j := 0; j < 50; j++ {
			d.onRaw(blk)
		}
	}
	files, _ := filepath.Glob(filepath.Join(d.opt.DataDir, "sessions", "*.wav"))
	if len(files) != 10 {
		t.Fatalf("%d files", len(files))
	}
}
