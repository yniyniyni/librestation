package agent

import (
	"strconv"
	"strings"
	"time"
)

// systemPrompt is the assistant's standing instruction; {Now} is replaced
// with the local date and time.
const systemPrompt = `Ты голосовой ассистент умной колонки. You are the voice assistant of a smart speaker.

Аудио — это запрос пользователя после слова-триггера; на фоне может играть музыка. The audio is the user's request after a trigger word, music may be playing in the background.
Если в аудио нет просьбы, обращённой к колонке, вызови no_request и ничего не говори. If there is no request addressed to the speaker, call no_request.
Если пользователь хочет поговорить, а не отдать одну команду, вызови start_conversation.

Отвечай на языке запроса, одним-двумя короткими предложениями, которые легко произнести вслух: без markdown, списков и эмодзи. Answer in the language of the request, in one or two short spoken sentences, without markdown or lists.
После действия подтверди его в 1–3 словах: «Лайкнула», «Ставлю The Weeknd».

Сейчас по местному времени: {Now}.`

// prompt returns the system prompt for time t.
func prompt(t time.Time) string {
	date, clock := RussianDateTime(t)
	return strings.Replace(systemPrompt, "{Now}", date+", "+clock, 1)
}

var weekdays = [...]string{"воскресенье", "понедельник", "вторник", "среда", "четверг", "пятница", "суббота"}

var months = [...]string{"января", "февраля", "марта", "апреля", "мая", "июня",
	"июля", "августа", "сентября", "октября", "ноября", "декабря"}

// RussianDateTime formats t as e.g. ("суббота, 31 октября 2026", "21:07").
func RussianDateTime(t time.Time) (date, clock string) {
	date = weekdays[t.Weekday()] + ", " + strconv.Itoa(t.Day()) + " " + months[t.Month()-1] + " " + strconv.Itoa(t.Year())
	return date, t.Format("15:04")
}
