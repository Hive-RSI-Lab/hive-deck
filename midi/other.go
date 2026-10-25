//go:build !linux && !windows && !(darwin && cgo)

package midi

import (
	"fmt"
	"runtime"
)

func list() ([]string, error) { return nil, unsupported() }

func open(string) (Port, error) { return nil, unsupported() }

func unsupported() error {
	if runtime.GOOS == "darwin" {
		return fmt.Errorf("this deck was built without cgo, which CoreMIDI needs: build it with CGO_ENABLED=1 (the Xcode command line tools give the compiler)")
	}
	return fmt.Errorf("no MIDI driver for %s; the deck runs on Linux, macOS and Windows", runtime.GOOS)
}
