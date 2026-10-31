package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Spotify (and Gemini) may need a proxy. It runs either on the router, where
// the speaker needs nothing, or as Xray inside the speaker's rootfs: the
// service starts it when soloist/xray.json exists, with socks on
// 127.0.0.1:1080 and http on 127.0.0.1:1081 for Soloist and voiced.

const (
	xrayDir    = rootfs + "/opt/xray"
	xrayConfig = soloistDir + "/xray.json"
	proxyMode  = soloistDir + "/proxy-mode" // "router" or "speaker": the choice made in setup
	xrayRepo   = "XTLS/Xray-core"
	xrayAsset  = "Xray-linux-arm64-v8a.zip"
)

func checkProxy(e *env) (bool, string, error) {
	mode, _ := e.d.Root("cat " + proxyMode + " 2>/dev/null || true")
	mode = strings.TrimSpace(mode)
	hasConfig, err := e.d.RootOK("test -s " + xrayConfig)
	if err != nil {
		return false, "", err
	}
	if !hasConfig {
		if mode == "router" {
			return true, "на роутере (или не нужен)", nil
		}
		return false, "не выбрано, где работает прокси", nil
	}
	bin, err := e.d.RootOK("test -x " + xrayDir + "/xray")
	if err != nil || !bin {
		return false, "есть xray.json, но нет " + xrayDir + "/xray", err
	}
	running, err := e.d.RootOK("ps -A -o NAME | grep -qx xray")
	if err != nil {
		return false, "", err
	}
	if !running {
		return false, "xray на колонке не запущен", nil
	}
	ver, _ := e.d.Root(xrayDir + "/xray version | head -1")
	return true, "xray на колонке · " + strings.TrimSpace(ver), nil
}

func fixProxy(e *env) error {
	e.u.say("  Где работает прокси для Spotify и Gemini?")
	e.u.say("    1 — на роутере, или прокси не нужен (на колонке ничего не ставится)")
	e.u.say("    2 — Xray прямо на колонке (скачать последний Xray-core и настроить)")
	switch e.u.ask("  Выбор", "1") {
	case "1":
		if on, _ := e.d.RootOK("test -s " + xrayConfig); on {
			if !e.u.yes("  На колонке настроен xray. Выключить его?", true) {
				return fmt.Errorf("оставлен xray на колонке")
			}
			if _, err := e.d.Root("mv -f " + xrayConfig + " " + xrayConfig + ".off"); err != nil {
				return err
			}
			if err := restartServices(e); err != nil {
				return err
			}
		}
		_, err := e.d.Root("echo router > " + proxyMode)
		return err
	case "2":
		return setupXray(e)
	}
	return fmt.Errorf("нужно 1 или 2")
}

func setupXray(e *env) error {
	have, err := e.d.RootOK("test -x " + xrayDir + "/xray")
	if err != nil {
		return err
	}
	if !have || e.u.yes("  Xray уже стоит. Обновить до последней версии?", false) {
		if err := installXray(e); err != nil {
			return err
		}
	}
	if ok, _ := e.d.RootOK("test -s " + xrayConfig); !ok || e.u.yes("  Заменить настройки прокси?", false) {
		cfg, err := askXrayConfig(e)
		if err != nil {
			return err
		}
		if err := e.d.WriteSecret(xrayConfig, cfg); err != nil {
			return err
		}
	}
	if _, err := e.d.Root("echo speaker > " + proxyMode); err != nil {
		return err
	}
	e.u.say("  Перезапускаю сервисы, чтобы поднять xray (музыка прервётся)…")
	return restartServices(e)
}

func installXray(e *env) error {
	tag, err := latestTag(xrayRepo)
	if err != nil {
		return err
	}
	e.u.say("  Скачиваю Xray-core %s…", tag)
	files, err := fetchXray(tag)
	if err != nil {
		return err
	}
	tmp, err := os.MkdirTemp("", "xray")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	var mv []string
	for name, data := range files {
		local := filepath.Join(tmp, name)
		if err := os.WriteFile(local, data, 0o644); err != nil {
			return err
		}
		remote := "/data/local/tmp/.librestation-" + name
		if err := e.d.Push(local, remote); err != nil {
			return err
		}
		mv = append(mv, fmt.Sprintf("mv -f %s %s/%s", remote, xrayDir, name))
	}
	_, err = e.d.Root("mkdir -p " + xrayDir + "\n" + strings.Join(mv, "\n") + "\nchmod 755 " + xrayDir + "/xray")
	return err
}

var httpClient = &http.Client{Timeout: 2 * time.Minute}

