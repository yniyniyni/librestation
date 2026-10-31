package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"voice/internal/adb"
)

// Paths on the speaker; the device-side scripts use the same defaults.
const (
	soloistDir = adb.Tmp + "/soloist"
	voiceDir   = adb.Tmp + "/voice"
	rootfs     = soloistDir + "/rootfs"
	bootHook   = adb.Tmp + "/myscript-max.sh" // run as root at boot by the root kit
	startLine  = "sh " + soloistDir + "/start.sh"
)

type env struct {
	u    *ui
	d    *adb.Device
	host string // speaker IP, for the web panel
	repo string // repository root on this computer
}

// step is one thing a working speaker needs. check says whether it is in
// place; fix (nil = only by hand, see hint) puts it there.
type step struct {
	name     string
	optional bool // a missing optional step is not a failure
	check    func(*env) (ok bool, detail string, err error)
	fix      func(*env) error
	ask      string // question before fix; "" = fix without asking
	hint     string // what to do by hand when there is no fix or it failed
	// update, when set, is offered even when the step is in place.
	update string
	// canUpdate, when set, limits the update offer to speakers where it applies.
	canUpdate func(*env) bool
}

func steps() []step {
	return []step{
		{name: "Инструменты на компьютере", check: checkHost,
			hint: "нужны adb, go, javac, python3, Android SDK (ANDROID_SDK) и NDK (NDK или $ANDROID_SDK/ndk/*); см. README"},
		{name: "Root на колонке", check: checkRoot,
			hint: "нужен рут с командой mysudo; без него установка невозможна"},
		{name: "Свободное место на /data", check: checkSpace,
			hint: "освободите место на /data: rootfs занимает около 1,5 ГБ"},
		{name: "Программы проекта на колонке", check: checkDeployed, fix: fixDeploy,
			ask: "Собрать и залить voiced, ledd, aaudio_pipe и скрипты?", update: "Пересобрать и залить текущую версию?"},
		{name: "Debian rootfs с PulseAudio", check: checkRootfs, fix: fixRootfs,
			ask:  "Залить и распаковать rootfs (архив rootfs.tar.xz)?",
			hint: "нужен архив Debian arm64 с PulseAudio: на работающей колонке его делает ./setup export-rootfs, см. README"},
		{name: "Spotify Soloist", check: checkSoloist, fix: fixSoloist,
			ask:  "Положить бинарник Soloist в rootfs?",
			hint: "Soloist для linux/aarch64 получают у Spotify; в репозитории его нет"},
		{name: "Ключ Soloist", check: fileCheck(soloistDir + "/api-key"), fix: fixKey("Soloist API key", soloistDir+"/api-key"),
			ask: "Ввести ключ Soloist сейчас?"},
		{name: "Ключ Gemini (разметка срабатываний)", optional: true, check: fileCheck(voiceDir + "/gemini.key"),
			fix: fixKey("Gemini API key (https://aistudio.google.com/apikey)", voiceDir+"/gemini.key"),
			ask: "Ввести ключ Gemini? Без него срабатывания не проверяются"},
		{name: "Прокси для Spotify и Gemini", optional: true, check: checkProxy, fix: fixProxy,
			ask: "Выбрать, где работает прокси?", update: "Сменить, где работает прокси?"},
		{name: "Автозапуск", check: checkAutostart, fix: fixAutostart,
			ask:  "Добавить запуск в " + bootHook + "?",
			hint: "добавьте `" + startLine + "` в загрузочный скрипт вашего рута (от root)"},
		{name: "Яндекс: голосовой стек io.sdk удалён", check: checkIOSDK, fix: fixIOSDK,
			ask: "Удалить com.yandex.io.sdk? Он держит микрофон и рисует на кольце; обратимо: " + restoreIOSDK},
		{name: "Яндекс: лишние приложения удалены", optional: true, check: checkYandexApps, fix: fixYandexApps,
			ask: "Удалить их? Проверено на Станции Макс: звук, сеть и команды работают; обратимо: " + restoreApp},
		{name: "Старые файлы прежних версий", optional: true, check: checkLeftovers, fix: fixLeftovers,
			ask: "Удалить их?"},
		{name: "Кэш Soloist", check: checkCache, fix: fixCache,
			ask: "Очистить кэш? Soloist перезапустится, музыка прервётся"},
		{name: "Сервисы запущены", check: checkServices, fix: fixServices,
			ask: "Перезапустить сервисы?"},
		{name: "Веб-панель и микрофон", check: checkPanel,
			hint: "смотрите лог: adb shell -tt 'mysudo -n tail -50 " + soloistDir + "/soloist.log'"},
		{name: "Модель Gemini", optional: true, check: checkModel, fix: fixModel,
			ask: "Выбрать модель?", update: "Выбрать другую модель Gemini?"},
		{name: "Архив rootfs для другой колонки", optional: true, check: checkRootfsArchive, fix: fixRootfsArchive,
			canUpdate: canExportRootfs,
			update:    "Снять rootfs.tar.xz (без ключей и входа Soloist) для другой колонки? Музыка остановится на пару минут"},
	}
}

