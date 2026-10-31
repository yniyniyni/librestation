package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
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

func TestDownloadRootfsVerifiesHash(t *testing.T) {
	body := []byte("rootfs bytes")
	sum := sha256.Sum256(body)
	good := hex.EncodeToString(sum[:])
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Write(body)
	}))
	defer srv.Close()
	u := &ui{}

	dest := filepath.Join(t.TempDir(), "rootfs.tar.xz")
	if err := downloadRootfs(u, srv.URL, dest, good); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(dest); !bytes.Equal(b, body) {
		t.Fatalf("downloaded %q", b)
	}

	// a matching file is reused without touching the network
	if err := downloadRootfs(u, srv.URL, dest, good); err != nil || hits != 1 {
		t.Fatalf("err %v, %d requests", err, hits)
	}

	// a wrong hash fails and leaves nothing behind
	bad := filepath.Join(t.TempDir(), "rootfs.tar.xz")
	if err := downloadRootfs(u, srv.URL, bad, "00"+good[2:]); err == nil {
		t.Fatal("hash mismatch accepted")
	}
	if _, err := os.Stat(bad); err == nil {
		t.Fatal("bad download not deleted")
	}

	// a stale file with another hash is replaced
	if err := os.WriteFile(bad, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := downloadRootfs(u, srv.URL, bad, good); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(bad); !bytes.Equal(b, body) {
		t.Fatalf("stale file kept: %q", b)
	}
}

func TestSoloistBinaryFromFolder(t *testing.T) {
	dir := t.TempDir()
	if _, err := soloistBinary(dir); err == nil {
		t.Fatal("empty folder accepted")
	}
	bin := filepath.Join(dir, "soloist")
	os.WriteFile(bin, []byte("ELF"), 0o755)
	if got, err := soloistBinary(dir); err != nil || got != bin {
		t.Fatalf("folder: %q, %v", got, err)
	}
	if got, err := soloistBinary(bin); err != nil || got != bin {
		t.Fatalf("file: %q, %v", got, err)
	}
}