// latestTag is the newest release of a GitHub repository.
func latestTag(repo string) (string, error) {
	r, err := httpClient.Get("https://api.github.com/repos/" + repo + "/releases/latest")
	if err != nil {
		return "", err
	}
	defer r.Body.Close()
	var rel struct {
		Tag string `json:"tag_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&rel); err != nil || rel.Tag == "" {
		return "", fmt.Errorf("последний релиз %s: %s %v", repo, r.Status, err)
	}
	return rel.Tag, nil
}

// fetchXray downloads the arm64 build of tag, checks it against the
// release's SHA2-256 digest and returns xray and its geo data files.
func fetchXray(tag string) (map[string][]byte, error) {
	base := "https://github.com/" + xrayRepo + "/releases/download/" + tag + "/" + xrayAsset
	zipData, err := get(base)
	if err != nil {
		return nil, err
	}
	dgst, err := get(base + ".dgst")
	if err != nil {
		return nil, err
	}
	if err := checkDigest(zipData, string(dgst)); err != nil {
		return nil, err
	}
	return unzipXray(zipData)
}

func get(u string) ([]byte, error) {
	r, err := httpClient.Get(u)
	if err != nil {
		return nil, err
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", u, r.Status)
	}
	return io.ReadAll(io.LimitReader(r.Body, 100<<20))
}

// checkDigest compares data with the "SHA2-256= <hex>" line of an Xray
// release .dgst file.
func checkDigest(data []byte, dgst string) error {
	for _, l := range strings.Split(dgst, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(l), "SHA2-256="); ok {
			sum := sha256.Sum256(data)
			if strings.EqualFold(strings.TrimSpace(v), hex.EncodeToString(sum[:])) {
				return nil
			}
			return fmt.Errorf("контрольная сумма Xray не совпала — архив повреждён или подменён")
		}
	}
	return fmt.Errorf("в .dgst нет SHA2-256")
}

func unzipXray(data []byte) (map[string][]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, err
	}
	out := map[string][]byte{}
	for _, f := range zr.File {
		switch f.Name {
		case "xray", "geoip.dat", "geosite.dat":
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			b, err := io.ReadAll(io.LimitReader(rc, 100<<20))
			rc.Close()
			if err != nil {
				return nil, err
			}
			out[f.Name] = b
		}
	}
	if out["xray"] == nil {
		return nil, fmt.Errorf("в архиве нет xray")
	}
	return out, nil
}

func askXrayConfig(e *env) ([]byte, error) {
	link := strings.TrimSpace(e.u.secret("  VLESS-ссылка (vless://…, ввод скрыт) или Enter, чтобы указать готовый xray.json"))
	if link != "" {
		return vlessConfig(link)
	}
	p := hostPath(e.u.ask("  Путь к xray.json на этом компьютере", ""))
	if p == "" {
		return nil, fmt.Errorf("настройки прокси не заданы")
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var cfg map[string]any
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, fmt.Errorf("%s: не JSON: %v", p, err)
	}
	if s := string(b); !strings.Contains(s, "1080") || !strings.Contains(s, "1081") {
		e.u.say("  Внимание: Soloist ждёт socks на 127.0.0.1:1080 и http на 127.0.0.1:1081, а в этом файле их не видно.")
	}
	return b, nil
}

// vlessConfig turns a vless:// share link into an Xray config with the
// inbounds the service expects. Private addresses go direct.
func vlessConfig(link string) ([]byte, error) {
	u, err := url.Parse(link)
	if err != nil || u.Scheme != "vless" || u.User == nil || u.Hostname() == "" {
		return nil, fmt.Errorf("это не vless://-ссылка")
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port <= 0 {
		return nil, fmt.Errorf("в ссылке нет порта")
	}
	q := u.Query()
	network := firstOf(q.Get("type"), "tcp")
	security := firstOf(q.Get("security"), "none")
	stream := map[string]any{"network": network, "security": security}
	switch security {
	case "tls":
		tls := map[string]any{"serverName": firstOf(q.Get("sni"), u.Hostname()), "fingerprint": firstOf(q.Get("fp"), "chrome")}
		if a := q.Get("alpn"); a != "" {
			tls["alpn"] = strings.Split(a, ",")
		}
		stream["tlsSettings"] = tls
	case "reality":
		if q.Get("pbk") == "" {
			return nil, fmt.Errorf("reality без pbk")
		}
		stream["realitySettings"] = map[string]any{
			"serverName": q.Get("sni"), "fingerprint": firstOf(q.Get("fp"), "chrome"),
			"publicKey": q.Get("pbk"), "shortId": q.Get("sid"), "spiderX": q.Get("spx"),
		}
	case "none":
	default:
		return nil, fmt.Errorf("неизвестный security=%s", security)
	}
	switch network {
	case "tcp", "raw":
	case "ws":
		stream["wsSettings"] = map[string]any{"path": firstOf(q.Get("path"), "/"), "host": q.Get("host")}
	case "grpc":
		stream["grpcSettings"] = map[string]any{"serviceName": q.Get("serviceName")}
	case "xhttp":
		stream["xhttpSettings"] = map[string]any{"path": firstOf(q.Get("path"), "/"), "host": q.Get("host"), "mode": firstOf(q.Get("mode"), "auto")}
	case "httpupgrade":
		stream["httpupgradeSettings"] = map[string]any{"path": firstOf(q.Get("path"), "/"), "host": q.Get("host")}
	default:
		return nil, fmt.Errorf("транспорт %s не поддерживается; укажите готовый xray.json", network)
	}
	user := map[string]any{"id": u.User.Username(), "encryption": firstOf(q.Get("encryption"), "none")}
	if f := q.Get("flow"); f != "" {
		user["flow"] = f
	}
	cfg := map[string]any{
		"log": map[string]any{"loglevel": "warning"},
		"inbounds": []any{
			map[string]any{"tag": "socks", "listen": "127.0.0.1", "port": 1080, "protocol": "socks", "settings": map[string]any{"udp": true}},
			map[string]any{"tag": "http", "listen": "127.0.0.1", "port": 1081, "protocol": "http"},
		},
		"outbounds": []any{
			map[string]any{"tag": "proxy", "protocol": "vless", "streamSettings": stream,
				"settings": map[string]any{"vnext": []any{map[string]any{"address": u.Hostname(), "port": port, "users": []any{user}}}}},
			map[string]any{"tag": "direct", "protocol": "freedom"},
		},
		"routing": map[string]any{"rules": []any{
			map[string]any{"type": "field", "outboundTag": "direct",
				"ip": []string{"127.0.0.0/8", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "169.254.0.0/16", "::1/128", "fc00::/7", "fe80::/10"}},
		}},
	}
	return json.MarshalIndent(cfg, "", "  ")
}

// restartServices restarts everything and waits until it runs.
func restartServices(e *env) error {
	return fixServices(e)
}
