package daemon

import (
	"fmt"
	"log"
	"os/exec"
	"strings"
)

// Actuator performs playback commands on the station.
type Actuator interface {
	Pause() error
	Play() error
	Next() error
	Prev() error
	Step(delta int) error
}

// VolumeStep is the volume change of the louder/quieter actions.
const VolumeStep = 5

// actionNames is the fixed list of per-word actions; "" means none.
var actionNames = []string{"pause", "play", "next", "prev", "louder", "quieter"}

// normalizeAction maps "none" to "" and rejects unknown names.
func normalizeAction(a string) (string, error) {
	a = strings.TrimSpace(a)
	if a == "none" {
		return "", nil
	}
	if a == "" {
		return "", nil
	}
	for _, n := range actionNames {
		if a == n {
			return a, nil
		}
	}
	return "", fmt.Errorf("неизвестное действие %q", a)
}

// runAction performs a validated action; "" does nothing.
func runAction(a Actuator, action string) error {
	if action == "" {
		return nil
	}
	if a == nil {
		return fmt.Errorf("no actuator")
	}
	switch action {
	case "pause":
		return a.Pause()
	case "play":
		return a.Play()
	case "next":
		return a.Next()
	case "prev":
		return a.Prev()
	case "louder":
		return a.Step(VolumeStep)
	case "quieter":
		return a.Step(-VolumeStep)
	}
	return fmt.Errorf("unknown action %q", action)
}

// killYandex kills Yandex's voice stack so it releases the microphone.
// A variable so tests can replace it.
var killYandex = func() {
	out, err := exec.Command("/vendor/xbin/busybox", "pidof", "com.yandex.io.sdk").Output()
	pids := strings.Fields(string(out))
	if err != nil || len(pids) == 0 {
		log.Printf("killYandex: no com.yandex.io.sdk process")
		return
	}
	args := append([]string{"kill", "-9"}, pids...)
	if b, err := exec.Command("/vendor/xbin/busybox", args...).CombinedOutput(); err != nil {
		log.Printf("killYandex: %v: %s", err, strings.TrimSpace(string(b)))
		return
	}
	log.Printf("killYandex: killed com.yandex.io.sdk (%s)", strings.Join(pids, " "))
}
