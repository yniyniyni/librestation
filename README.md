# spoti-alice

Яндекс Станция Макс без Яндекса: Spotify Connect, локальные голосовые команды и
своя индикация на кольце и экране. Всё работает прямо на колонке, без облака.

*Yandex Station Max (rooted, Android 9) turned into a local Spotify Connect speaker
with on-device voice commands and its own LED ring/screen UI. Docs are in Russian.*

> Неофициальный проект. Не связан с Яндексом или Spotify. Нужна рутованная
> колонка; всё, что здесь описано, вы делаете на свой страх и риск.

## Важно: поддерживаемый способ root

Колонка **изначально должна быть рутована** и иметь:

- ADB по сети;
- команду `mysudo`, через которую доступен root-shell;
- рабочий вызов `mysudo su shell -c '...'`.

Все команды, которые выполняются на колонке с правами root, в этом проекте
проходят через `mysudo su shell`. Если колонка рутована другим способом,
пользователь должен самостоятельно адаптировать команды и установочные скрипты.
Поддержка других root-обвязок этим README не обещается.

## Что внутри

| Часть | Где | Что делает |
|---|---|---|
| Spotify Connect | `soloist/` | Официальный Spotify Soloist в Debian-chroot → PulseAudio (float) → FIFO → `aaudio_pipe` → audioserver. Сервис поднимает и перезапускает всё сам. |
| `aaudio_pipe` | `aaudio-pipe/` | Мост FIFO → AAudio (NDK, C). Компенсирует программную громкость Soloist, чтобы громкостью управлял усилитель. |
| `voiced` | `voice/cmd/voiced` | Локальные голосовые команды, обучение на голосе пользователя, эхоподавление, единая громкость и веб-панель. |
| `ledd` | `voice/cmd/ledd` | Часы, спектр музыки, громкость и реакция на команды для кольца и экрана. |

Дизайн и решения — в `docs/`: голосовые команды
([docs/2026-10-29-voice-commands-design.md](docs/2026-10-29-voice-commands-design.md)),
кольцо и экран ([docs/2026-10-29-led-ui-design.md](docs/2026-10-29-led-ui-design.md)),
замеры железа ([docs/led-hw-notes.md](docs/led-hw-notes.md)).

## Что нужно

### Колонка

- Яндекс Станция Макс (`yandexstation_2`) на Android 9.
- Root через `mysudo su shell` и ADB по сети.
- Свободное место под Debian arm64 rootfs, модели и логи.
- Подготовленный Debian rootfs для `linux/aarch64` с PulseAudio.
- Сборка Spotify Soloist для `linux/aarch64` и **свой** developer API key Soloist.
  Soloist и ключ в репозитории не распространяются.
- `xray` внутри rootfs — только если сеть требует прокси; конфигурация
  `xray.json` загружается отдельно и в репозитории отсутствует.

### Компьютер для сборки

Поддерживаются macOS и Linux:

- Go 1.24 или совместимый Go, умеющий собирать `linux/arm64`;
- Java `javac`;
- Android SDK с платформой `android-28` и `d8` из build-tools;
- Android NDK с `aarch64-linux-android28-clang`;
- `adb`, `python3`, `xz`/обычные Unix-утилиты.

По умолчанию на macOS скрипты ищут SDK в `$HOME/Library/Android/sdk`.
На Linux задайте `ANDROID_SDK` явно. Пути можно переопределить переменными
окружения; сами скрипты больше не требуют фиксированного расположения NDK.

## Полная установка с нуля

Ниже описан стандартный путь для root-обвязки `mysudo su shell`.
Начинайте из корня репозитория.

### 1. Подключить ADB и проверить root

```sh
export ADB_DEVICE=<ip-колонки>:5555
adb connect "$ADB_DEVICE"
adb -s "$ADB_DEVICE" shell 'mysudo su shell -c "id"'
```

В выводе должен быть root (`uid=0`). Если `adb connect` или `mysudo su shell`
не работают, сначала исправьте root/ADB на самой колонке.

### 2. Подготовить host-инструменты

На macOS установите Android SDK/NDK и Java любым удобным способом. Если SDK
находится не в стандартном macOS-пути:

```sh
export ANDROID_SDK=/path/to/Android/sdk
export NDK="$ANDROID_SDK/ndk/<версия>"
```

На Linux задайте оба пути явно, например:

```sh
export ANDROID_SDK=$HOME/Android/Sdk
export NDK="$ANDROID_SDK/ndk/<версия>"
```

Если версия build-tools отличается от `34.0.0`, задайте:

```sh
export ANDROID_BUILD_TOOLS_VERSION=<версия>
```

Если Android platform отличается от `android-28`, это можно переопределить через
`ANDROID_PLATFORM`, но совместимость с Android 9 отдельно не гарантируется.

### 3. Собрать и загрузить проект

```sh
ADB_DEVICE="$ADB_DEVICE" sh voice/deploy.sh
```

