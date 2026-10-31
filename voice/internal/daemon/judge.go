package daemon

import (
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"time"

	"voice/internal/judge"
	"voice/internal/kws"
)

// Hits are labelled by an audio model (Options.Judge) after undoWindow, so it
// is told whether the user took the action back. Its call becomes the hit's
// verdict unless the user has given one; a false alarm saves the clip as a
// hard negative of the word, as a manual "false" does.

const (
	// A call decides the verdict only when the model is fairly sure: a
	// command at judgeOK or more, a false alarm at judgeFalse or less. In
	// between it is only shown.
	judgeOK    = 0.7
	judgeFalse = 0.3
	// judgeQueue bounds hits waiting for the model; more are not judged.
	judgeQueue = 16
	// judgeTimeout bounds one call.
	judgeTimeout = 45 * time.Second
)

type judgeView struct {
	Available bool          `json:"available"` // a model is configured (key present)
	On        bool          `json:"on"`
	Model     string        `json:"model,omitempty"`
	Models    []judgeChoice `json:"models"`
	Pending   int           `json:"pending"`
}

type judgeChoice struct {
	ID   string `json:"id"`
	Note string `json:"note"`
}

func (d *Daemon) judgeViewLocked() judgeView {
	v := judgeView{Available: d.opt.Judge != nil, On: d.opt.Judge != nil && !d.settings.JudgeOff,
		Model: d.judgeModelLocked(), Pending: len(d.judgeQ)}
	for _, m := range judge.Models {
		v.Models = append(v.Models, judgeChoice{m.ID, m.Note})
	}
	return v
}

// judgeModelLocked is the model hits go to; caller holds d.mu.
func (d *Daemon) judgeModelLocked() string {
	if d.settings.JudgeModel != "" {
		return d.settings.JudgeModel
	}
	return d.opt.JudgeModel
}

// judgeLaterLocked queues h for the model once its take-back window is over;
// caller holds d.mu.
func (d *Daemon) judgeLaterLocked(h *Hit) {
	if d.opt.Judge == nil || d.settings.JudgeOff {
		return
	}
	time.AfterFunc(d.judgeDelay, func() { d.enqueueJudge(h) })
}

func (d *Daemon) enqueueJudge(h *Hit) {
	select {
	case d.judgeQ <- h:
	default:
		d.mu.Lock()
		h.JudgeError = "очередь проверки переполнена"
		d.mu.Unlock()
		d.persist(h)
	}
}

// runJudge asks the model about queued hits one at a time until ctx ends.
func (d *Daemon) runJudge(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case h := <-d.judgeQ:
			d.judgeHit(ctx, h)
		}
	}
}

func (d *Daemon) judgeHit(ctx context.Context, h *Hit) {
	d.mu.Lock()
	if h.pruned || d.opt.Judge == nil {
		d.mu.Unlock()
		return
	}
	in := judge.Hit{Word: h.Name, Action: h.Action, Rate: kws.SampleRate, Undone: h.Undone, Playback: h.Playback,
		Model: d.judgeModelLocked()}
	if in.Word == "" {
		in.Word = h.Word
	}
	d.mu.Unlock()
	audio, err := d.hitAudio(h)
	if err == nil && len(audio) == 0 {
		err = errors.New("у срабатывания нет звука")
	}
	var v judge.Verdict
	if err == nil {
		in.Audio = audio
		cctx, cancel := context.WithTimeout(ctx, judgeTimeout)
		v, err = d.opt.Judge.Judge(cctx, in)
		cancel()
	}
	if ctx.Err() != nil {
		return
	}
	d.ioMu.Lock() // applyJudgeLocked may take back a false clip being written
	d.mu.Lock()
	if err != nil {
		h.JudgeError = err.Error()
		log.Printf("judge hit %d %s: %v", h.ID, h.Word, err)
	} else {
		h.Judge, h.JudgeError = &v, ""
		log.Printf("judge hit %d %s: command probability %.2f, heard %q: %s", h.ID, h.Word, v.Probability, v.Heard, v.Reason)
		d.applyJudgeLocked(h)
	}
	d.mu.Unlock()
	d.ioMu.Unlock()
	d.persist(h)
}

// applyJudgeLocked turns a confident call into the verdict. A verdict the
// user gave is never changed; one the station gave itself (a take-back) is.
// Caller holds d.ioMu and d.mu.
func (d *Daemon) applyJudgeLocked(h *Hit) {
	v := h.Judge
	if v == nil || (h.Verdict != "" && !h.Auto) {
		return
	}
	switch {
	case v.Probability <= judgeFalse:
		d.markFalseLocked(h, true)
		return
	case v.Probability < judgeOK:
		return
	}
	if ff := h.FalseFile; ff != "" {
		// A take-back marked it false, the model heard a real command.
		os.Remove(filepath.Join(d.wordDir(h.Word), ff))
	}
	h.Verdict, h.Auto, h.FalseFile = "ok", true, ""
}

// rejudge queues h again (the user asked, e.g. after an error).
func (d *Daemon) rejudge(h *Hit) bool {
	d.mu.Lock()
	ok := d.opt.Judge != nil
	if ok {
		h.JudgeError = ""
	}
	d.mu.Unlock()
	if ok {
		d.enqueueJudge(h)
	}
	return ok
}
