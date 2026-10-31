package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"voice/internal/adb"
)

func TestHostsOfSkipsOwnAddress(t *testing.T) {
	_, n, _ := net.ParseCIDR("192.168.1.10/16")
	n.IP = net.ParseIP("192.168.1.10")
	hs := hostsOf(n)
	if len(hs) != 253 || hs[0] != "192.168.1.1" || hs[252] != "192.168.1.254" {
		t.Fatalf("%d hosts, %s..%s", len(hs), hs[0], hs[len(hs)-1])
	}
	for _, h := range hs {
		if h == "192.168.1.10" {
			t.Fatal("own address scanned")
		}
	}
}

func TestFindRepoFromSubdir(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "voice", "cmd"), 0o755)
	os.WriteFile(filepath.Join(root, "voice", "deploy.sh"), nil, 0o644)
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	os.Chdir(filepath.Join(root, "voice", "cmd"))
	got, err := findRepo()
	if err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.EvalSymlinks(root)
	if got, _ = filepath.EvalSymlinks(got); got != want {
		t.Fatalf("repo %s, want %s", got, want)
	}
}

func TestYandexAppsKeepPlatform(t *testing.T) {
	for _, p := range yandexApps {
		switch p {
		case "com.yandex.tv.services", "com.yandex.tv.services.platform", "com.yandex.tv.services.privileged",
			"com.yandex.server.SystemExtensionService", "com.yandex.tv.home", "com.yandex.tv.settings", "com.yandex.tv.setupwizard":
			t.Errorf("%s is something the system leans on", p)
		}
	}
}

