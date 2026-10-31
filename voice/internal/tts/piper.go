package tts

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"voice/internal/wav"
)

// stuckAfter is how long a request may go unanswered before piper is
// considered wedged and killed. A cancelled request does not kill piper:
// barge-in cancels often, and a restart costs a 1-3 s model load.
const stuckAfter = 15 * time.Second

var errStuck = errors.New("piper: no answer, killed")

// Piper drives one resident piper process in --json-input mode: a JSON line
// in per text, the written file's path out. Loading the voice model takes
// seconds, so the process is kept between replies.
type Piper struct {
	// Cmd is the command line, e.g. sh /data/local/tmp/soloist/chroot.sh
	// /opt/piper/piper --model /opt/piper/voice.onnx --json-input.
	Cmd []string
	// OutDir is where piper writes, as piper sees it (a path inside the
	// chroot); HostOutDir is the same directory as voiced sees it.
	OutDir, HostOutDir string

	stuck time.Duration // 0 = stuckAfter; tests shorten it

	semOnce sync.Once
	sem     chan struct{} // 1 slot: serialises Synth/Start, guards base and proc
	base    context.Context
	proc    *piperProc
	n       atomic.Uint64
}

type piperProc struct {
	stdin  io.WriteCloser
	lines  chan string   // stdout lines; closed when stdout ends
	done   chan struct{} // closed when the process is dropped
	exited chan struct{} // closed once the process has been reaped
	cancel context.CancelFunc
	once   sync.Once
}

// drop stops the process and releases its reader goroutines.
func (pp *piperProc) drop() {
	pp.once.Do(func() {
		close(pp.done)
		pp.cancel()
	})
}

func (pp *piperProc) alive() bool {
	select {
	case <-pp.exited:
		return false
	default:
		return true
	}
}

func (p *Piper) lock(ctx context.Context) error {
	p.semOnce.Do(func() { p.sem = make(chan struct{}, 1) })
	select {
	case p.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *Piper) unlock() { <-p.sem }

// Start launches the process so the first reply does not wait for the model
// load. The process lives until ctx ends. It is a no-op while a process is
// already running.
func (p *Piper) Start(ctx context.Context) error {
	if err := p.lock(ctx); err != nil {
		return err
	}
	defer p.unlock()
	if p.proc != nil && p.proc.alive() {
		return nil
	}
	p.base = ctx
	return p.startLocked()
}

func (p *Piper) startLocked() error {
	if len(p.Cmd) == 0 {
		return errors.New("piper: no command")
	}
	if p.proc != nil {
		p.proc.drop()
		p.proc = nil
	}
	if p.base == nil {
		p.base = context.Background()
	}
	if p.HostOutDir != "" { // leftovers from a previous run
		old, _ := filepath.Glob(filepath.Join(p.HostOutDir, "*.wav"))
		for _, f := range old {
			os.Remove(f)
		}
	}
	ctx, cancel := context.WithCancel(p.base)
	cmd := exec.CommandContext(ctx, p.Cmd[0], p.Cmd[1:]...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		return err
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return fmt.Errorf("piper: %w", err)
	}
	pp := &piperProc{stdin: stdin, lines: make(chan string, 16), done: make(chan struct{}),
		exited: make(chan struct{}), cancel: cancel}
	var readers sync.WaitGroup
	readers.Add(2)
	go func() {
		defer readers.Done()
		defer close(pp.lines)
		sc := bufio.NewScanner(stdout)
	scan:
		for sc.Scan() {
			select {
			case pp.lines <- sc.Text():
			case <-pp.done:
				break scan
			}
		}
		io.Copy(io.Discard, stdout) // never leave piper blocked on a full pipe
	}()
	go func() {
		defer readers.Done()
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			log.Printf("piper: %s", sc.Text())
		}
		io.Copy(io.Discard, stderr)
	}()
	go func() {
		readers.Wait() // Wait must follow the pipe reads
		cmd.Wait()
		cancel()
		close(pp.exited)
	}()
	p.proc = pp
	return nil
}

// Synth speaks text and returns the file's own rate (22050 for most voices).
// A process that died is restarted, once per call. If ctx ends first, the
// request is abandoned and piper keeps running; a process that does not
// answer within stuckAfter is killed and restarts on the next call.
func (p *Piper) Synth(ctx context.Context, text string) ([]int16, int, error) {
	if err := p.lock(ctx); err != nil {
		return nil, 0, err
	}
	defer p.unlock()
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		if p.proc == nil || !p.proc.alive() {
			if err = p.startLocked(); err != nil {
				return nil, 0, err
			}
		}
		var pcm []int16
		var rate int
		var dead bool
		pcm, rate, dead, err = p.once(ctx, text)
		if err == nil {
			return pcm, rate, nil
		}
		if !dead {
			return nil, 0, err
		}
		p.proc.drop()
		p.proc = nil
		if ctx.Err() != nil {
			return nil, 0, ctx.Err()
		}
		if errors.Is(err, errStuck) {
			return nil, 0, err
		}
	}
	return nil, 0, err
}

// dropStale deletes the file of an answer that arrived after its request
// was abandoned, if it lies in OutDir.
func (p *Piper) dropStale(line string) {
	if p.HostOutDir == "" || path.Dir(line) != path.Clean(p.OutDir) {
		return
	}
	if base := path.Base(line); strings.HasSuffix(base, ".wav") {
		os.Remove(filepath.Join(p.HostOutDir, base))
	}
}

// once runs one request; dead reports that the process must be replaced.
func (p *Piper) once(ctx context.Context, text string) (pcm []int16, rate int, dead bool, err error) {
	name := fmt.Sprintf("%d.wav", p.n.Add(1))
	want := path.Join(p.OutDir, name)
	host := filepath.Join(p.HostOutDir, name)
	defer os.Remove(host)

	req, _ := json.Marshal(struct {
		Text       string `json:"text"`
		OutputFile string `json:"output_file"`
	}{text, want})
	if _, err := p.proc.stdin.Write(append(req, '\n')); err != nil {
		return nil, 0, true, fmt.Errorf("piper: %w", err)
	}
	limit := p.stuck
	if limit == 0 {
		limit = stuckAfter
	}
	timer := time.NewTimer(limit)
	defer timer.Stop()
	for {
		select {
		case line, ok := <-p.proc.lines:
			if !ok {
				return nil, 0, true, errors.New("piper: process exited")
			}
			if line != want {
				p.dropStale(line) // an abandoned request's answer, or log output
				continue
			}
			b, err := os.ReadFile(host)
			if err != nil {
				return nil, 0, false, fmt.Errorf("piper: %w", err)
			}
			pcm, rate, err := wav.Decode(b)
			if err != nil {
				return nil, 0, false, fmt.Errorf("piper: %w", err)
			}
			if len(pcm) == 0 {
				return nil, 0, false, errors.New("piper: empty audio")
			}
			return pcm, rate, false, nil
		case <-ctx.Done():
			return nil, 0, false, ctx.Err()
		case <-timer.C:
			return nil, 0, true, errStuck
		}
	}
}
