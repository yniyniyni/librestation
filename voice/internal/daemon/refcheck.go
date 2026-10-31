package daemon

import (
	"context"
	"log"
	"math"
	"sync"
	"time"

	"voice/internal/kws"
)

// The playback reference is what the speaker itself plays. While music is
// on, refWatch embeds it like the mic stream, so a hit can be scored against
// it: a word the model also finds in the song is the song, not the user.
// Nothing is computed while the speaker is silent.

const (
	// refBar is the share of a word's threshold that its score on the
	// playback must reach for the hit to count as coming from the music. The
	// reference is cleaner than what the mic hears through the room and the
	// echo canceller, so it scores lower on the same lyric.
	refBar = 0.8
	// refEmbs is the history kept: one window plus ~1 s of window ends, so
	// the song's word is found even if it ended a little before the hit.
	refEmbs = kws.Ctx + 12
	// refSilence is the block RMS (int16 scale) below which the speaker is
	// treated as silent (about -60 dBFS).
	refSilence = 32
	// refTarget is the RMS the reference is levelled to before embedding,
	// about the level of a word in the user's takes (-26 dBFS).
	refTarget = 1640
	// refFresh is how recent the last embedding of audible playback must be
	// for a hit to be checked at all.
	refFresh = time.Second
)

type refWatch struct {
	in  chan []float32
	now func() time.Time

	feat *kws.Features // used only by push
	env  float64       // smoothed block RMS for levelling (push only)
	on   bool          // last block was audible (push only)

	mu      sync.Mutex
	embs    []float32 // last n embeddings, oldest first
	n       int
	lastEmb time.Time // when the last embedding of audible playback was added
	load    float64   // smoothed CPU time per second of reference audio
	dropped int       // blocks dropped because push fell behind
}

func newRefWatch(now func() time.Time) (*refWatch, error) {
	f, err := kws.NewFeatures()
	if err != nil {
		return nil, err
	}
	return &refWatch{
		in:   make(chan []float32, 50), // 1 s of 20 ms blocks
		now:  now,
		feat: f,
		embs: make([]float32, refEmbs*kws.EmbDim),
	}, nil
}

// offer queues a reference block without blocking the audio goroutine.
func (r *refWatch) offer(blk []float32) {
	select {
	case r.in <- blk:
	default:
		r.mu.Lock()
		r.dropped++
		r.mu.Unlock()
	}
}

// run embeds queued blocks until ctx ends.
func (r *refWatch) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case blk := <-r.in:
			r.push(blk)
		}
	}
}

// push embeds one block of the reference (16 kHz, int16 scale).
func (r *refWatch) push(blk []float32) {
	if len(blk) == 0 {
		return
	}
	var e float64
	for _, v := range blk {
		e += float64(v) * float64(v)
	}
	rms := math.Sqrt(e / float64(len(blk)))
	if rms < refSilence {
		if r.on {
			// Pause or silence between tracks: start clean when it resumes.
			r.on = false
			r.feat.Reset()
			r.mu.Lock()
			r.n = 0
			r.mu.Unlock()
		}
		return
	}
	if !r.on {
		r.on, r.env = true, rms
	}
	start := time.Now()
	r.env = 0.95*r.env + 0.05*rms // ~0.4 s at 20 ms blocks
	g := min(max(refTarget/r.env, 0.01), 100)
	pcm := make([]int16, len(blk))
	for i, v := range blk {
		pcm[i] = int16(min(max(float64(v)*g, -32768), 32767))
	}
	r.feat.Push(pcm, func(emb []float32) {
		r.mu.Lock()
		copy(r.embs, r.embs[kws.EmbDim:])
		copy(r.embs[(refEmbs-1)*kws.EmbDim:], emb)
		r.n = min(r.n+1, refEmbs)
		r.lastEmb = r.now()
		r.mu.Unlock()
	})
	busy := time.Since(start).Seconds() / (float64(len(blk)) / kws.SampleRate)
	r.mu.Lock()
	r.load = 0.99*r.load + 0.01*busy
	r.mu.Unlock()
}

// score returns the best score of head over the recent playback windows;
// ok is false when the speaker has not been playing just now.
func (r *refWatch) score(head *kws.Head) (best float32, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.n < kws.Ctx || r.now().Sub(r.lastEmb) > refFresh {
		return 0, false
	}
	s := kws.Stream(r.embs)
	for e := refEmbs - r.n + kws.Ctx - 1; e < refEmbs; e++ {
		best = max(best, head.Score(s.Window(e)))
	}
	return best, true
}

type refView struct {
	On      bool    `json:"on"`
	Playing bool    `json:"playing"` // the speaker is playing right now
	Load    float64 `json:"load"`    // CPU share of one core spent on the check
	Dropped int     `json:"dropped"`
}

func (r *refWatch) view(on bool) refView {
	r.mu.Lock()
	defer r.mu.Unlock()
	return refView{On: on, Playing: on && r.n > 0 && r.now().Sub(r.lastEmb) < refFresh, Load: r.load, Dropped: r.dropped}
}

// refScoreLocked checks the word against the playback; caller holds d.mu.
// bar is the score at which the hit counts as coming from the music.
func (d *Daemon) refScoreLocked(id string) (score, bar float32, ok bool) {
	if d.refw == nil || !d.settings.RefCheck {
		return 0, 0, false
	}
	var m *kws.Model
	for _, w := range d.words {
		if w.ID == id {
			m = w.Model
		}
	}
	if m == nil {
		return 0, 0, false
	}
	thr := m.Threshold
	if s := d.settings.Words[id]; s != nil {
		thr -= s.Sensitivity
	}
	thr = min(0.99, max(0.05, thr))
	score, ok = d.refw.score(m.Head)
	if ok {
		log.Printf("ref check %s: playback score %.3f (bar %.3f)", id, score, refBar*thr)
	}
	return score, refBar * thr, ok
}
