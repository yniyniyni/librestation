package main

import (
	"context"
	"log"
	"sync"
	"time"

	"voice/internal/alarms"
	"voice/internal/led/proto"
)

const (
	// musicWait is how long a music alarm waits for the music before ringing.
	musicWait = 10 * time.Second
	// timerMinVolume keeps a timer audible when the music was left very quiet.
	timerMinVolume = 30
	// ringEvery is how often a ring looks at the time and tells ledd it is on.
	ringEvery = time.Second
)

// ringer makes the ring on the hardware; it is alarms.Ringer. hush, music and led
// may be nil: a ring needs neither the assistant nor Spotify.
type ringer struct {
	sounds  string // the ringtones' directory
	play    func(ctx context.Context, pcm []int16, rate int) error
	flush   func() // drops the audio already in the device
	hold    func(v int)
	volume  func() int
	playing func() bool
	pause   func() error
	resume  func() error
	hush    func()
	music   func(ctx context.Context, query string) error
	led     func(proto.Event)
	now     func() time.Time
	sleep   func(ctx context.Context, d time.Duration) error
	ticks   func() (<-chan time.Time, func())

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{} // closes when the last ring is over
}

func newTicks() (<-chan time.Time, func()) {
	t := time.NewTicker(ringEvery)
	return t.C, t.Stop
}

// Start begins a ring once the one before it has finished.
func (r *ringer) Start(ring alarms.Ring) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	r.mu.Lock()
	prev := r.done
	r.cancel, r.done = cancel, done
	r.mu.Unlock()
	go func() {
		defer close(done)
		if prev != nil {
			<-prev
		}
		r.run(ctx, ring)
	}()
}

// Stop ends the ring. A snoozed one ends the same way.
func (r *ringer) Stop(bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cancel != nil {
		r.cancel()
	}
}

// wait returns when the last ring has finished.
func (r *ringer) wait() {
	r.mu.Lock()
	done := r.done
	r.mu.Unlock()
	if done != nil {
		<-done
	}
}

func (r *ringer) show(kind alarms.Kind) {
	if r.led != nil {
		r.led(proto.Event{T: "ring", A: string(kind)})
	}
}

func (r *ringer) run(ctx context.Context, ring alarms.Ring) {
	start := r.now()
	if r.hush != nil {
		r.hush()
	}
	wasPlaying := r.playing()
	level := alarms.RingLevel(0)
	ramps := ring.Kind == alarms.KindAlarm
	if !ramps {
		level = max(r.volume(), timerMinVolume)
	}
	r.hold(level)
	r.show(ring.Kind)

	var melody sync.WaitGroup
	soundOn, resume := false, false
	startSound := func() {
		soundOn = true
		// What played stays where it was: the ring only interrupts it.
		if r.playing() {
			if err := r.pause(); err != nil {
				log.Printf("ring: pause: %v", err)
			}
			resume = wasPlaying
		}
		melody.Add(1)
		go func() {
			defer melody.Done()
			r.loopSound(ctx, ring.Sound)
		}()
	}

	var musicDone chan error
	waiting := ring.Music != "" && r.music != nil
	if waiting {
		musicDone = make(chan error, 1)
		go func() { musicDone <- r.music(ctx, ring.Music) }()
	} else {
		startSound()
	}
	musicOn := false
	asked := false // the request for the music came back without an error

	ticks, stop := r.ticks()
	defer stop()
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case err := <-musicDone:
			musicDone = nil
			if err != nil {
				log.Printf("ring: music: %v; ringing instead", err)
				waiting = false
				startSound()
			} else {
				asked = true
			}
		case now := <-ticks:
			elapsed := now.Sub(start)
			if ramps {
				if l := alarms.RingLevel(elapsed); l != level {
					level = l
					r.hold(level)
				}
			}
			r.show(ring.Kind)
			if waiting {
				if asked && r.playing() {
					waiting, musicOn = false, true
				} else if elapsed >= musicWait {
					log.Printf("ring: no music after %v; ringing instead", musicWait)
					waiting = false
					startSound()
				}
			}
		}
	}

	melody.Wait()
	if soundOn {
		r.flush()
	}
	r.show("")
	r.hold(-1)
	if musicOn || waiting {
		if err := r.pause(); err != nil {
			log.Printf("ring: pause: %v", err)
		}
	} else if resume {
		if err := r.resume(); err != nil {
			log.Printf("ring: resume: %v", err)
		}
	}
}

// loopSound plays the ringtone round and round. An output that fails is tried
// again each second: only the end of the ring ends the trying.
func (r *ringer) loopSound(ctx context.Context, name string) {
	pcm, rate := alarms.LoadSound(r.sounds, name)
	for ctx.Err() == nil {
		if err := r.play(ctx, pcm, rate); err != nil && ctx.Err() == nil {
			log.Printf("ring: %v", err)
			r.sleep(ctx, time.Second)
		}
	}
}

// showTimers keeps ledd told what is left on the nearest timer.
func showTimers(ctx context.Context, m *alarms.Manager, led func(proto.Event)) {
	t := time.NewTicker(ringEvery)
	defer t.Stop()
	shown := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			secs, ok := m.NearestTimer()
			if ok || shown {
				led(proto.Event{T: "timer", V: secs})
			}
			shown = ok
		}
	}
}
