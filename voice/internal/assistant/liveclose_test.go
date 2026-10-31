package assistant

import (
	"testing"
	"time"

	"voice/internal/agent"
	"voice/internal/tools"
)

func TestCloseAfter(t *testing.T) {
	ok := func(names ...string) []toolResult {
		var r []toolResult
		for _, n := range names {
			r = append(r, toolResult{n, true})
		}
		return r
	}
	for _, tc := range []struct {
		name string
		turn turnSummary
		now  bool
		wait time.Duration
	}{
		{"play_music ok", turnSummary{Tools: ok("play_music"), Reply: "Включаю."}, true, 0},
		{"playback ok", turnSummary{Tools: ok("playback"), Reply: "Пауза."}, true, 0},
		{"like_track ok", turnSummary{Tools: ok("like_track"), Reply: "Лайкнула."}, true, 0},
		{"end_conversation ok", turnSummary{Tools: ok("end_conversation"), Reply: "Пока."}, true, 0},
		{"end_conversation with question", turnSummary{Tools: ok("end_conversation"), Reply: "Точно хочешь закончить?"}, true, 0},
		{"play_music failed", turnSummary{Tools: []toolResult{{"play_music", false}}, Reply: "Не получилось."}, false, dialogWindow},
		{"action then failed action", turnSummary{Tools: []toolResult{{"playback", true}, {"play_music", false}}}, false, dialogWindow},
		{"action with question", turnSummary{Tools: ok("play_music"), Reply: "Включаю. Сделать погромче?"}, false, dialogWindow},
		{"question with closing quote", turnSummary{Tools: ok("play_music"), Reply: "Включить «Утро»? »"}, false, dialogWindow},
		{"question with trailing spaces", turnSummary{Tools: ok("playback"), Reply: "Погромче? \n"}, false, dialogWindow},
		{"get_weather", turnSummary{Tools: ok("get_weather"), Reply: "Плюс пять."}, false, infoWindow},
		{"get_time", turnSummary{Tools: ok("get_time"), Reply: "Десять часов."}, false, infoWindow},
		{"info with question", turnSummary{Tools: ok("get_weather"), Reply: "Плюс пять. Назвать на завтра?"}, false, dialogWindow},
		{"action beats info", turnSummary{Tools: ok("get_weather", "play_music"), Reply: "Включаю."}, true, 0},
		{"info after action", turnSummary{Tools: ok("play_music", "get_time"), Reply: "Включаю."}, true, 0},
		{"web_answer", turnSummary{Tools: ok("web_answer"), Reply: "Вот."}, false, dialogWindow},
		{"now_playing", turnSummary{Tools: ok("now_playing"), Reply: "Играет Кино."}, false, dialogWindow},
		{"no tools", turnSummary{Reply: "Привет."}, false, dialogWindow},
		{"empty turn", turnSummary{}, false, dialogWindow},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now, wait := closeAfter(tc.turn)
			if now != tc.now || wait != tc.wait {
				t.Fatalf("closeAfter = (%v, %v), want (%v, %v)", now, wait, tc.now, tc.wait)
			}
		})
	}
}

// TestEveryToolClassified fails when a tool has no close class, so a new tool
// cannot silently fall into the default dialog window. Keep the list in step
// with buildAssistant in cmd/voiced/assistant.go.
func TestEveryToolClassified(t *testing.T) {
	all := []agent.Tool{
		tools.Clock{}, tools.Weather{}, tools.Web{}, tools.Playback{},
		tools.PlayMusic{}, tools.LikeTrack{}, tools.NowPlaying{},
	}
	for _, tool := range all {
		name := tool.Decl().Name
		n := 0
		for _, set := range []map[string]bool{closingTools, infoTools, dialogTools} {
			if set[name] {
				n++
			}
		}
		if n != 1 {
			t.Errorf("tool %q is in %d close classes, want exactly 1", name, n)
		}
	}
	if !closingTools["end_conversation"] {
		t.Error("end_conversation is not a closing tool")
	}
}
