package main

import (
	"context"
	"log"
	"os"
	"os/exec"
	"strconv"
	"time"

	"voice/internal/input"
	"voice/internal/uinput"
)

// The BLE remote's USB ids (YNDX-00402). The uinput clone carries them so
// Android maps its re-emitted scancodes with the same key layout
// (Vendor_057a_Product_006f.kl) as the physical remote.
const (
	remoteVendor  = 0x057A
	remoteProduct = 0x006F
)

// forwardKeycode lists the remote scancodes voiced passes straight through to
// Android, with the Android KeyEvent code used only by the slow `input`
// fallback. The fast path re-emits the same scancode on the uinput clone, so
// the layout resolves it to this keycode on its own. Pairing (231) is a
// firmware-level BLE action with no Android keycode, so it is absent. Voice,
// volume and play/pause are claimed fully by the switch in run.go; Back and OK
// are here because, when not consumed (hush / stop alarm), they fall through to
// Android as BACK and DPAD_CENTER.
var forwardKeycode = map[uint16]int{
	input.KeyDpadUp:    19,  // KEYCODE_DPAD_UP
	input.KeyDpadDown:  20,  // KEYCODE_DPAD_DOWN
	input.KeyDpadLeft:  21,  // KEYCODE_DPAD_LEFT
	input.KeyDpadRight: 22,  // KEYCODE_DPAD_RIGHT
	input.KeyEnter:     23,  // KEYCODE_DPAD_CENTER (OK, scancode 28)
	input.KeySelect:    23,  // KEYCODE_DPAD_CENTER (OK, scancode 353 — what this remote sends)
	input.KeyBack:      4,   // KEYCODE_BACK
	input.KeyHome:      3,   // KEYCODE_HOME
	input.KeyPower:     26,  // KEYCODE_POWER
	input.KeyMenu:      82,  // KEYCODE_MENU
	input.KeyTVInput:   178, // KEYCODE_TV_INPUT
	input.KeyAppSwitch: 187, // KEYCODE_APP_SWITCH
	input.KeySearch:    84,  // KEYCODE_SEARCH
}

// forwarder re-emits grabbed remote keys to Android. It prefers a uinput clone
// of the remote — a kernel write that costs microseconds — and falls back to
// the `input` command (app_process, ~0.65 s per press, which lagged the remote
// badly) only when uinput cannot be opened.
type forwarder struct {
	ui *uinput.Device // nil ⇒ fallback to the `input` command
}

// newForwarder builds the uinput clone; on failure it logs once and returns a
// fallback forwarder so the remote still works, just slowly.
func newForwarder() *forwarder {
	keys := make([]uint16, 0, len(forwardKeycode))
	for sc := range forwardKeycode {
		keys = append(keys, sc)
	}
	ui, err := uinput.New("Yandex Remote 2 (librestation)", remoteVendor, remoteProduct, keys)
	if err != nil {
		log.Printf("remote: uinput unavailable, using the slow input command: %v", err)
		return &forwarder{}
	}
	return &forwarder{ui: ui}
}

// key forwards one scancode to Android. Unknown scancodes are ignored.
func (f *forwarder) key(code uint16) {
	kc, ok := forwardKeycode[code]
	if !ok {
		return
	}
	if f.ui != nil {
		if err := f.ui.Key(code); err != nil {
			log.Printf("remote uinput key %d: %v", code, err)
		}
		return
	}
	go injectKey(kc) // off the reader goroutine: app_process is slow
}

func (f *forwarder) Close() {
	if f.ui != nil {
		f.ui.Close()
	}
}

// readRemote reads BLE remote key presses, reopening the device after errors.
func readRemote(ctx context.Context, path string, onKey func(uint16)) {
	for ctx.Err() == nil {
		err := input.ReadKeys(ctx, path, onKey)
		if ctx.Err() != nil {
			return
		}
		log.Printf("remote %s: %v", path, err)
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
		}
	}
}

// injectKey sends one key press to Android's input subsystem.
func injectKey(code int) {
	cmd := exec.Command("input", "keyevent", strconv.Itoa(code))
	cmd.Env = injectEnv
	if err := cmd.Run(); err != nil {
		log.Printf("inject keyevent %d: %v", code, err)
	}
}

// injectEnv is the environment for the `input` helper. voiced is started by
// mysudo with neither ANDROID_ROOT nor ANDROID_DATA set, and without them the
// `input` command (which runs app_process) aborts in ICU setup — SIGABRT in
// ICURegistration, before any event is injected — so every forwarded remote
// key is silently dropped (e.g. Home never reaches the launcher). /system and
// /data are the station's standard locations; an inherited value wins if set.
var injectEnv = androidToolEnv()

func androidToolEnv() []string {
	env := os.Environ()
	if os.Getenv("ANDROID_ROOT") == "" {
		env = append(env, "ANDROID_ROOT=/system")
	}
	if os.Getenv("ANDROID_DATA") == "" {
		env = append(env, "ANDROID_DATA=/data")
	}
	return env
}
