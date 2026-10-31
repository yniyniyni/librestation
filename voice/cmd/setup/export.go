package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"voice/internal/adb"
)

// personal are rootfs paths that belong to this speaker's owner and must not
// travel with the archive: keys, proxy settings, Soloist's login and cache,
// the Soloist and Xray binaries (each speaker installs its own), pulse cookie
// and history. They are moved out for the tar and put back afterwards.
var personal = []string{
	"etc/soloist", "etc/xray.json", "etc/resolv.conf",
	"var/lib/soloist", "var/cache/soloist",
	"opt/soloist", "opt/xray",
	"root/.config", "root/.cache", "root/.bash_history",
}

const (
	exportTar  = soloistDir + "/.rootfs-export.tar"
	exportHold = soloistDir + "/.rootfs-export-hold"
)

// rootfsBusybox matches the device-side scripts.
const rootfsBusybox = "BB=/vendor/xbin/busybox\n"

// restoreScript moves held personal files back into the rootfs and drops the
// hold dir only once it is empty of them, so an interrupted export never
// loses a key.
func restoreScript() string {
	var b strings.Builder
	fmt.Fprintf(&b, "if [ -d %s ]; then\n", exportHold)
	for _, p := range personal {
		fmt.Fprintf(&b, "  if [ -e %[1]s/%[2]s ] || [ -L %[1]s/%[2]s ]; then mkdir -p %[3]s/%[4]s && mv -f %[1]s/%[2]s %[3]s/%[2]s; fi\n",
			exportHold, p, rootfs, filepath.Dir(p))
	}
	fmt.Fprintf(&b, "  if [ -z \"$(find %[1]s ! -type d | head -1)\" ]; then rm -rf %[1]s; else echo \"held files left in %[1]s\"; exit 1; fi\nfi\n", exportHold)
	return b.String()
}

// packScript stops the services, unmounts the chroot, holds the personal
// files, tars the rootfs and puts everything back, whatever tar did.
func packScript() string {
	var b strings.Builder
	b.WriteString("set +e\n" + rootfsBusybox)
	b.WriteString(restoreScript())
	fmt.Fprintf(&b, "sh %s/stop.sh >/dev/null\n", soloistDir)
	for _, m := range []string{"dev/shm", "dev/pts", "dev", "sys", "proc"} {
		fmt.Fprintf(&b, "if $BB grep -q ' %[1]s/%[2]s ' /proc/mounts; then $BB umount %[1]s/%[2]s || $BB umount -l %[1]s/%[2]s; fi\n", rootfs, m)
	}
	fmt.Fprintf(&b, "if $BB grep -q ' %s/' /proc/mounts; then echo 'rootfs is still mounted'; exit 1; fi\n", rootfs)
	// sockets cannot go into a tar; pulseaudio makes a new one at start
	fmt.Fprintf(&b, "rm -f %s/tmp/pulse.sock\n", rootfs)
	fmt.Fprintf(&b, "rm -f %s\nmkdir -p %s\nfail=\n", exportTar, exportHold)
	for _, p := range personal {
		fmt.Fprintf(&b, "if [ -e %[1]s/%[2]s ] || [ -L %[1]s/%[2]s ]; then mkdir -p %[3]s/%[4]s && mv -f %[1]s/%[2]s %[3]s/%[2]s || fail=1; fi\n",
			rootfs, p, exportHold, filepath.Dir(p))
	}
	fmt.Fprintf(&b, "rc=1\nif [ -z \"$fail\" ]; then (cd %s && $BB tar -c -f %s .); rc=$?; fi\n", rootfs, exportTar)
	b.WriteString(restoreScript())
	fmt.Fprintf(&b, "if [ $rc -ne 0 ]; then rm -f %s; echo \"tar failed: $rc\"; exit 1; fi\n", exportTar)
	fmt.Fprintf(&b, "chmod 644 %s\n", exportTar)
	return b.String()
}