Скрипт собирает `voiced`, `ledd`, `mic.dex`, `aaudio_pipe`, при необходимости
скачивает набор негативов для обучения и загружает бинарники и device-side
скрипты в:

```text
/data/local/tmp/voice
/data/local/tmp/soloist
```

Эти каталоги можно изменить:

```sh
export REMOTE_BASE=/data/local/tmp
export REMOTE_VOICE_DIR="$REMOTE_BASE/voice"
export REMOTE_SOLOIST_DIR="$REMOTE_BASE/soloist"
```

Для нестандартных путей нужно передавать те же `SOLOIST_BASE` и `VOICE_DIR`
при запуске device-side скриптов.

### 4. Загрузить и распаковать Debian rootfs

Архив rootfs не входит в репозиторий. После `deploy.sh` загрузите его:

```sh
adb -s "$ADB_DEVICE" push /path/to/rootfs.tar.xz \
  /data/local/tmp/soloist/rootfs.tar.xz
adb -s "$ADB_DEVICE" shell -tt \
  'mysudo su shell -c "sh /data/local/tmp/soloist/setup-rootfs.sh"'
```

`setup-rootfs.sh` остановит старый librespot-пайплайн, распакует архив и удалит
архив после успешной распаковки.

Для нестандартного пути:

```sh
adb -s "$ADB_DEVICE" shell -tt \
  'mysudo su shell -c "SOLOIST_BASE=/custom/soloist sh /custom/soloist/setup-rootfs.sh"'
```

### 5. Установить Soloist в rootfs

Получите бинарник Soloist отдельно у его поставщика, затем положите его в
rootfs:

```sh
adb -s "$ADB_DEVICE" push /path/to/soloist \
  /data/local/tmp/soloist/rootfs/opt/soloist/soloist
adb -s "$ADB_DEVICE" shell -tt \
  'mysudo su shell -c "chmod 755 /data/local/tmp/soloist/rootfs/opt/soloist/soloist"'
```

Если используется Xray, отдельно положите бинарник в
`/data/local/tmp/soloist/rootfs/opt/xray/xray` и конфигурацию на колонку:

```sh
adb -s "$ADB_DEVICE" push /path/to/xray \
  /data/local/tmp/soloist/rootfs/opt/xray/xray
adb -s "$ADB_DEVICE" push /path/to/xray.json \
  /data/local/tmp/soloist/xray.json
adb -s "$ADB_DEVICE" shell -tt \
  'mysudo su shell -c "chmod 755 /data/local/tmp/soloist/rootfs/opt/xray/xray && chmod 600 /data/local/tmp/soloist/xray.json"'
```

`xray.json` содержит сетевую конфигурацию и может содержать секреты. Не
коммитьте его в репозиторий.

### 6. Сохранить Soloist API key

Ключ вводится скрыто и передаётся на колонку через stdin. Он не записывается в
репозиторий:

```sh
ADB_DEVICE="$ADB_DEVICE" sh soloist/put-key.zsh
```

Скрипт сразу создаёт файл с правами `600`. Если используется нестандартный
каталог:

```sh
REMOTE_SOLOIST_DIR=/custom/soloist \
ADB_DEVICE="$ADB_DEVICE" sh soloist/put-key.zsh
```

### 7. Запустить

```sh
adb -s "$ADB_DEVICE" shell -tt \
  'mysudo su shell -c "sh /data/local/tmp/soloist/start.sh"'
```

Проверить лог:

```sh
adb -s "$ADB_DEVICE" shell -tt \
  'mysudo su shell -c "tail -f /data/local/tmp/soloist/soloist.log"'
```

Остановить всё:

```sh
adb -s "$ADB_DEVICE" shell -tt \
  'mysudo su shell -c "sh /data/local/tmp/soloist/stop.sh"'
```

Для автозапуска добавьте вызов `start.sh` в загрузочный скрипт именно вашего
root-решения, также через `mysudo su shell`. Процесс автозапуска зависит от
способа root и в проекте намеренно не привязан к конкретному менеджеру.

## После запуска

Веб-панель доступна по адресу:

```text
http://<ip-колонки>:8780
```

Через неё можно записать и обучить слова команд, настроить громкость, шаг
кольца, стили индикации и яркость экрана.

> В панели сейчас нет аутентификации. Порт `8780` слушает на всех интерфейсах,
> поэтому не выставляйте его в недоверенную сеть и ограничьте доступ firewall'ом
> или локальной сетью.

Если голосовой стек Яндекса мешает, его можно удалить обратимо:

```sh
adb -s "$ADB_DEVICE" shell -tt \
  'mysudo su shell -c "pm uninstall -k --user 0 com.yandex.io.sdk"'
```

Вернуть компонент:

```sh
adb -s "$ADB_DEVICE" shell -tt \
  'mysudo su shell -c "cmd package install-existing com.yandex.io.sdk"'
```

