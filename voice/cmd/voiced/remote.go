package main

import (
	"context"
	"log"
	"os/exec"
	"strconv"
	"time"

	"voice/internal/input"
)

// remoteForward maps Linux evdev codes from the grabbed BLE remote to Android
// KeyEvent codes, injected so the TV UI keeps seeing navigation keys.
var remoteForward = map[uint16]int{
	input.KeyDpadUp:    19, // KEYCODE_DPAD_UP
	input.KeyDpadDown:  20, // KEYCODE_DPAD_DOWN
	input.KeyDpadLeft:  21, // KEYCODE_DPAD_LEFT
	input.KeyDpadRight: 22, // KEYCODE_DPAD_RIGHT
	input.KeyHome:      3,  // KEYCODE_HOME
	input.KeyPower:     26, // KEYCODE_POWER
	input.KeyMenu:      82, // KEYCODE_MENU
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
	exec.Command("input", "keyevent", strconv.Itoa(code)).Run()
}