func checkHost(e *env) (bool, string, error) {
	var miss []string
	for _, c := range []string{"adb", "go", "javac", "python3"} {
		if _, err := exec.LookPath(c); err != nil {
			miss = append(miss, c)
		}
	}
	sdk := androidSDK()
	if sdk == "" {
		miss = append(miss, "Android SDK")
	} else if ndk := os.Getenv("NDK"); ndk == "" {
		if m, _ := filepath.Glob(filepath.Join(sdk, "ndk", "*")); len(m) == 0 {
			miss = append(miss, "Android NDK")
		}
	}
	if len(miss) > 0 {
		return false, "нет: " + strings.Join(miss, ", "), nil
	}
	return true, "SDK " + sdk, nil
}

func androidSDK() string {
	if s := os.Getenv("ANDROID_SDK"); s != "" {
		return s
	}
	if runtime.GOOS == "darwin" {
		if h, err := os.UserHomeDir(); err == nil {
			p := filepath.Join(h, "Library", "Android", "sdk")
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
	}
	return ""
}

func checkRoot(e *env) (bool, string, error) {
	model, _ := e.d.Shell("getprop ro.product.device")
	model = strings.TrimSpace(model)
	out, err := e.d.Root("id -u")
	if err != nil {
		return false, err.Error(), nil
	}
	if strings.TrimSpace(out) != "0" {
		return false, "mysudo не дал root: " + strings.TrimSpace(out), nil
	}
	detail := "модель " + model
	if model != "yandexstation_2" {
		detail += " — проект сделан для Станции Макс (yandexstation_2), остальное не проверялось"
	}
	return true, detail, nil
}

func checkSpace(e *env) (bool, string, error) {
	out, err := e.d.Shell("df -k /data | tail -1")
	if err != nil {
		return false, "", err
	}
	f := strings.Fields(out)
	if len(f) < 4 {
		return false, "", fmt.Errorf("df: %q", out)
	}
	var free int64
	fmt.Sscan(f[3], &free)
	mb := free / 1024
	// The rootfs takes about 1.5 GB; once it is in, logs, hits and models
	// need little.
	need := int64(100)
	if ok, _ := e.d.RootOK("test -d " + rootfs + "/usr"); !ok {
		need = 1700
	}
	if mb < need {
		return false, fmt.Sprintf("свободно %d МБ, нужно хотя бы %d", mb, need), nil
	}
	return true, fmt.Sprintf("свободно %d МБ", mb), nil
}

func checkDeployed(e *env) (bool, string, error) {
	ok, err := e.d.RootOK(strings.Join([]string{
		"test -x " + voiceDir + "/voiced", "test -x " + voiceDir + "/ledd", "test -s " + voiceDir + "/mic.dex",
		"test -x " + soloistDir + "/aaudio_pipe", "test -s " + soloistDir + "/soloist-service.sh",
		"test -s " + soloistDir + "/setup-rootfs.sh"}, "\n"))
	return ok, "", err
}

func fixDeploy(e *env) error {
	cmd := exec.Command("sh", filepath.Join(e.repo, "voice", "deploy.sh"))
	cmd.Env = append(os.Environ(), "ADB_DEVICE="+e.d.Serial)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

func checkRootfs(e *env) (bool, string, error) {
	ok, err := e.d.RootOK("test -x " + rootfs + "/usr/bin/pulseaudio\ntest -x " + rootfs + "/bin/sh")
	return ok, "", err
}

func fixRootfs(e *env) error {
	def := ""
	if _, err := os.Stat(filepath.Join(e.repo, "rootfs.tar.xz")); err == nil {
		def = filepath.Join(e.repo, "rootfs.tar.xz")
	}
	p := hostPath(e.u.ask("Путь к rootfs.tar.xz на этом компьютере", def))
	if p == "" {
		return fmt.Errorf("архив не указан")
	}
	if _, err := os.Stat(p); err != nil {
		return err
	}
	e.u.say("Заливаю архив, это может занять несколько минут…")
	// adb push runs as the shell user and soloistDir is root's, so stage it.
	tmp := adb.Tmp + "/.librestation-rootfs.tar.xz"
	if err := e.d.Push(p, tmp); err != nil {
		return err
	}
	out, err := e.d.Root(fmt.Sprintf("mkdir -p %s\nmv -f %s %s/rootfs.tar.xz\nsh %s/setup-rootfs.sh", soloistDir, tmp, soloistDir, soloistDir))
	e.u.say("%s", strings.TrimSpace(out))
	return err
}

func checkSoloist(e *env) (bool, string, error) {
	ok, err := e.d.RootOK("test -x " + rootfs + "/opt/soloist/soloist")
	return ok, "", err
}

func fixSoloist(e *env) error {
	p := hostPath(e.u.ask("Путь к бинарнику soloist (linux/aarch64)", ""))
	if p == "" {
		return fmt.Errorf("путь не указан")
	}
	tmp := adb.Tmp + "/.librestation-soloist"
	if err := e.d.Push(p, tmp); err != nil {
		return err
	}
	_, err := e.d.Root(fmt.Sprintf("mkdir -p %s/opt/soloist\nmv -f %s %s/opt/soloist/soloist\nchmod 755 %s/opt/soloist/soloist", rootfs, tmp, rootfs, rootfs))
	return err
}

func fileCheck(path string) func(*env) (bool, string, error) {
	return func(e *env) (bool, string, error) {
		ok, err := e.d.RootOK("test -s " + path)
		return ok, "", err
	}
}

func fixKey(prompt, path string) func(*env) error {
	return func(e *env) error {
		k := strings.TrimSpace(e.u.secret(prompt + " (ввод скрыт)"))
		if k == "" {
			return fmt.Errorf("пустой ключ, ничего не записано")
		}
		if err := e.d.WriteSecret(path, []byte(k)); err != nil {
			return err
		}
		// Keys are read at start; on a fresh speaker nothing runs yet.
		if up, _, _ := checkServices(e); up {
			e.u.say("  Перезапускаю сервисы, чтобы подхватить ключ…")
			return fixServices(e)
		}
		return nil
	}
}

func checkAutostart(e *env) (bool, string, error) {
	ok, err := e.d.RootOK("grep -q '" + soloistDir + "/start.sh' " + bootHook)
	if err == nil && !ok {
		if exists, _ := e.d.RootOK("test -f " + bootHook); !exists {
			return false, bootHook + " нет — автозапуск придётся добавить в ваш загрузочный скрипт", nil
		}
	}
	return ok, bootHook, err
}

func fixAutostart(e *env) error {
	if exists, err := e.d.RootOK("test -f " + bootHook); err != nil || !exists {
		return fmt.Errorf("%s нет", bootHook)
	}
	_, err := e.d.Root(fmt.Sprintf("printf '\\n# librestation: Spotify Connect, voice commands, LEDs\\n%s\\n' >> %s", startLine, bootHook))
	return err
}

const restoreIOSDK = "adb shell -tt 'mysudo -n cmd package install-existing com.yandex.io.sdk'"

func checkIOSDK(e *env) (bool, string, error) {
	out, err := e.d.Shell("pm list packages com.yandex.io.sdk")
	if err != nil {
		return false, "", err
	}
	return !strings.Contains(out, "package:com.yandex.io.sdk"), "", nil
}

func fixIOSDK(e *env) error {
	// ledd draws with io.sdk's stock frames; they go with its data, so copy
	// them first (deploy does it while io.sdk is there).
	have, err := e.d.RootOK("test -d " + voiceDir + "/assets/25x16")
	if err != nil {
		return err
	}
	if !have {
		if _, err := e.d.Root(fmt.Sprintf("mkdir -p %[1]s/assets\ncp -r /data/data/com.yandex.io.sdk/files/system/frontal_animation/25x16 /data/data/com.yandex.io.sdk/files/system/frontal_animation/fonts %[1]s/assets/\nchmod -R a+rX %[1]s/assets", voiceDir)); err != nil {
			return fmt.Errorf("копия анимаций экрана: %w", err)
		}
	}
	_, err = e.d.Root("pm uninstall -k --user 0 com.yandex.io.sdk")
	return err
}

// cacheLimitMB matches soloist-service.sh, which clears a bigger cache at
// start; Soloist itself does not keep to its --cache-size.
const cacheLimitMB = 400

func checkCache(e *env) (bool, string, error) {
	out, err := e.d.Root("du -sm " + rootfs + "/var/cache/soloist 2>/dev/null | cut -f1 || echo 0")
	if err != nil {
		return false, "", err
	}
	var mb int
	fmt.Sscan(strings.TrimSpace(out), &mb)
	return mb <= cacheLimitMB, fmt.Sprintf("%d МБ", mb), nil
}

func fixCache(e *env) error {
	_, err := e.d.Root("sh " + soloistDir + "/stop.sh >/dev/null\nrm -rf " + rootfs + "/var/cache/soloist/cache\nsh " + soloistDir + "/start.sh")
	return err
}

// yandexApps are Yandex apps a speaker running this project does not use,
// each removed on a Station Max with sound, network, commands and LEDs still
// working. Removal is for user 0 with data kept, so it can be undone. The
// platform services, launcher, settings and setup wizard stay: the system
// leans on them.
var yandexApps = []string{
	"com.yandex.android.advid",          // advertising id
	"com.yandex.launcher.updaterapp",    // app updates
	"com.yandex.tv.bugreportsender",     // bug reports to Yandex
	"com.yandex.tv.daydream",            // screensaver
	"com.yandex.tv.alice",               // Alice
	"com.yandex.tv.music",               // Yandex Music
	"com.yandex.tv.live",                // TV channels
	"com.yandex.tv.input.efir",          // TV input
	"com.yandex.tv.videoplayer",         // video
	"com.yandex.tv.webplayer",           // web player
	"com.yandex.tv.ytplayer",            // YouTube player
	"com.yandex.tv.keyboard",            // on-screen keyboard
	"ru.yandex.androidkeyboard.tv",      // on-screen keyboard
	"com.yandex.tv.remotechecker",       // remote control check
	"com.yandex.devices.payment",        // payments
	"com.yandex.devices.appstore",       // app store
	"com.yandex.devices.farm.companion", // device farm
	"com.yandex.devices.player",         // media player
}

const restoreApp = "adb shell -tt 'mysudo -n cmd package install-existing <пакет>'"

func installedYandexApps(e *env) ([]string, error) {
	out, err := e.d.Shell("pm list packages")
	if err != nil {
		return nil, err
	}
	var left []string
	for _, p := range yandexApps {
		if strings.Contains(out, "package:"+p+"\n") {
			left = append(left, p)
		}
	}
	return left, nil
}

func checkYandexApps(e *env) (bool, string, error) {
	left, err := installedYandexApps(e)
	if err != nil {
		return false, "", err
	}
	if len(left) > 0 {
		return false, fmt.Sprintf("установлено %d из %d", len(left), len(yandexApps)), nil
	}
	return true, fmt.Sprintf("%d удалено", len(yandexApps)), nil
}

func fixYandexApps(e *env) error {
	left, err := installedYandexApps(e)
	if err != nil {
		return err
	}
	var script []string
	for _, p := range left {
		script = append(script, "pm uninstall -k --user 0 "+p)
	}
	_, err = e.d.Root(strings.Join(script, "\n"))
	return err
}

// leftovers are device files of earlier versions that nothing uses now.
var leftovers = []string{
	soloistDir + "/alice-button.sh", soloistDir + "/mute-errors.sh", soloistDir + "/unmute-errors.sh",
	adb.Tmp + "/volsample.log", adb.Tmp + "/ledd-assets.sh",
}

func checkLeftovers(e *env) (bool, string, error) {
	var found []string
	for _, f := range leftovers {
		if ok, err := e.d.RootOK("test -e " + f); err != nil {
			return false, "", err
		} else if ok {
			found = append(found, filepath.Base(f))
		}
	}
	if len(found) > 0 {
		return false, strings.Join(found, ", "), nil
	}
	return true, "", nil
}

func fixLeftovers(e *env) error {
	_, err := e.d.Root("rm -f " + strings.Join(leftovers, " "))
	return err
}

// services are the device processes a working speaker runs: a name for
// people and the check, by exact process name (toybox ps cuts ARGS to base
// names). Nothing from ps is shown: Soloist's command line has its API key.
var services = []struct{ name, check string }{
	{"супервизор", "ps -A -o ARGS | grep -v grep | grep -q soloist-service.sh"},
	{"PulseAudio", "ps -A -o NAME | grep -qx pulseaudio"},
	{"aaudio_pipe", "ps -A -o NAME | grep -qx aaudio_pipe"},
	{"Soloist", "ps -A -o NAME | grep -qx soloist"},
	{"voiced", "ps -A -o NAME | grep -qx voiced"},
	{"ledd", "ps -A -o NAME | grep -qx ledd"},
}

func checkServices(e *env) (bool, string, error) {
	var down []string
	for _, s := range services {
		ok, err := e.d.RootOK(s.check)
		if err != nil {
			return false, "", err
		}
		if !ok {
			down = append(down, s.name)
		}
	}
	if len(down) > 0 {
		return false, "не работают: " + strings.Join(down, ", "), nil
	}
	return true, "", nil
}

func fixServices(e *env) error {
	if _, err := e.d.Root("sh " + soloistDir + "/stop.sh >/dev/null\nsh " + soloistDir + "/start.sh"); err != nil {
		return err
	}
	e.u.say("Жду запуска (до минуты)…")
	for range 20 {
		time.Sleep(3 * time.Second)
		if ok, _, _ := checkServices(e); ok {
			return nil
		}
	}
	return nil // checked again by the caller
}

// panelState reads the web panel's state into v.
func panelState(e *env, v any) error {
	c := http.Client{Timeout: 5 * time.Second}
	r, err := c.Get("http://" + e.host + ":8780/api/state")
	if err != nil {
		return err
	}
	defer r.Body.Close()
	return json.NewDecoder(r.Body).Decode(v)
}

type judgeState struct {
	Judge struct {
		Available bool   `json:"available"`
		Model     string `json:"model"`
		Models    []struct {
			ID   string `json:"id"`
			Note string `json:"note"`
		} `json:"models"`
	} `json:"judge"`
}

func checkModel(e *env) (bool, string, error) {
	var s judgeState
	if err := panelState(e, &s); err != nil {
		return false, "панель не отвечает", nil
	}
	if !s.Judge.Available {
		return false, "нет ключа Gemini", nil
	}
	return true, s.Judge.Model, nil
}

func fixModel(e *env) error {
	var s judgeState
	if err := panelState(e, &s); err != nil {
		return err
	}
	if !s.Judge.Available {
		return fmt.Errorf("сначала ключ Gemini")
	}
	def := ""
	for i, m := range s.Judge.Models {
		cur := ""
		if m.ID == s.Judge.Model {
			cur, def = " (сейчас)", strconv.Itoa(i+1)
		}
		e.u.say("    %d — %s: %s%s", i+1, m.ID, m.Note, cur)
	}
	ans := e.u.ask("  Номер или имя модели", def)
	model := ans
	if n, err := strconv.Atoi(ans); err == nil {
		if n < 1 || n > len(s.Judge.Models) {
			return fmt.Errorf("нет модели с номером %d", n)
		}
		model = s.Judge.Models[n-1].ID
	}
	body, _ := json.Marshal(map[string]string{"judge_model": model})
	c := http.Client{Timeout: 5 * time.Second}
	r, err := c.Post("http://"+e.host+":8780/api/settings", "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(r.Body)
		return fmt.Errorf("панель: %s %s", r.Status, strings.TrimSpace(string(b)))
	}
	return nil
}

func checkPanel(e *env) (bool, string, error) {
	c := http.Client{Timeout: 5 * time.Second}
	var r *http.Response
	var err error
	for try := range 3 { // Wi-Fi on the speaker drops single requests now and then
		if try > 0 {
			time.Sleep(2 * time.Second)
		}
		if r, err = c.Get("http://" + e.host + ":8780/api/state"); err == nil {
			break
		}
	}
	if err != nil {
		return false, "панель не отвечает: " + err.Error(), nil
	}
	defer r.Body.Close()
	var s struct {
		Audio struct {
			OK    bool    `json:"ok"`
			Error string  `json:"error"`
			Level float64 `json:"level"`
		} `json:"audio"`
		Words []struct{} `json:"words"`
	}
	if err := json.NewDecoder(r.Body).Decode(&s); err != nil {
		return false, "", err
	}
	if !s.Audio.OK {
		return false, "микрофон: " + s.Audio.Error, nil
	}
	return true, fmt.Sprintf("http://%s:8780 · микрофон %.0f dBFS · слов %d", e.host, s.Audio.Level, len(s.Words)), nil
}
