package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"voice/internal/adb"
)

// The Piper release and the Russian voice voiced reads aloud with. The
// hashes were taken from the files at these URLs when this was written.
const (
	piperRelease = "https://github.com/rhasspy/piper/releases/download/2023.11.14-2/"
	piperVoices  = "https://huggingface.co/rhasspy/piper-voices/resolve/v1.0.0/ru/ru_RU/irina/medium/"
	piperDir     = "/opt/piper" // inside the rootfs; voiced runs /opt/piper/piper from there
	piperUpload  = adb.Tmp + "/piper-upload"
)

// download is a file fetched on the computer and checked before it is pushed.
type download struct {
	name, url, sha256 string
}

var piperFiles = []download{
	{"piper_linux_aarch64.tar.gz", piperRelease + "piper_linux_aarch64.tar.gz", "fea0fd2d87c54dbc7078d0f878289f404bd4d6eea6e7444a77835d1537ab88eb"},
	{"ru_RU-irina-medium.onnx", piperVoices + "ru_RU-irina-medium.onnx", "8ff38212d23da300bbe3705c645e6e5b9475f0bfde01558eb17813e22acaaaaa"},
	{"ru_RU-irina-medium.onnx.json", piperVoices + "ru_RU-irina-medium.onnx.json", "c2ec28bb38e2b59e93b959b3e40348c1afebbd272f30fed5d41205d08e98a9d7"},
}

func checkPiper(e *env) (bool, string, error) {
	if ok, _, err := checkRootfs(e); err != nil || !ok {
		return false, "нужен rootfs", err
	}
	r := rootfs + piperDir
	ok, err := e.d.RootOK(strings.Join([]string{
		"test -x " + r + "/piper", "test -s " + r + "/voice.onnx", "test -s " + r + "/voice.onnx.json"}, "\n"))
	return ok, "", err
}

func fixPiper(e *env) error {
	cache := filepath.Join(e.repo, ".cache", "piper")
	paths := make([]string, len(piperFiles))
	for i, f := range piperFiles {
		p, err := fetchCached(e.u, cache, f)
		if err != nil {
			return err
		}
		paths[i] = p
	}
	e.u.say("Заливаю Piper и голос на колонку…")
	if _, err := e.d.Shell("rm -rf " + piperUpload + " && mkdir -p " + piperUpload); err != nil {
		return err
	}
	for i, f := range piperFiles {
		if err := e.d.Push(paths[i], piperUpload+"/"+f.name); err != nil {
			return err
		}
	}
	out, err := e.d.Root(piperInstallScript())
	if s := strings.TrimSpace(out); s != "" && err != nil {
		e.u.say("%s", s)
	}
	e.d.Shell("rm -rf " + piperUpload)
	return err
}

// piperInstallScript unpacks the upload into the rootfs and starts Piper
// once inside the chroot as a smoke test (it needs its libs and espeak data).
func piperInstallScript() string {
	r := rootfs + piperDir
	return strings.Join([]string{
		"BB=/vendor/xbin/busybox",
		"rm -rf " + r,
		"mkdir -p " + rootfs + "/opt",
		"$BB zcat " + piperUpload + "/" + piperFiles[0].name + " | $BB tar -x -C " + rootfs + "/opt -f -",
		"mv -f " + piperUpload + "/" + piperFiles[1].name + " " + r + "/voice.onnx",
		"mv -f " + piperUpload + "/" + piperFiles[2].name + " " + r + "/voice.onnx.json",
		"chmod 755 " + r + "/piper",
		"sh " + soloistDir + "/chroot.sh " + piperDir + "/piper --help >/dev/null 2>&1",
	}, "\n")
}

// fetchCached returns the path of d in dir, downloading it first unless a
// copy with the right hash is already there. A download that does not match
// is deleted.
func fetchCached(u *ui, dir string, d download) (string, error) {
	path := filepath.Join(dir, d.name)
	if got, err := fileSHA256(path); err == nil {
		if got == d.sha256 {
			return path, nil
		}
		os.Remove(path)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	u.say("Скачиваю %s", d.name)
	if err := fetchFile(d.url, path+".part", u); err != nil {
		os.Remove(path + ".part")
		return "", fmt.Errorf("%s: %w", d.name, err)
	}
	got, err := fileSHA256(path + ".part")
	if err != nil {
		return "", err
	}
	if got != d.sha256 {
		os.Remove(path + ".part")
		return "", fmt.Errorf("%s: контрольная сумма не совпала (получено %s), файл удалён", d.name, got)
	}
	return path, os.Rename(path+".part", path)
}

// fetchFile saves url to path, printing progress when attached to a terminal.
func fetchFile(url, path string, u *ui) error {
	c := http.Client{} // big files: no overall timeout
	r, err := c.Get(url)
	if err != nil {
		return err
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusOK {
		return fmt.Errorf("%s", r.Status)
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	p := &progress{total: r.ContentLength, show: u.interactive}
	_, err = io.Copy(f, io.TeeReader(r.Body, p))
	p.done()
	return err
}

// progress prints "  N / M МБ" on one line, about twice a second.
type progress struct {
	total, n int64
	last     time.Time
	show     bool
}

func (p *progress) Write(b []byte) (int, error) {
	p.n += int64(len(b))
	if p.show && time.Since(p.last) > 500*time.Millisecond {
		p.last = time.Now()
		p.print()
	}
	return len(b), nil
}

func (p *progress) print() {
	if p.total > 0 {
		fmt.Printf("\r  %d / %d МБ", p.n>>20, p.total>>20)
	} else {
		fmt.Printf("\r  %d МБ", p.n>>20)
	}
}

func (p *progress) done() {
	if p.show {
		p.print()
		fmt.Println()
	}
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
