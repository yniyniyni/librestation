# spoti-alice

Яндекс Станция Макс без Яндекса: Spotify Connect, локальные голосовые команды и
своя индикация на кольце и экране. Всё работает прямо на колонке, без облака.

*Yandex Station Max (rooted, Android 9) turned into a local Spotify Connect speaker
with on-device voice commands and its own LED ring/screen UI. Docs are in Russian.*

> Неофициальный проект. Не связан с Яндексом или Spotify. Нужна рутованная
> колонка; всё, что здесь описано, вы делаете на свой страх и риск.

## Что внутри

| Часть | Где | Что делает |
|---|---|---|
| Spotify Connect | `soloist/` | Официальный Spotify Soloist в Debian-chroot → PulseAudio (float) → FIFO → `aaudio_pipe` → audioserver. Сервис поднимает и перезапускает всё сам. |
| `aaudio_pipe` | `aaudio-pipe/` | Мост FIFO → AAudio (NDK, C). Компенсирует программную громкость Soloist, чтобы громкостью управлял усилитель. |
| `voiced` | `voice/cmd/voiced` | Свои голосовые команды без слова-активатора (пауза, продолжи, дальше, назад, громче, тише), обучение на голосе пользователя прямо на колонке, эхоподавление, единая громкость (Spotify, кольцо-крутилка, голос). Веб-панель на `:8780`. |
| `ledd` | `voice/cmd/ledd` | Своя индикация: часы, спектр музыки, громкость, реакция на команды, режим «слушаю» для будущего ИИ. Кольцо (24 RGB) и монохромный экран 25×16, автояркость по датчику света. |

Дизайн и решения — в `docs/`: голосовые команды
([docs/2026-10-29-voice-commands-design.md](docs/2026-10-29-voice-commands-design.md)),
кольцо и экран ([docs/2026-10-29-led-ui-design.md](docs/2026-10-29-led-ui-design.md)),
замеры железа ([docs/led-hw-notes.md](docs/led-hw-notes.md)).

## Что нужно

- Яндекс Станция Макс (`yandexstation_2`) с root через `mysudo` и adb по сети.
- Сборка Spotify Soloist для `linux/aarch64` и **свой** developer API key Soloist —
  в репозитории их нет.
- Debian rootfs (arm64) с PulseAudio для chroot — `soloist/setup-rootfs.sh` распаковывает его.
- На Mac/Linux: Go 1.24, Android SDK (build-tools, `d8`) и NDK — для `mic.dex` и `aaudio_pipe`.

## Установка (кратко)

```sh
# 1. Ключ Soloist — вводится скрыто, сохраняется только на колонке
zsh soloist/put-key.zsh

# 2. Сборка и заливка voiced, ledd, aaudio_pipe, скриптов сервиса
ADB_DEVICE=<ip-колонки>:5555 sh voice/deploy.sh

# 3. Запуск (на колонке, от root)
sh /data/local/tmp/soloist/start.sh
```

Для автозапуска добавьте `sh /data/local/tmp/soloist/start.sh` в загрузочный скрипт
вашего способа рута. Остановить всё: `sh /data/local/tmp/soloist/stop.sh`.

Слова команд записываются и обучаются на странице `http://<ip-колонки>:8780`;
там же — громкость, шаг кольца, стили индикации и яркость экрана.

## Яндекс-компоненты

Голосовой стек Яндекса (`com.yandex.io.sdk`) мешает: держит микрофон и перерисовывает
кольцо и экран. `voiced` его останавливает; насовсем его можно убрать обратимо:
`pm uninstall -k --user 0 com.yandex.io.sdk` (вернуть — `cmd package install-existing
com.yandex.io.sdk`). Стоковые анимации экрана Яндекса в репозиторий **не входят**:
`deploy.sh` копирует их на самой колонке из данных io.sdk, если они там есть.

## Благодарности и лицензии сторонних частей

- Распознавание слов — порт фронтенда и speech embedding из
  [openWakeWord](https://github.com/dscripka/openWakeWord) на Go. Веса speech
  embedding (`voice/internal/kws/assets/oww.bin`) — Google, Apache 2.0.
- Набор негативов для обучения (`voice/tools/build_negatives.py` скачивает его в
  `dist/`, в репозитории его нет) — [openwakeword_features](https://huggingface.co/datasets/davidscripka/openwakeword_features),
  CC BY-NC-SA 4.0, только некоммерческое использование.
- Spotify Soloist — проприетарный, распространяется Spotify отдельно.

## Лицензия

Код проекта — [MIT](LICENSE). Сторонние части — под своими лицензиями (см. выше).