func runExport(u *ui, device, out string) error {
	for _, c := range []string{"adb", "xz"} {
		if _, err := exec.LookPath(c); err != nil {
			return fmt.Errorf("нет %s на компьютере", c)
		}
	}
	out = hostPath(out)
	if _, err := os.Stat(out); err == nil {
		if !u.interactive || !u.yes("  "+out+" уже есть. Перезаписать?", false) {
			return fmt.Errorf("%s уже есть", out)
		}
	}
	serial, err := pickDevice(u, device)
	if err != nil {
		return err
	}
	e := &env{u: u, d: adb.New(serial), host: strings.Split(serial, ":")[0]}
	u.say("Колонка %s\n", serial)

	if ok, err := e.d.RootOK("test -x " + rootfs + "/usr/bin/pulseaudio"); err != nil {
		return fmt.Errorf("нет связи с колонкой: %w", err)
	} else if !ok {
		return fmt.Errorf("на колонке нет rootfs (%s)", rootfs)
	}
	secrets, err := speakerSecrets(e)
	if err != nil {
		return err
	}
	sizeOut, err := e.d.Root("du -skx " + rootfs + " | cut -f1")
	if err != nil {
		return err
	}
	var needKB int64
	fmt.Sscan(strings.TrimSpace(sizeOut), &needKB)
	dfOut, err := e.d.Shell("df -k /data | tail -1")
	if err != nil {
		return err
	}
	var freeKB int64
	if f := strings.Fields(dfOut); len(f) >= 4 {
		fmt.Sscan(f[3], &freeKB)
	}
	if needKB == 0 || freeKB < needKB+100*1024 {
		return fmt.Errorf("на /data свободно %d МБ, для архива нужно около %d МБ", freeKB/1024, needKB/1024+100)
	}

	wasUp, _, _ := checkServices(e)
	u.say("Rootfs %d МБ. Музыка и голосовые команды остановятся на время упаковки;", needKB/1024)
	u.say("ключи, прокси, вход и кэш Soloist, бинарники Soloist и Xray в архив не попадут.")
	if u.interactive && !u.yes("  Упаковать?", true) {
		return errors.New("отменено")
	}
	defer func() {
		if wasUp {
			u.say("Запускаю сервисы обратно…")
			if _, err := e.d.Root("sh " + soloistDir + "/start.sh"); err != nil {
				u.say("  не запустились: %v; запустите ./setup", err)
			}
		}
	}()

	u.say("Упаковываю на колонке (несколько минут)…")
	if out, err := e.d.Root(packScript()); err != nil {
		return fmt.Errorf("упаковка: %w %s", err, strings.TrimSpace(out))
	}
	defer e.d.Root("rm -f " + exportTar)

	part := out + ".tar.part"
	defer os.Remove(part)
	u.say("Скачиваю…")
	if err := e.d.Pull(exportTar, part); err != nil {
		return err
	}
	u.say("Проверяю, что в архиве нет ключей…")
	f, err := os.Open(part)
	if err != nil {
		return err
	}
	hit, err := findAny(f, secrets)
	f.Close()
	if err != nil {
		return err
	}
	if hit >= 0 {
		return errors.New("в архиве нашёлся ключ или настройка прокси с колонки; архив удалён, ничего не отдавайте")
	}

	u.say("Сжимаю (xz)…")
	tmp := out + ".part"
	defer os.Remove(tmp)
	if err := xzFile(part, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, out); err != nil {
		return err
	}
	st, _ := os.Stat(out)
	u.say("\nГотово: %s (%d МБ).", out, st.Size()>>20)
	u.say("На другом компьютере положите его в корень репозитория и запустите ./setup — шаг rootfs возьмёт его сам.")
	return nil
}

// speakerSecrets are the values that must never show up in the archive: the
// Soloist and Gemini keys and the proxy credentials. They are only compared,
// never printed.
func speakerSecrets(e *env) ([][]byte, error) {
	out, err := e.d.Root(fmt.Sprintf("for f in %s/api-key %s/gemini.key; do [ -s $f ] && cat $f && echo; done; true", soloistDir, voiceDir))
	if err != nil {
		return nil, err
	}
	var s [][]byte
	for _, l := range strings.Split(out, "\n") {
		s = appendSecret(s, l)
	}
	cfg, err := e.d.Root(fmt.Sprintf("cat %s/xray.json 2>/dev/null; true", soloistDir))
	if err != nil {
		return nil, err
	}
	var v any
	if json.Unmarshal([]byte(cfg), &v) == nil {
		for _, x := range credStrings(v) {
			s = appendSecret(s, x)
		}
	}
	return s, nil
}

// appendSecret skips values too short to search for without false hits.
func appendSecret(s [][]byte, v string) [][]byte {
	if v = strings.TrimSpace(v); len(v) >= 12 {
		s = append(s, []byte(v))
	}
	return s
}

// credStrings are the "id" and "password" values anywhere in an Xray config.
func credStrings(v any) []string {
	var out []string
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			if str, ok := val.(string); ok && (k == "id" || k == "password") {
				out = append(out, str)
			} else {
				out = append(out, credStrings(val)...)
			}
		}
	case []any:
		for _, val := range x {
			out = append(out, credStrings(val)...)
		}
	}
	return out
}

// findAny reports which needle occurs in r (-1 for none), reading it in
// chunks that overlap by the longest needle.
func findAny(r io.Reader, needles [][]byte) (int, error) {
	maxLen := 0
	for _, n := range needles {
		maxLen = max(maxLen, len(n))
	}
	if maxLen == 0 {
		return -1, nil
	}
	br := bufio.NewReaderSize(r, 1<<20)
	buf := make([]byte, 0, 1<<20+maxLen)
	chunk := make([]byte, 1<<20)
	for {
		n, err := io.ReadFull(br, chunk)
		buf = append(buf, chunk[:n]...)
		for i, nd := range needles {
			if bytes.Contains(buf, nd) {
				return i, nil
			}
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return -1, nil
		}
		if err != nil {
			return -1, err
		}
		keep := min(len(buf), maxLen-1)
		buf = append(buf[:0], buf[len(buf)-keep:]...)
	}
}

func xzFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	o, err := os.Create(dst)
	if err != nil {
		return err
	}
	cmd := exec.Command("xz", "-T0", "-6", "-c")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = in, o, os.Stderr
	err = cmd.Run()
	if cerr := o.Close(); err == nil {
		err = cerr
	}
	return err
}

// hostPath resolves p against the directory ./setup was started from (the
// wrapper cds into voice/ before go run).
func hostPath(p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	if strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, p[2:])
		}
	}
	base := os.Getenv("SETUP_CWD")
	if base == "" {
		base, _ = os.Getwd()
	}
	return filepath.Join(base, p)
}
