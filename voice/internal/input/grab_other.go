//go:build !linux

package input

import (
	"errors"
	"os"
)

func grab(*os.File) error { return errors.New("EVIOCGRAB needs Linux") }

func readSwitches(*os.File, []byte) error { return errors.New("EVIOCGSW needs Linux") }
