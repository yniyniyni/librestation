// Package adb drives a rooted Yandex Station over adb from the host.
//
// Root goes through the speaker's `mysudo`, which has two quirks: it needs a
// tty (`adb shell -tt`), and it splits its command line on spaces, so
// `mysudo sh -c "a b"` runs just `sh -c a`. Root commands are therefore
// written to a script on the speaker and run as `mysudo -n sh <script>`.
// mysudo also always exits 0, so the script prints its own status.
package adb

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
)

// Tmp is a directory on the speaker the adb shell user can write.
const Tmp = "/data/local/tmp"

const rcMark = "__librestation_rc="

// Device is one speaker, by adb serial (ip:port).
type Device struct {
	Serial string
	// Command builds the adb invocation; tests replace it.
	Command func(args ...string) *exec.Cmd
}

func New(serial string) *Device {
	return &Device{Serial: serial, Command: func(args ...string) *exec.Cmd { return exec.Command("adb", args...) }}
}

func (d *Device) run(stdin io.Reader, args ...string) (string, error) {
	cmd := d.Command(append([]string{"-s", d.Serial}, args...)...)
	cmd.Stdin = stdin
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	s := strings.ReplaceAll(out.String(), "\r", "")
	if err != nil {
		return s, fmt.Errorf("adb %s: %v: %s", args[0], err, strings.TrimSpace(s))
	}
	return s, nil
}

// Shell runs cmd as the adb shell user.
func (d *Device) Shell(cmd string) (string, error) { return d.run(nil, "shell", cmd) }

// Push copies a host file to the speaker.
func (d *Device) Push(local, remote string) error {
	_, err := d.run(nil, "push", local, remote)
	return err
}

// Pull copies a speaker file to the host.
func (d *Device) Pull(remote, local string) error {
	_, err := d.run(nil, "pull", remote, local)
	return err
}

// Write stores data in remote as the shell user, readable only by it.
// The data goes over stdin and never into a command line.
func (d *Device) Write(remote string, data []byte) error {
	_, err := d.run(bytes.NewReader(data), "shell", "umask 077 && cat > "+remote)
	return err
}

// RootError is a root script that ran and failed.
type RootError struct {
	Code   int
	Output string
}

func (e *RootError) Error() string {
	return fmt.Sprintf("root command failed (%d): %s", e.Code, strings.TrimSpace(e.Output))
}

// Root runs script as root with `set -e` and returns its output. The script
// is trusted text: never put secrets or untrusted input in it.
func (d *Device) Root(script string) (string, error) {
	path := Tmp + "/.librestation-root.sh"
	body := "(\nset -e\n" + script + "\n)\necho \"" + rcMark + "$?\"\n"
	if err := d.Write(path, []byte(body)); err != nil {
		return "", err
	}
	defer d.Shell("rm -f " + path)
	out, err := d.run(nil, "shell", "-tt", "mysudo -n sh "+path)
	if err != nil {
		return out, err
	}
	return parseRoot(out)
}

// parseRoot splits the status line off a root script's output.
func parseRoot(out string) (string, error) {
	i := strings.LastIndex(out, rcMark)
	if i < 0 {
		return out, errors.New("root: no status from mysudo (is root set up?): " + strings.TrimSpace(out))
	}
	line := out[i+len(rcMark):]
	if j := strings.IndexByte(line, '\n'); j >= 0 {
		line = line[:j]
	}
	rc, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		return out, fmt.Errorf("root: bad status %q", line)
	}
	out = out[:i]
	if rc != 0 {
		return out, &RootError{Code: rc, Output: out}
	}
	return out, nil
}

// RootOK runs a root test script: true when it exits 0, false when it
// exits non-zero, error when it could not run.
func (d *Device) RootOK(script string) (bool, error) {
	_, err := d.Root(script)
	var re *RootError
	if errors.As(err, &re) {
		return false, nil
	}
	return err == nil, err
}

// WriteSecret stores data in remote (root-owned dirs too) with mode 600.
// It is written by the shell user to a temp file and moved into place.
func (d *Device) WriteSecret(remote string, data []byte) error {
	tmp := Tmp + "/.librestation-secret"
	if err := d.Write(tmp, data); err != nil {
		return err
	}
	_, err := d.Root(fmt.Sprintf("mkdir -p %s\nmv -f %s %s\nchmod 600 %s", dirOf(remote), tmp, remote, remote))
	if err != nil {
		d.Shell("rm -f " + tmp)
	}
	return err
}

func dirOf(p string) string {
	if i := strings.LastIndexByte(p, '/'); i > 0 {
		return p[:i]
	}
	return "/"
}
