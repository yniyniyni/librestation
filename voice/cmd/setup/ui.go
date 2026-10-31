package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// ui talks to the person at the terminal. Without one (doctor, CI) it never
// asks and takes the defaults.
type ui struct {
	in          *bufio.Reader
	interactive bool
}

func newUI(interactive bool) *ui {
	return &ui{in: bufio.NewReader(os.Stdin), interactive: interactive}
}

func (u *ui) say(format string, a ...any) { fmt.Printf(format+"\n", a...) }

func (u *ui) line() string {
	s, _ := u.in.ReadString('\n')
	return strings.TrimSpace(s)
}

// ask returns the answer, or def when it is empty or there is no terminal.
func (u *ui) ask(q, def string) string {
	if !u.interactive {
		return def
	}
	if def != "" {
		fmt.Printf("%s [%s]: ", q, def)
	} else {
		fmt.Printf("%s: ", q)
	}
	if s := u.line(); s != "" {
		return s
	}
	return def
}

// yes asks a yes/no question; def is the answer to a bare Enter.
func (u *ui) yes(q string, def bool) bool {
	if !u.interactive {
		return def
	}
	hint := "y/N"
	if def {
		hint = "Y/n"
	}
	fmt.Printf("%s [%s]: ", q, hint)
	switch strings.ToLower(u.line()) {
	case "":
		return def
	case "y", "yes", "д", "да":
		return true
	}
	return false
}

// secret reads a line without echo.
func (u *ui) secret(q string) string {
	if !u.interactive {
		return ""
	}
	fmt.Printf("%s: ", q)
	stty := func(arg string) {
		c := exec.Command("stty", arg)
		c.Stdin = os.Stdin
		c.Run()
	}
	stty("-echo")
	s := u.line()
	stty("echo")
	fmt.Println()
	return s
}
