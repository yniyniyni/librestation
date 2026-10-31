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
	"sync"
	"sync/atomic"

	"voice/internal/wav"
)

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

	mu   sync.Mutex // serialises Synth and guards proc
	base context.Context
	proc *piperProc
	n    atomic.Uint64
}

type piperProc struct {
	stdin  io.WriteCloser
	lines  chan string // stdout lines; closed when the process ends
	cancel context.CancelFunc
	cmd    *exec.Cmd
}

func (pp *piperProc) kill() {
	pp.cancel()
	pp.cmd.Wait() // reaps; the reader goroutine ends with the pipe
}

// Start launches the process so the first reply does not wait for the model
// load. The process lives until ctx ends.
func (p *Piper) Start(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.base = ctx
	return p.startLocked()
}

func (p *Piper) startLocked() error {
	if len(p.Cmd) == 0 {
		return errors.New("piper: no command")
	}
	if p.base == nil {
		p.base = context.Background()
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
	lines := make(chan string, 16)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()
	go func() {
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			log.Printf("piper: %s", sc.Text())
		}
	}()
	p.proc = &piperProc{stdin: stdin, lines: lines, cancel: cancel, cmd: cmd}
	return nil
}

// Synth speaks text and returns the file's own rate (22050 for most voices).
// A process that died is restarted, once per call. If ctx ends first, the
// process is killed so a stuck piper cannot hold up the next reply.
func (p *Piper) Synth(ctx context.Context, text string) ([]int16, int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		if p.proc == nil {
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
		p.proc.kill()
		p.proc = nil
		if ctx.Err() != nil {
			return nil, 0, ctx.Err()
		}
	}
	return nil, 0, err
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
	for {
		select {
		case line, ok := <-p.proc.lines:
			if !ok {
				return nil, 0, true, errors.New("piper: process exited")
			}
			if line != want {
				continue // log output, not our answer
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
			return nil, 0, true, ctx.Err()
		}
	}
}
