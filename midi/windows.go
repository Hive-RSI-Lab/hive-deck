//go:build windows

package midi

import (
	"fmt"
	"sync"
	"syscall"
	"unsafe"
)

// On Windows a USB MIDI device is opened through winmm.dll, the multimedia
// API every Windows has: an input with a callback and an output for short
// messages. Plain syscalls, no cgo.

var (
	winmm              = syscall.NewLazyDLL("winmm.dll")
	midiInGetNumDevs   = winmm.NewProc("midiInGetNumDevs")
	midiInGetDevCapsW  = winmm.NewProc("midiInGetDevCapsW")
	midiInOpen         = winmm.NewProc("midiInOpen")
	midiInStart        = winmm.NewProc("midiInStart")
	midiInStop         = winmm.NewProc("midiInStop")
	midiInClose        = winmm.NewProc("midiInClose")
	midiOutGetNumDevs  = winmm.NewProc("midiOutGetNumDevs")
	midiOutGetDevCapsW = winmm.NewProc("midiOutGetDevCapsW")
	midiOutOpen        = winmm.NewProc("midiOutOpen")
	midiOutShortMsg    = winmm.NewProc("midiOutShortMsg")
	midiOutClose       = winmm.NewProc("midiOutClose")
)

const (
	callbackFunction = 0x30000
	mimData          = 0x3C3
)

type midiInCaps struct {
	Mid, Pid      uint16
	DriverVersion uint32
	Pname         [32]uint16
	Support       uint32
}

type midiOutCaps struct {
	Mid, Pid                         uint16
	DriverVersion                    uint32
	Pname                            [32]uint16
	Technology, Voices, Notes, Chans uint16
	Support                          uint32
}

func inNames() []string {
	n, _, _ := midiInGetNumDevs.Call()
	var out []string
	for i := uintptr(0); i < n; i++ {
		var c midiInCaps
		if r, _, _ := midiInGetDevCapsW.Call(i, uintptr(unsafe.Pointer(&c)), unsafe.Sizeof(c)); r == 0 {
			out = append(out, syscall.UTF16ToString(c.Pname[:]))
		} else {
			out = append(out, "")
		}
	}
	return out
}

func outNames() []string {
	n, _, _ := midiOutGetNumDevs.Call()
	var out []string
	for i := uintptr(0); i < n; i++ {
		var c midiOutCaps
		if r, _, _ := midiOutGetDevCapsW.Call(i, uintptr(unsafe.Pointer(&c)), unsafe.Sizeof(c)); r == 0 {
			out = append(out, syscall.UTF16ToString(c.Pname[:]))
		} else {
			out = append(out, "")
		}
	}
	return out
}

func list() ([]string, error) {
	var out []string
	for _, n := range inNames() {
		out = append(out, n+" (in)")
	}
	for _, n := range outNames() {
		out = append(out, n+" (out)")
	}
	return out, nil
}

// The callback can't carry a Go pointer through winmm, so open ports are
// found by the handle winmm passes back.
var (
	portsMu sync.Mutex
	ports   = map[uintptr]*winPort{}
	inCB    = syscall.NewCallback(func(h, msg, instance, p1, p2 uintptr) uintptr {
		if msg != mimData {
			return 0
		}
		portsMu.Lock() // held through the send, so Close can't close in under it
		defer portsMu.Unlock()
		if p := ports[h]; p != nil {
			m := Msg{byte(p1), byte(p1 >> 8), byte(p1 >> 16)}
			select {
			case p.in <- m:
			default: // a full queue drops rather than stalls the driver's thread
			}
		}
		return 0
	})
)

type winPort struct {
	name    string
	in      chan Msg
	hin     uintptr
	hout    uintptr
	mu      sync.Mutex
	closing sync.Once
}

func open(want string) (Port, error) {
	in, out := -1, -1
	var name string
	for i, n := range inNames() {
		if Match(n, want) {
			in, name = i, n
			break
		}
	}
	for i, n := range outNames() {
		if Match(n, want) {
			out = i
			break
		}
	}
	if in < 0 {
		return nil, fmt.Errorf("%w matching %q (is it plugged in, and closed in rekordbox? `deck list` shows what is there)", ErrNoDevice, want)
	}
	p := &winPort{name: name, in: make(chan Msg, 256)}
	portsMu.Lock()
	defer portsMu.Unlock()
	if r, _, _ := midiInOpen.Call(uintptr(unsafe.Pointer(&p.hin)), uintptr(in), inCB, 0, callbackFunction); r != 0 {
		return nil, fmt.Errorf("opening %s for input: MMSYSERR %d (another program may hold it)", name, r)
	}
	ports[p.hin] = p
	if out >= 0 {
		if r, _, _ := midiOutOpen.Call(uintptr(unsafe.Pointer(&p.hout)), uintptr(out), 0, 0, 0); r != 0 {
			p.hout = 0 // lights stay off; the controls still work
		}
	}
	midiInStart.Call(p.hin)
	return p, nil
}

func (p *winPort) Name() string   { return p.name }
func (p *winPort) In() <-chan Msg { return p.in }

func (p *winPort) Send(m Msg) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.hout == 0 {
		return nil
	}
	if r, _, _ := midiOutShortMsg.Call(p.hout, uintptr(m[0])|uintptr(m[1])<<8|uintptr(m[2])<<16); r != 0 {
		return fmt.Errorf("midiOutShortMsg: MMSYSERR %d", r)
	}
	return nil
}

func (p *winPort) Close() error {
	p.closing.Do(func() {
		midiInStop.Call(p.hin)
		midiInClose.Call(p.hin)
		p.mu.Lock()
		if p.hout != 0 {
			midiOutClose.Call(p.hout)
			p.hout = 0
		}
		p.mu.Unlock()
		portsMu.Lock()
		delete(ports, p.hin)
		close(p.in)
		portsMu.Unlock()
	})
	return nil
}
