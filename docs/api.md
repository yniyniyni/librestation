# REST API колонки

Этим API пользуются веб-панель и `setup`, отдельных путей у них нет. На нём можно писать мобильное
приложение. Версия 1.

## Основное

- База: `http://<колонка>:8780/api/v1`. Только локальная сеть, без HTTPS.
- Формат: JSON в UTF-8. Записи отдаются как `audio/wav`. Время в формате RFC 3339.
- Авторизации нет: управлять колонкой может любое устройство в этой сети.
- Запрос, меняющий состояние, с заголовком `Origin` другого хоста получает 403, а CORS выключен. Так
  сайт, открытый в браузере, не может управлять колонкой. Приложения на этой защите не спотыкаются:
  нативный клиент `Origin` не шлёт.
- Запись слова занимает секунды: запросы `POST /enroll/ambient`, `/takes` и `/speech` отвечают
  только по её окончании, поэтому держите таймаут запроса не меньше 30 с.

### Версии

Новые поля и новые адреса появляются без смены версии, поэтому незнакомые поля надо игнорировать.
Несовместимое изменение означает новый префикс `/api/v2`.

### Ошибки

```json
{"error": {"code": "not_found", "message": "нет такого слова"}}
```

`code` предназначен для программы, `message` можно показать человеку.

| Статус | `code` | Когда |
|---|---|---|
| 400 | `bad_request` | неверный JSON или значение |
| 403 | `forbidden` | запрос с чужого сайта |
| 404 | `not_found` | нет такого адреса или ресурса, в том числе неверный метод |
| 409 | `conflict` | нельзя сейчас: идёт запись или обучение, нет ключа, нет звука |
| 499 | `canceled` | клиент закрыл запрос во время записи |
| 500 | `internal` | не удалось сохранить настройки или файл |
| 502 | `led_unavailable` | сервис экрана не отвечает |
| 503 | `too_many_subscribers` | уже открыто 4 потока событий |

## Как найти колонку

Колонка отвечает на запросы mDNS: сервис `_librestation._tcp`, порт из записи SRV, в TXT `api=1` и
`path=/api/v1`. Если сеть режет multicast, приложение должно дать ввести адрес вручную. После
обнаружения первым запросом идёт `GET /info`.

```bash
dns-sd -B _librestation._tcp
```

## Как получать состояние

Один поток событий вместо опроса: `GET /state` нужен только там, где поток не подходит.

