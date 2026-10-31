package assistant

import (
	"strings"
	"time"
)

const (
	// infoWindow is the silence that ends a conversation after a plain fact
	// (weather, time); dialogWindow after everything else.
	infoWindow   = 4 * time.Second
	dialogWindow = 7 * time.Second
)

// closingTools end the conversation as soon as their reply has played.
var closingTools = map[string]bool{"play_music": true, "playback": true, "like_track": true, "end_conversation": true}

// infoTools give a fact and leave a short window for a follow-up.
var infoTools = map[string]bool{"get_weather": true, "get_time": true}

// dialogTools keep the conversation open for the full window.
var dialogTools = map[string]bool{"web_answer": true, "now_playing": true}

// toolResult is one tool call of a turn.
type toolResult struct {
	Name string
	OK   bool
}

// turnSummary is what closeAfter needs to know about a finished turn.
type turnSummary struct {
	Tools []toolResult
	Reply string // transcription of the spoken reply
}

// closeAfter decides how a Live session ends after a turn: now=true closes it
// once the reply has played; otherwise it waits wait of silence, counted from
// the end of the reply's audio. An action beats information in one turn; a
// failed action or a reply that asks a question keeps the dialog window.
func closeAfter(t turnSummary) (now bool, wait time.Duration) {
	closing, failed, info, ended := false, false, false, false
	for _, r := range t.Tools {
		switch {
		case closingTools[r.Name]:
			if r.OK {
				closing = true
				ended = ended || r.Name == "end_conversation"
			} else {
				failed = true
			}
		case infoTools[r.Name]:
			info = true
		}
	}
	if ended {
		return true, 0
	}
	if failed || endsWithQuestion(t.Reply) {
		return false, dialogWindow
	}
	if closing {
		return true, 0
	}
	if info {
		return false, infoWindow
	}
	return false, dialogWindow
}

// endsWithQuestion reports whether the reply ends with "?", ignoring spaces
// and closing quotes or brackets after it.
func endsWithQuestion(s string) bool {
	s = strings.TrimRight(s, " \t\r\n»\"”’')]}")
	return strings.HasSuffix(s, "?")
}
