//go:build !linux

package input

import (
	"errors"
	"os"
)

func grab(*os.File) error { return errors.New("EVIOCGRAB needs Linux") }
