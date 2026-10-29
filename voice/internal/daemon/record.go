package daemon

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"

	miccap "voice/internal/capture"
)

const (
	maxRecordSecs   = 30
	maxSessionFiles = 5
	// minFreeBytes stays free on /data after a recording, so a session
	// can never fill the speaker's storage.
	minFreeBytes = 300 << 20
)

// freeBytes reports the space available to us on the filesystem holding dir.
var freeBytes = func(dir string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}

// recorder saves the next N seconds of raw 16-channel capture blocks as a WAV
// so a session can be replayed on the Mac (`voiced replay`).
type recorder struct {
	mu     sync.Mutex
	f      *os.File
	w      *bufio.Writer
	path   string
	left   int // frames still to write
	frames int
	done   bool
	err    string
}

type recordStatus struct {
	Active bool   `json:"active"`
	Path   string `json:"path,omitempty"`
	Done   bool   `json:"done"`
	Error  string `json:"error,omitempty"`
}

func (d *Daemon) recordStatus() recordStatus {
	r := &d.rec
	r.mu.Lock()
	defer r.mu.Unlock()
	return recordStatus{Active: r.f != nil, Path: r.path, Done: r.done, Error: r.err}
}

func (d *Daemon) startRecording(secs int) error {
	r := &d.rec
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f != nil {
		return errors.New("a recording is already in progress")
	}
	dir := filepath.Join(d.opt.DataDir, "sessions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	// make room for the new file first, then check it fits
	pruneSessions(dir, maxSessionFiles-1)
	need := uint64(len(wavHeader(0))+secs*miccap.Rate*miccap.Channels*2) + minFreeBytes
	if free, err := freeBytes(dir); err != nil {
		return err
	} else if free < need {
		return fmt.Errorf("not enough free space: %d MB free, need %d MB", free>>20, need>>20)
	}
	base := d.now().Format("20060102-150405")
	path := filepath.Join(dir, base+".wav")
	for i := 2; ; i++ {
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			break
		}
		path = filepath.Join(dir, fmt.Sprintf("%s-%d.wav", base, i))
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	w := bufio.NewWriterSize(f, 1<<16)
	w.Write(wavHeader(0)) // sizes patched on finish
	r.f, r.w, r.path, r.left, r.frames, r.done, r.err = f, w, path, secs*miccap.Rate, 0, false, ""
	log.Printf("recording %d s of raw capture to %s", secs, path)
	return nil
}

// onRaw sees every raw capture block (the slice is reused by the caller, so
// it is serialised right here).
func (d *Daemon) onRaw(b []int16) {
	d.pushRef(b)
	r := &d.rec
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return
	}
	n := len(b) / miccap.Channels
	n = min(n, r.left)
	buf := make([]byte, 2*n*miccap.Channels)
	for i := 0; i < n*miccap.Channels; i++ {
		binary.LittleEndian.PutUint16(buf[2*i:], uint16(b[i]))
	}
	if _, err := r.w.Write(buf); err != nil {
		d.finishRecordingLocked(err)
		return
	}
	r.left -= n
	r.frames += n
	if r.left <= 0 {
		d.finishRecordingLocked(nil)
	}
}

func (d *Daemon) finishRecordingLocked(werr error) {
	r := &d.rec
	err := werr
	if e := r.w.Flush(); err == nil {
		err = e
	}
	if err == nil {
		_, err = r.f.WriteAt(wavHeader(r.frames), 0)
	}
	if e := r.f.Close(); err == nil {
		err = e
	}
	r.f, r.w = nil, nil
	if err != nil {
		r.err = err.Error()
		log.Printf("recording %s: %v", r.path, err)
		os.Remove(r.path) // a truncated file is useless and takes space
	} else {
		r.done = true
		log.Printf("recording saved: %s (%d frames)", r.path, r.frames)
	}
}

// pruneSessions keeps the newest keep recordings.
func pruneSessions(dir string, keep int) {
	files, _ := filepath.Glob(filepath.Join(dir, "*.wav"))
	sort.Strings(files)
	for len(files) > keep {
		os.Remove(files[0])
		files = files[1:]
	}
}

// wavHeader is the 44-byte header of 16-channel 48 kHz s16 PCM with the given
// number of frames.
func wavHeader(frames int) []byte {
	const ch, bits = miccap.Channels, 16
	data := frames * ch * bits / 8
	b := make([]byte, 44)
	copy(b, "RIFF")
	binary.LittleEndian.PutUint32(b[4:], uint32(36+data))
	copy(b[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(b[16:], 16)
	binary.LittleEndian.PutUint16(b[20:], 1)
	binary.LittleEndian.PutUint16(b[22:], ch)
	binary.LittleEndian.PutUint32(b[24:], miccap.Rate)
	binary.LittleEndian.PutUint32(b[28:], uint32(miccap.Rate*ch*bits/8))
	binary.LittleEndian.PutUint16(b[32:], ch*bits/8)
	binary.LittleEndian.PutUint16(b[34:], bits)
	copy(b[36:], "data")
	binary.LittleEndian.PutUint32(b[40:], uint32(data))
	return b
}

// pushRef feeds the playback reference (channels 11 and 12, averaged) to the
// spectrum analyser behind the LED events.
func (d *Daemon) pushRef(b []int16) {
	if d.bands == nil {
		return
	}
	n := len(b) / miccap.Channels
	x := make([]float64, n)
	for i := range x {
		f := b[i*miccap.Channels:]
		x[i] = (float64(f[refL]) + float64(f[refR])) / (2 * 32768)
	}
	d.bands.Push(x)
}

// refL and refR are the playback reference channels in the raw capture.
const refL, refR = 11, 12