func TestVLESSRealityConfig(t *testing.T) {
	b, err := vlessConfig("vless://11111111-2222-3333-4444-555555555555@example.org:443?type=tcp&security=reality&pbk=PUBKEY&fp=chrome&sni=www.example.com&sid=ab12&flow=xtls-rprx-vision&encryption=none#name")
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Inbounds []struct {
			Port     int    `json:"port"`
			Listen   string `json:"listen"`
			Protocol string `json:"protocol"`
		} `json:"inbounds"`
		Outbounds []struct {
			Protocol string `json:"protocol"`
			Settings struct {
				Vnext []struct {
					Address string `json:"address"`
					Port    int    `json:"port"`
					Users   []struct {
						ID, Flow string
					} `json:"users"`
				} `json:"vnext"`
			} `json:"settings"`
			Stream struct {
				Network  string            `json:"network"`
				Security string            `json:"security"`
				Reality  map[string]string `json:"realitySettings"`
			} `json:"streamSettings"`
		} `json:"outbounds"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Inbounds) != 2 || cfg.Inbounds[0].Port != 1080 || cfg.Inbounds[1].Port != 1081 || cfg.Inbounds[0].Listen != "127.0.0.1" {
		t.Fatalf("inbounds %+v", cfg.Inbounds)
	}
	o := cfg.Outbounds[0]
	v := o.Settings.Vnext[0]
	if o.Protocol != "vless" || v.Address != "example.org" || v.Port != 443 || v.Users[0].ID != "11111111-2222-3333-4444-555555555555" || v.Users[0].Flow != "xtls-rprx-vision" {
		t.Fatalf("outbound %+v", o)
	}
	if o.Stream.Network != "tcp" || o.Stream.Security != "reality" || o.Stream.Reality["publicKey"] != "PUBKEY" || o.Stream.Reality["serverName"] != "www.example.com" || o.Stream.Reality["shortId"] != "ab12" {
		t.Fatalf("stream %+v", o.Stream)
	}
}

func TestVLESSRejects(t *testing.T) {
	for _, l := range []string{
		"vmess://abc", "vless://id@host?type=tcp", "vless://id@host:443?security=reality",
		"vless://id@host:443?type=kcp",
	} {
		if _, err := vlessConfig(l); err == nil {
			t.Errorf("%s accepted", l)
		}
	}
}

func TestCheckDigest(t *testing.T) {
	data := []byte("xray")
	good := "MD5= x\nSHA2-256= 0a6b3c3f6d5b4e43dfc4b2d8fd6a3b1bd4e2a1b2e4b6e0c3d7a1f1f6b0c9e2a1\n"
	if err := checkDigest(data, good); err == nil {
		t.Fatal("wrong digest accepted")
	}
	sum := sha256.Sum256(data)
	if err := checkDigest(data, "SHA2-256= "+hex.EncodeToString(sum[:])); err != nil {
		t.Fatal(err)
	}
	if err := checkDigest(data, "MD5= x"); err == nil {
		t.Fatal("no sha256 accepted")
	}
}

func TestFindAnyAcrossChunks(t *testing.T) {
	data := make([]byte, 3<<20)
	key := []byte("sk-0123456789abcdef")
	copy(data[1<<20-5:], key) // straddles the first chunk boundary
	if got, err := findAny(bytes.NewReader(data), [][]byte{[]byte("absent-needle-xyz"), key}); err != nil || got != 1 {
		t.Fatalf("got %d, %v", got, err)
	}
	if got, _ := findAny(bytes.NewReader(data[:1<<20-5]), [][]byte{key}); got != -1 {
		t.Fatalf("false hit %d", got)
	}
}

func TestCredStringsFromXray(t *testing.T) {
	b, err := vlessConfig("vless://11111111-2222-3333-4444-555555555555@example.org:443?type=tcp&security=reality&pbk=PUBKEY&fp=chrome&sni=www.example.com&sid=ab12&flow=xtls-rprx-vision&encryption=none#name")
	if err != nil {
		t.Fatal(err)
	}
	var v any
	json.Unmarshal(b, &v)
	got := credStrings(v)
	if len(got) != 1 || got[0] != "11111111-2222-3333-4444-555555555555" {
		t.Fatalf("creds %q", got)
	}
}

// fakeDevice answers every adb call with out and a successful exit; a root
// script then reports whatever status out carries.
func fakeDevice(out string) *adb.Device {
	return &adb.Device{Serial: "fake", Command: func(args ...string) *exec.Cmd {
		return exec.Command("sh", "-c", "cat >/dev/null; printf '%s' \"$1\"", "sh", out)
	}}
}

func TestPKCEChallenge(t *testing.T) {
	// the example of RFC 7636, appendix B
	got := pkceChallenge("dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk")
	if want := "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"; got != want {
		t.Fatalf("challenge %s, want %s", got, want)
	}
}

func TestSpotifyCallbackExtractsCode(t *testing.T) {
	get := func(query string) (int, string, error) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		type res struct {
			code string
			err  error
		}
		done := make(chan res, 1)
		go func() {
			c, err := awaitCallback(ctx, ln, "st4te")
			done <- res{c, err}
		}()
		var status int
		for _, q := range strings.Split(query, "|") {
			r, err := http.Get("http://" + ln.Addr().String() + "/callback?" + q)
			if err != nil {
				t.Fatal(err)
			}
			r.Body.Close()
			status = r.StatusCode
		}
		r := <-done
		return status, r.code, r.err
	}
	if st, code, err := get("code=abc&state=st4te"); err != nil || code != "abc" || st != 200 {
		t.Fatalf("good: %d %q %v", st, code, err)
	}
	// a wrong state is refused and the right one still gets through
	if st, code, err := get("code=evil&state=other|code=abc&state=st4te"); err != nil || code != "abc" || st != 200 {
		t.Fatalf("after wrong state: %d %q %v", st, code, err)
	}
	if _, code, err := get("error=access_denied&state=st4te"); err == nil || code != "" || !strings.Contains(err.Error(), "access_denied") {
		t.Fatalf("denied: %q %v", code, err)
	}
}

func TestPiperStepNeedsRootfs(t *testing.T) {
	e := &env{u: newUI(false), d: fakeDevice("__librestation_rc=1\n")}
	ok, detail, err := checkPiper(e)
	if err != nil || ok || detail != "нужен rootfs" {
		t.Fatalf("ok=%v detail=%q err=%v", ok, detail, err)
	}
	e.d = fakeDevice("__librestation_rc=0\n")
	if ok, detail, err := checkPiper(e); err != nil || !ok {
		t.Fatalf("installed: ok=%v detail=%q err=%v", ok, detail, err)
	}
}

func TestFetchCachedChecksAndDeletes(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		io.WriteString(w, "voice")
	}))
	defer srv.Close()
	sum := sha256.Sum256([]byte("voice"))
	dir := t.TempDir()
	d := download{"v.onnx", srv.URL + "/v.onnx", hex.EncodeToString(sum[:])}
	for range 2 { // the second call uses the cache
		p, err := fetchCached(newUI(false), dir, d)
		if err != nil || p != filepath.Join(dir, "v.onnx") {
			t.Fatalf("%q %v", p, err)
		}
	}
	if hits != 1 {
		t.Fatalf("%d downloads, want 1", hits)
	}
	bad := download{"bad.onnx", srv.URL + "/bad.onnx", strings.Repeat("0", 64)}
	if _, err := fetchCached(newUI(false), dir, bad); err == nil {
		t.Fatal("wrong hash accepted")
	}
	if m, _ := filepath.Glob(filepath.Join(dir, "bad.onnx*")); len(m) != 0 {
		t.Fatalf("mismatching download kept: %v", m)
	}
}

func TestExchangeCode(t *testing.T) {
	var form url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		form = r.PostForm
		if r.PostForm.Get("code") == "bad" {
			w.WriteHeader(400)
			io.WriteString(w, `{"error":"invalid_grant","error_description":"Invalid authorization code"}`)
			return
		}
		io.WriteString(w, `{"access_token":"a","refresh_token":"r-1"}`)
	}))
	defer srv.Close()
	got, err := exchangeCode(srv.URL, "cid", "good", "ver")
	if err != nil || got != "r-1" {
		t.Fatalf("%q %v", got, err)
	}
	if form.Get("grant_type") != "authorization_code" || form.Get("client_id") != "cid" || form.Get("code_verifier") != "ver" || form.Get("redirect_uri") != spotifyRedirectURI {
		t.Fatalf("form %v", form)
	}
	if _, err := exchangeCode(srv.URL, "cid", "bad", "ver"); err == nil || !strings.Contains(err.Error(), "invalid_grant") {
		t.Fatalf("error %v", err)
	}
}

func TestCheckSpotify(t *testing.T) {
	linked := "null"
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		io.WriteString(w, `{"assistant":{"spotify_linked":`+linked+`}}`)
	}))
	defer srv.Close()
	host, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	oldPort, oldPoll := panelPort, spotifyPoll
	panelPort, spotifyPoll = port, time.Millisecond
	defer func() { panelPort, spotifyPoll = oldPort, oldPoll }()
	e := &env{u: newUI(false), d: fakeDevice("__librestation_rc=0\n"), host: host}

	if ok, detail, err := checkSpotify(e); err != nil || ok || !strings.Contains(detail, "проверяет") || calls != spotifyPolls {
		t.Fatalf("null: ok=%v %q %v after %d polls", ok, detail, err, calls)
	}
	linked = "false"
	calls = 0
	if ok, detail, err := checkSpotify(e); err != nil || ok || !strings.Contains(detail, "не принял") || calls != spotifyPolls {
		t.Fatalf("false: ok=%v %q %v", ok, detail, err)
	}
	linked = "true"
	if ok, _, err := checkSpotify(e); err != nil || !ok {
		t.Fatalf("true: ok=%v %v", ok, err)
	}
	e.d = fakeDevice("__librestation_rc=1\n") // no token file
	if ok, _, err := checkSpotify(e); err != nil || ok {
		t.Fatalf("no file: ok=%v %v", ok, err)
	}
}

func TestExchangeCodeErrorBodies(t *testing.T) {
	status, body := 502, strings.Repeat("x", 500)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	defer srv.Close()
	_, err := exchangeCode(srv.URL, "c", "c", "v")
	if err == nil || len(err.Error()) > 300 {
		t.Fatalf("long body: %v", err)
	}
	status, body = 200, `{"access_token":"SECRET"}`
	_, err = exchangeCode(srv.URL, "c", "c", "v")
	if err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("200 body: %v", err)
	}
}

func TestCheckAssistantStockSounds(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"assistant":{"status":{"state":"idle"}}}`)
	}))
	defer srv.Close()
	host, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	oldPort := panelPort
	panelPort = port
	defer func() { panelPort = oldPort }()

	e := &env{u: newUI(false), d: fakeDevice("__librestation_rc=0\n"), host: host}
	if ok, detail, err := checkAssistant(e); err != nil || !ok {
		t.Fatalf("good: ok=%v %q %v", ok, detail, err)
	}
	e.d = fakeDevice("bad=no_internet\r\n__librestation_rc=0\n")
	if ok, detail, err := checkAssistant(e); err != nil || ok || !strings.Contains(detail, "no_internet.wav") || strings.Contains(detail, "something_went_wrong") {
		t.Fatalf("bad: ok=%v %q %v", ok, detail, err)
	}
}