Стоковые анимации экрана Яндекса в репозиторий не входят. `deploy.sh` пытается
скопировать их из данных `io.sdk`; отсутствие этих файлов не должно ломать
остановку и запуск, но часть анимаций будет недоступна.

## Если установка не проходит

- `missing host command` — установите указанную host-зависимость.
- `ANDROID_SDK is required` — задайте `ANDROID_SDK`; на macOS проверьте
  `$HOME/Library/Android/sdk`.
- `missing d8` или `missing Android platform` — установите соответствующие
  Android SDK platform/build-tools.
- `NDK is required` или `aarch64-linux-android28-clang not found` — задайте
  корректный `NDK`.
- `no API key ... not starting` — повторите шаг с `put-key.zsh`.
- `soloist ... not found` — проверьте путь к бинарнику внутри rootfs.
- Нет звука — смотрите `soloist.log`, проверьте PulseAudio rootfs и наличие
  `/dev`/`/dev/pts`, которые монтирует `chroot.sh`.
- Нет веб-панели — проверьте, что `voiced` запущен, и что порт `8780` доступен
  с компьютера.

## Переменные установки

| Переменная | Назначение | Значение по умолчанию |
|---|---|---|
| `ADB_DEVICE` | ADB-адрес колонки | нет, обязательна |
| `ANDROID_SDK` | Android SDK на host | `$HOME/Library/Android/sdk` на macOS |
| `NDK` | Android NDK на host | последняя найденная версия внутри `ANDROID_SDK/ndk` |
| `ANDROID_PLATFORM` | Android SDK platform | `android-28` |
| `ANDROID_BUILD_TOOLS_VERSION` | Версия build-tools/d8 | `34.0.0` |
| `NEG_PYTHON` | Python для negatives dataset | `python3` |
| `REMOTE_BASE` | Базовый каталог на колонке | `/data/local/tmp` |
| `REMOTE_VOICE_DIR` | Бинарники/данные voice | `$REMOTE_BASE/voice` |
| `REMOTE_SOLOIST_DIR` | Сервис/ключ/rootfs | `$REMOTE_BASE/soloist` |
| `SOLOIST_BASE` | База device-side runtime | `/data/local/tmp/soloist` |
| `VOICE_DIR` | База device-side voice | `/data/local/tmp/voice` |
| `XRAY_BIN` | Путь xray внутри rootfs | `/opt/xray/xray` |

## Идеи для дальнейшего улучшения установщика

Сейчас скрипты уже разделяют host-пути и device-пути, умеют работать с
`ANDROID_SDK`/`NDK` на macOS и Linux и используют единый root-вызов
`mysudo su shell`. Дальше установку можно сделать ещё удобнее:

- добавить `tools/install.sh`, который проверяет ОС, версии Go/Java/SDK/NDK,
  подключение ADB и root-доступ до начала сборки;
- добавить интерактивный режим с вопросами про `ADB_DEVICE`, rootfs, Soloist и
  Xray, а также non-interactive режим для CI через `.env.example` или аргументы;
- автоматически находить `adb`, `javac`, `d8` и NDK в стандартных путях macOS/Linux,
  оставляя явные переменные окружения приоритетными;
- сделать отдельный `tools/push-rootfs.sh`, который загрузит архив, распакует его
  через `mysudo su shell` и проверит наличие `opt/soloist/soloist`;
- добавить проверку архитектуры и контрольные суммы для rootfs, Soloist и Xray;
- добавить безопасный `xray.json.example` без секретов и отдельную команду для
  загрузки настоящей конфигурации;
- для macOS предложить launchd-инструкцию только для host-части, а для Linux —
  systemd user/service-инструкцию; автозапуск на колонке всё равно должен
  подключаться к конкретному root-решению пользователя;
- добавить smoke-test после deploy: наличие файлов, права, доступность `8780`,
  запуск/остановка и понятный вывод последних строк `soloist.log`;
- в будущем ограничить веб-панель `voiced` на localhost или добавить пароль/токен,
  чтобы установка не открывала управление колонкой всей локальной сети.

## Благодарности и лицензии сторонних частей

- Распознавание слов — порт фронтенда и speech embedding из
  [openWakeWord](https://github.com/dscripka/openWakeWord) на Go. Веса speech
  embedding (`voice/internal/kws/assets/oww.bin`) — Google, Apache 2.0.
- Набор негативов для обучения (`voice/tools/build_negatives.py` скачивает его в
  `dist/`, в репозитории его нет) —
  [openwakeword_features](https://huggingface.co/datasets/davidscripka/openwakeword_features),
  CC BY-NC-SA 4.0, только некоммерческое использование.
- Spotify Soloist — проприетарный, распространяется Spotify отдельно.

## Лицензия

Код проекта — [MIT](LICENSE). Сторонние части — под своими лицензиями (см. выше).
