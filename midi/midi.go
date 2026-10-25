// Package midi opens a USB MIDI controller for reading and writing short
// messages, with a driver per system: ALSA raw MIDI on Linux, CoreMIDI on
// macOS and the Windows multimedia API on Windows. Fake stands in for a
// device in tests and dry runs.
package midi

import (
	"errors"
	"fmt"
	"strings"
	"sync"
)

// Msg is one short MIDI message: status, then up to two data bytes.
type Msg [3]byte

// Status is the message type (high nibble), Channel the low nibble.
func (m Msg) Status() byte  { return m[0] & 0xF0 }
func (m Msg) Channel() byte { return m[0] & 0x0F }

func (m Msg) String() string { return fmt.Sprintf("%02X %02X %02X", m[0], m[1], m[2]) }

// Port is an open device: messages arrive on In until Close; Send writes one.
type Port interface {
	Name() string
	In() <-chan Msg
	Send(Msg) error
	Close() error
}

// ErrNoDevice is returned when no port matches.
var ErrNoDevice = errors.New("no MIDI device found")

// Match reports whether a port's name is the one asked for: letters and
// digits only, case folded, so "ddj-flx4" finds "DDJ-FLX4 MIDI 1".
func Match(name, want string) bool {
	return strings.Contains(fold(name), fold(want))
}

func fold(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Open finds a device whose name matches want (see Match) on this system.
func Open(want string) (Port, error) { return open(want) }

// List names the devices this system's driver sees.
func List() ([]string, error) { return list() }

// Parser turns a raw MIDI byte stream into short messages: it keeps running
// status, drops system exclusive and real-time bytes, and passes the rest.
type Parser struct {
	status byte
	data   [2]byte
	n      int
	sysex  bool
}

// Feed takes bytes as they come and returns the complete messages in them.
func (p *Parser) Feed(b []byte) []Msg {
	var out []Msg
	for _, c := range b {
		switch {
		case c >= 0xF8: // real-time: clock, start, stop; never breaks a message
			continue
		case c == 0xF0:
			p.sysex = true
			continue
		case c == 0xF7:
			p.sysex = false
			continue
		case c >= 0xF0: // other system common messages; not used by controllers
			p.status, p.n = 0, 0
			continue
		case c&0x80 != 0:
			p.sysex = false
			p.status, p.n = c, 0
			continue
		}
		if p.sysex || p.status == 0 {
			continue
		}
		p.data[p.n] = c
		p.n++
		if p.n == dataLen(p.status) {
			m := Msg{p.status, p.data[0]}
			if p.n == 2 {
				m[2] = p.data[1]
			}
			out = append(out, m)
			p.n = 0
		}
	}
	return out
}

func dataLen(status byte) int {
	switch status & 0xF0 {
	case 0xC0, 0xD0:
		return 1
	}
	return 2
}

// Fake is a device in memory: tests Press its controls and read what was
// sent back to its lights.
type Fake struct {
	name string
	in   chan Msg
	mu   sync.Mutex
	sent []Msg
	once sync.Once
}

// NewFake is a fake device named name.
func NewFake(name string) *Fake { return &Fake{name: name, in: make(chan Msg, 256)} }

func (f *Fake) Name() string   { return f.name }
func (f *Fake) In() <-chan Msg { return f.in }
func (f *Fake) Close() error   { f.once.Do(func() { close(f.in) }); return nil }
func (f *Fake) Receive(m ...Msg) {
	for _, x := range m {
		f.in <- x
	}
}

// Send records m.
func (f *Fake) Send(m Msg) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, m)
	return nil
}

// Sent returns the messages sent so far and forgets them.
func (f *Fake) Sent() []Msg {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.sent
	f.sent = nil
	return out
}
