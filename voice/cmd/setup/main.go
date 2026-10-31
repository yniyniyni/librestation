// Command setup installs the project on a rooted Yandex Station Max and
// checks an installed one.
//
//	./setup              walk through every step, fixing what is missing
//	./setup doctor       only check, change nothing; exit 1 if something is wrong
//	./setup export-rootfs [-o rootfs.tar.xz]   pack this speaker's rootfs for
//	                     another one, without keys, proxy, Soloist login or binaries
//	./setup ... -device 192.168.1.50   which speaker (else ADB_DEVICE, else ask)
//
// Each step first checks whether it is already in place, so setup can be run
// again at any time: on a new speaker it installs everything, on a working
// one it only offers updates.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"voice/internal/adb"
)

func main() {
	mode := "setup"
	args := os.Args[1:]
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		mode, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet(mode, flag.ExitOnError)
	device := fs.String("device", os.Getenv("ADB_DEVICE"), "speaker adb address, ip[:port]")
	out := fs.String("o", "rootfs.tar.xz", "export-rootfs: archive to write")
	fs.Parse(args)
	if mode == "export-rootfs" {
		if err := runExport(newUI(isTerminal()), *device, *out); err != nil {
			fmt.Fprintln(os.Stderr, "✗", err)
			os.Exit(1)
		}
		return
	}
	if mode != "setup" && mode != "doctor" {
		fmt.Fprintf(os.Stderr, "usage: setup [doctor | export-rootfs [-o rootfs.tar.xz]] [-device ip:5555]\n")
		os.Exit(2)
	}
	interactive := mode == "setup" && isTerminal()
	if mode == "setup" && !interactive {
		fmt.Println("Нет терминала: только проверяю, ничего не меняю.")
	}
	// Fixes ask first, so without a terminal setup only checks.
	if err := run(interactive, newUI(interactive), *device); err != nil {
		fmt.Fprintln(os.Stderr, "✗", err)
		os.Exit(1)
	}
}

func run(fix bool, u *ui, device string) error {
	repo, err := findRepo()
	if err != nil {
		return err
	}
	if ok, detail, _ := checkHost(nil); !ok {
		mark("✗", "Инструменты на компьютере", detail)
		return errors.New("сначала поставьте недостающее на компьютер (см. README)")
	}
	serial, err := pickDevice(u, device)
	if err != nil {
		return err
	}
	e := &env{u: u, d: adb.New(serial), host: strings.Split(serial, ":")[0], repo: repo}
	u.say("Колонка %s\n", serial)

	failed := 0
	for _, s := range steps()[1:] { // the host was checked above
		ok, detail, err := s.check(e)
		if err != nil {
			mark("✗", s.name, err.Error())
			return fmt.Errorf("%s: нет связи с колонкой", s.name)
		}
		switch {
		case ok && fix && s.update != "" && u.interactive:
			mark("✓", s.name, detail)
			if u.yes("  "+s.update, false) {
				ok, detail = apply(e, s)
			}
		case ok:
			mark("✓", s.name, detail)
		case fix && s.fix != nil:
			mark("…", s.name, detail)
			if s.ask == "" || u.yes("  "+s.ask, !s.optional) {
				ok, detail = apply(e, s)
				if ok {
					mark("✓", s.name, detail)
				}
			}
		}
		if !ok {
			if s.optional {
				mark("–", s.name, firstOf(detail, "пропущено"))
				continue
			}
			mark("✗", s.name, firstOf(detail, s.hint))
			if s.hint != "" && detail != "" {
				u.say("    %s", s.hint)
			}
			failed++
		}
	}
	if failed > 0 {
		return fmt.Errorf("не готово шагов: %d", failed)
	}
	u.say("\nГотово. Слова команд записываются и обучаются в панели: http://%s:8780", e.host)
	return nil
}

// apply runs a step's fix and checks it again.
func apply(e *env, s step) (bool, string) {
	if err := s.fix(e); err != nil {
		return false, err.Error()
	}
	ok, detail, err := s.check(e)
	if err != nil {
		return false, err.Error()
	}
	return ok, detail
}

func mark(sym, name, detail string) {
	if detail != "" {
		fmt.Printf("%s %s — %s\n", sym, name, detail)
	} else {
		fmt.Printf("%s %s\n", sym, name)
	}
}

func firstOf(s ...string) string {
	for _, x := range s {
		if x != "" {
			return x
		}
	}
	return ""
}

// isTerminal says whether stdin is a terminal (/dev/null is a character
// device too, so the file mode is not enough).
func isTerminal() bool {
	c := exec.Command("sh", "-c", "test -t 0")
	c.Stdin = os.Stdin
	return c.Run() == nil
}

// findRepo walks up from the working directory to the repository root.
func findRepo() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "voice", "deploy.sh")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("запускайте из репозитория (не найден voice/deploy.sh)")
		}
		dir = parent
	}
}
