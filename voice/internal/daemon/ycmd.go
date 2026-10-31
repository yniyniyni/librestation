package daemon

import (
	"log"
	"math"
	"time"

	"voice/internal/kws"
	"voice/internal/ycmd"
)

// Yandex's quick commands: its stock spotter listens on channel 0 of the raw
// capture and its hits run playback commands, but only while that cannot be
// a request to the assistant or a stray word in a quiet room.

// playWindow: "продолжи" resumes the music only this soon after it last
// played or after voiced started.
const playWindow = 15 * time.Minute

// quickCommand is what one of the model's commands does; group is the
// panel's switch for it.
type quickCommand struct{ action, group string }

// quickCommands are the commands used; the model's others are ignored.
var quickCommands = map[string]quickCommand{
	"стоп":      {"pause", "pause"},
	"хватит":    {"pause", "pause"},
	"пауза":     {"pause", "pause"},
	"дальше":    {"next", "next"},
	"следующий": {"next", "next"},
	"назад":     {"prev", "prev"},
	"громче":    {"louder", "volume"},
	"тише":      {"quieter", "volume"},
	"продолжи":  {"play", "play"},
}

// quickGroups are the panel's switches in display order, with the words
// each one covers.
var quickGroups = []struct{ id, words string }{
	{"pause", "стоп, хватит, пауза"},
	{"next", "дальше, следующий"},
	{"prev", "назад"},
	{"volume", "громче, тише"},
	{"play", "продолжи"},
}

// spotter is the Yandex recogniser as the daemon uses it (*ycmd.Spotter).
type spotter interface {
	Push(pcm []float32, fire func(cmd string, conf float64))
}

// spotCommands runs the spotter over one raw capture block. It runs on the
// audio goroutine without d.mu.
func (d *Daemon) spotCommands(block []int16) {
	if d.spot == nil {
		return
	}
	pcm := d.yin.Process(block)
	// The clip keeps channel 0 at its own level, like our words' clips; the
	// spotter gets it at InputGain.
	clip := make([]int16, len(pcm))
	for i, v := range pcm {
		clip[i] = int16(max(min(math.Round(float64(v)/ycmd.InputGain), math.MaxInt16), math.MinInt16))
	}
	d.mu.Lock()
	d.yring.write(clip)
	d.mu.Unlock()
	d.spot.Push(pcm, func(cmd string, conf float64) {
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.paused == 0 {
			d.onCommand(cmd, conf)
		}
	})
}

// onCommand records a Yandex command and runs it when the situation allows;
// caller holds d.mu.
func (d *Daemon) onCommand(cmd string, conf float64) {
	q, ok := quickCommands[cmd]
	if !ok || d.settings.Quick.Off || d.settings.Quick.Disabled[q.group] {
		return
	}
	now := d.now()
	playing := d.playback == "playing"
	h := &Hit{Word: cmd, Name: cmd, Score: float32(conf), At: now, Source: "yandex", Action: q.action,
		volBefore: d.volume, Playback: playing}
	h.audio = d.yring.last(hitSecs * kws.SampleRate)
	state := "idle"
	if a := d.opt.Assistant; a != nil {
		state = a.State()
	}
	var action string
	switch {
	case state == "speaking" && q.group == "pause":
		h.Action, action = "hush", "hush"
	case state != "idle":
		h.Debounced = true // a request or the reply: not a command
	case playing && q.action == "play":
		h.Debounced = true // already playing
	case playing:
		action = q.action
	case q.action == "play" && (now.Sub(d.lastPlaying) <= playWindow || now.Sub(d.started) <= playWindow):
		action = "play" // Spotify resumes where it was
	}
	if action != "" && !d.settings.DryRun {
		d.actLocked(h, action)
	}
	d.addHitLocked(h)
	d.judgeLaterLocked(h)
	log.Printf("yandex command %q conf %.3f state %s action %q fired %v debounced %v",
		cmd, conf, state, action, h.Fired, h.Debounced)
}