// modelDevice is a fake speaker: root scripts land in log and succeed, and
// `adb pull` of a model file copies it from src (a missing one fails).
func modelDevice(log, src string) *adb.Device {
	return &adb.Device{Serial: "fake", Command: func(args ...string) *exec.Cmd {
		if len(args) == 5 && args[2] == "pull" {
			return exec.Command("cp", filepath.Join(src, filepath.Base(args[3])), args[4])
		}
		return exec.Command("sh", "-c", "cat >>\"$1\"; printf '__librestation_rc=0\\n'", "sh", log)
	}}
}

// stubLoad replaces the host-side model check for one test.
func stubLoad(t *testing.T, f func(dir string) error) {
	old := loadYcmd
	loadYcmd = f
	t.Cleanup(func() { loadYcmd = old })
}

func TestCheckYcmd(t *testing.T) {
	log := filepath.Join(t.TempDir(), "scripts")
	src := t.TempDir()
	for _, f := range ycmdFiles {
		os.WriteFile(filepath.Join(src, f), []byte("data of "+f), 0o644)
	}
	var loaded []string
	stubLoad(t, func(dir string) error {
		for _, f := range ycmdFiles {
			b, _ := os.ReadFile(filepath.Join(dir, f))
			loaded = append(loaded, string(b))
		}
		return nil
	})
	e := &env{u: newUI(false), d: modelDevice(log, src)}
	if ok, _, err := checkYcmd(e); err != nil || !ok {
		t.Fatalf("in place: ok=%v err=%v", ok, err)
	}
	b, _ := os.ReadFile(log)
	for i, f := range ycmdFiles {
		if !strings.Contains(string(b), "test -s "+ycmdDir+"/"+f) {
			t.Errorf("%s is not checked:\n%s", f, b)
		}
		if loaded[i] != "data of "+f {
			t.Errorf("%s pulled as %q", f, loaded[i])
		}
	}
	e.d = fakeDevice("__librestation_rc=1\n")
	if ok, _, err := checkYcmd(e); err != nil || ok {
		t.Fatalf("missing: ok=%v err=%v", ok, err)
	}
}

