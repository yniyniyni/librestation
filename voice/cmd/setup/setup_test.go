package main

import (
	"net"
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