`GET /events` отдаёт [Server-Sent Events](https://html.spec.whatwg.org/multipage/server-sent-events.html):

| Событие | Данные | Когда |
|---|---|---|
| `state` | как `GET /state` | при подключении и при изменении; после изменения через API сразу, иначе в течение секунды |
| `hits` | как `GET /hits` | при подключении и при изменении |
| `live` | см. ниже | 4 раза в секунду, только с `?live=1` |

```json
{"level": -46.3, "audio_ok": true, "scores": {"alexa": 0.02}, "assistant": "idle", "volume": 31}
```

`level` в dBFS, `scores` это текущая уверенность по включённым словам (0..1), `assistant` одно из
`idle`, `listening`, `thinking`, `speaking`, `followup`. Поля `assistant` и `volume` отсутствуют, если
такой функции нет.

Правила, ради которых колонка не тратит CPU зря:

- **Закрывайте поток,** когда приложение свёрнуто или экран не виден. Без подписчиков сервер не
  делает для API ничего.
- **Просите `live=1`,** только пока показываете индикаторы.
- Медленный клиент теряет устаревшие значения, но всегда получает последние. Раз в 25 с приходит
  комментарий `: ping`.

## Ресурсы

### Станция

**`GET /info`**

```json
{"api_version": 1, "name": "localhost",
 "features": {"assistant": true, "judge": true, "led": true, "player": true, "quick": true, "volume": true}}
```

**`GET /state`** отдаёт снимок. `null` у `volume` значит, что управления громкостью нет; `null` у
`enroll` значит, что запись слова не идёт.

```json
{
  "station": "localhost",
  "audio": {"ok": true, "level": -46.3},
  "player": {"state": "paused"},
  "volume": {"volume": 31, "knob_step": 1},
  "words": [{"id": "alexa", "name": "Alexa", "enabled": true, "sensitivity": 0, "action": "assistant",
             "threshold": 0.8, "recall": 0.81, "false_per_h": 0, "takes": 16, "false_clips": 89,
             "new_falses": 6, "created": "2026-11-01T16:54:17Z", "score": 0.02, "notes": []}],
  "enroll": null,
  "train": {"active": false, "frac": 0},
  "judge": {"available": true, "on": false, "model": "gemini-3.5-flash-lite",
            "models": [{"id": "gemini-3.5-flash-lite", "note": "…"}], "pending": 0},
  "settings": {"judge": false, "auto_false_undo": false},
  "assistant": {"available": true, "status": {"state": "idle"}, "spotify_linked": true, "piper_missing": false,
                "settings": {"tts": "gemini", "voice": "Kore", "city": "Москва"}, "default_model": "gemini-3.5-flash-lite"},
  "quick": {"available": true, "off": false, "groups": [{"id": "pause", "words": "стоп, пауза", "on": true}]}
}
```

`train.stage` принимает значения `load`, `embed`, `train`, `calibrate`, `frac` это доля стадии (0..1),
`train.error` появляется при неудаче.

`enroll`, когда идёт запись:

```json
{"name": "Джарвис", "takes": [0.61, 0.58], "has_ambient": true, "speech_secs": 0, "min_takes": 6}
```

`takes` это длина слова в секундах по каждой записи.

### Громкость и музыка

**`GET /volume`** отдаёт `{"volume": 31, "knob_step": 1}`.

**`PATCH /volume`** с полями `step` (от -100 до 100, изменение громкости) и `knob_step` (шаг
колёсика, от 1 до 10). Отвечает новым состоянием.

**`POST /player/{pause|play|next|prev}`** отвечает `{"ok": true}`.

### Срабатывания

**`GET /hits`** отдаёт `{"hits": [...]}`, новые первыми.

```json
{"id": 41, "word": "alexa", "name": "Alexa", "score": 0.91, "at": "2026-11-02T22:57:01+03:00",
 "action": "assistant", "fired": true, "playback": false, "source": "", "verdict": "", "undone": 0}
```

Необязательные поля: `debounced`, `error`, `judge` (`probability`, `heard`, `reason`), `judge_error`,
`undone` (через сколько секунд команду отменили), `verdict` (`false` или `ok`), `auto` (разметила
станция), `source` (`yandex` у быстрых команд).

**`GET /hits/{id}/audio`** отдаёт запись в WAV.

**`PUT /hits/{id}/verdict`** с телом `{"verdict": "false"}` или `{"verdict": "ok"}`. `false` сохраняет
запись как ложную для обучения, `ok` возвращает обратно.

**`POST /hits/{id}/judge`** отправляет запись на проверку Gemini ещё раз. Без ключа ответит 409.

### Слова

**`PATCH /words/{id}`** с любыми из полей `enabled`, `sensitivity` (от -0.3 до 0.3), `action`
(`none`, `pause`, `play`, `next`, `prev`, `louder`, `quieter`, `assistant`).

**`DELETE /words/{id}`** удаляет слово со всеми записями.

**`POST /words/{id}/retrain`** запускает обучение заново. Ход обучения виден в `state.train`.

### Запись нового слова

Порядок: начать, записать фон, записать минимум `min_takes` произнесений, по желанию речь, обучить.
`POST /enroll` с именем существующего слова дозаписывает его.

| Запрос | Тело | Ответ |
|---|---|---|
| `POST /enroll` | `{"name": "Джарвис"}` | `enroll` |
| `DELETE /enroll` | | `{"ok": true}` |
| `POST /enroll/ambient` | | `{"level": -52.1}`, через 8 с |
| `POST /enroll/takes` | | `{"ok": true, "duration": 0.6, "level": -24.1, "takes": 3}` |
| `GET /enroll/takes/{i}` или `/last` | | WAV |
| `DELETE /enroll/takes/{i}` | | `enroll` |
| `POST /enroll/speech` | `{"seconds": 20}` | `{"level": -30.2}` |
| `POST /enroll/train` | `{"action": "assistant"}`, необязательно | `{"ok": true}` |

Неудачное произнесение это не ошибка: придёт `"ok": false` и пояснение в `message`.

### Настройки

**`PATCH /settings`** с любыми из полей:

```json
{
  "judge": true,
  "judge_model": "gemini-3.5-flash-lite",
  "auto_false_undo": false,
  "assistant": {"model": "", "tts": "gemini", "voice": "Kore", "city": "Москва"},
  "quick": {"off": false, "group": "pause", "on": true}
}
```

В `quick` поля `group` и `on` передаются только вместе. Пустая `assistant.model` значит «как у проверки».

### Экран и кольцо

**`GET /led`**

```json
{"settings": {"volume_screen": "number", "ai_ring": "carousel", "ai_screen": "dots",
              "flash": "sweep", "music": "spectrum", "brightness": 69}, "ai": false}
```

`brightness` 0 значит «подбирается сама», иначе от 1 до 100. Значения: `volume_screen` это `pulse`,
`number`; `ai_ring` это `carousel`, `flow`, `stock`; `ai_screen` это `dots`, `eq`; `flash` это
`sweep`, `white`, `color`; `music` это `spectrum`, `scope`, `clockbar`.

**`PUT /led/settings`** принимает весь объект `settings`. **`PUT /led/ai`** с телом `{"on": true}`
показывает режим ИИ. Оба отвечают как `GET /led`.

### Отладка

**`POST /record`** с `{"seconds": 10}` пишет необработанный звук в файл на колонке, **`GET /record`**
показывает ход. Панель этим не пользуется.