// TestCheckYcmdUnreadable: files in place that the model loader rejects.
func TestCheckYcmdUnreadable(t *testing.T) {
	src := t.TempDir()
	for _, f := range ycmdFiles {
		os.WriteFile(filepath.Join(src, f), []byte("not a model"), 0o644)
	}
	e := &env{u: newUI(false), d: modelDevice(filepath.Join(t.TempDir(), "scripts"), src)}
	ok, detail, err := checkYcmd(e)
	if err != nil || ok || !strings.HasPrefix(detail, "модель не читается: ") {
		t.Fatalf("ok=%v %q err=%v", ok, detail, err)
	}
	os.Remove(filepath.Join(src, "words.txt"))
	ok, detail, err = checkYcmd(e)
	if err != nil || ok || !strings.HasPrefix(detail, "не удалось скачать модель с колонки: ") {
		t.Fatalf("pull fails: ok=%v %q err=%v", ok, detail, err)
	}
}

// TestCheckYcmdReal loads the real model through the fake speaker.
func TestCheckYcmdReal(t *testing.T) {
	src := os.Getenv("YCMD_DIR")
	if src == "" {
		t.Skip("YCMD_DIR not set")
	}
	e := &env{u: newUI(false), d: modelDevice(filepath.Join(t.TempDir(), "scripts"), src)}
	if ok, detail, err := checkYcmd(e); err != nil || !ok {
		t.Fatalf("ok=%v %q err=%v", ok, detail, err)
	}
}

func TestFixYcmdWithoutAPK(t *testing.T) {
	e := &env{u: newUI(false), d: fakeDevice("__librestation_rc=1\n")}
	err := fixYcmd(e)
	if err == nil || !strings.Contains(err.Error(), "нет ServicesIosdk.apk") {
		t.Fatalf("err = %v", err)
	}
}

func TestFixYcmdExtractsFromAPK(t *testing.T) {
	log := filepath.Join(t.TempDir(), "scripts")
	d := &adb.Device{Serial: "fake", Command: func(args ...string) *exec.Cmd {
		// Everything pushed to the fake speaker lands in log; every root
		// script succeeds, so the services count as running.
		return exec.Command("sh", "-c", "cat >>\"$1\"; printf '__librestation_rc=0\\n'", "sh", log)
	}}
	e := &env{u: newUI(false), d: d}
	stubLoad(t, func(string) error { return nil })
	if err := fixYcmd(e); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(log)
	if strings.Contains(string(b), "unzip -o -j") || strings.Contains(string(b), " -j ") {
		t.Errorf("busybox unzip has no -j:\n%s", b)
	}
	for _, want := range []string{
		"/vendor/xbin/busybox unzip -o " + ycmdAPK,
		"'assets/quark/spotter_models/command/*'",
		"-d " + ycmdTmp + " || true",
		"mv " + ycmdTmp + "/assets/quark/spotter_models/command/words.txt " + ycmdDir + "/",
		"rm -rf " + ycmdTmp,
	} {
		if !strings.Contains(string(b), want) {
			t.Errorf("no %q in the scripts sent to the speaker:\n%s", want, b)
		}
	}
}

func TestFixYcmdEmptyExtraction(t *testing.T) {
	dir := t.TempDir()
	last := filepath.Join(dir, "last")
	// A script arrives on stdin (the push), then runs without stdin; the
	// model files count as missing: the verification script has "test -s".
	d := &adb.Device{Serial: "fake", Command: func(args ...string) *exec.Cmd {
		return exec.Command("sh", "-c", `in=$(cat); if [ -n "$in" ]; then printf '%s' "$in" >"$1"; else case "$(cat "$1")" in *"test -s"*) echo __librestation_rc=1;; *) echo __librestation_rc=0;; esac; fi`, "sh", last)
	}}
	e := &env{u: newUI(false), d: d}
	err := fixYcmd(e)
	if err == nil || !strings.Contains(err.Error(), "нет модели быстрых команд") {
		t.Fatalf("err = %v", err)
	}
}
