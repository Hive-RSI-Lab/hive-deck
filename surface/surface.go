// Package surface is a control surface's MIDI map: which message each
// physical control sends, how its value reads, and which message lights it.
// It turns raw MIDI into named controls ("pad.l.1", "jog.r", "fader.l") and
// named lights back into MIDI, so the action layer (package deck) never sees
// a MIDI byte, and another surface (a custom command center) is a new
// profile, not new actions.
package surface

import (
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/Hive-RSI-Lab/hive-deck/midi"
)

//go:embed profiles/*.json
var builtin embed.FS

// Control types.
const (
	Button   = "button"   // note on with velocity > 0 is a press, 0 (or note off) a release
	Absolute = "absolute" // a fader or knob: the value is its position
	Relative = "relative" // a jog wheel or endless encoder: the value is a step
)

// Control is one physical control.
type Control struct {
	Name     string   `json:"name"`
	Type     string   `json:"type"`
	Msgs     []string `json:"msgs"`               // "B0 22": status and first data byte, in hex
	Lights   []string `json:"lights,omitempty"`   // messages that light it; each is sent
	Encoding string   `json:"encoding,omitempty"` // relative: "offset64" (0x40 is still) or "twos" (0x01 up, 0x7F down)
	Ignore   []string `json:"ignore,omitempty"`   // messages it also sends that carry nothing new (a 14-bit fader's low byte)
	At       [2]int   `json:"at"`                 // for the picture: top left, or the centre of a circle
	Size     [2]int   `json:"size"`
	Shape    string   `json:"shape,omitempty"`
	Label    string   `json:"label,omitempty"`
}

// Profile is one surface.
type Profile struct {
	Name     string    `json:"name"`
	Match    string    `json:"match"` // the device name to open
	Size     [2]int    `json:"size"`
	Note     string    `json:"note,omitempty"`
	Controls []Control `json:"controls"`

	byKey   map[[2]byte]*Control
	ignored map[[2]byte]bool
	byName  map[string]*Control
}

// Event is a control moving.
type Event struct {
	Control string
	Type    string
	Pressed bool    // button: down
	Value   float64 // absolute: 0..1
	Delta   int     // relative: steps, positive clockwise
	Raw     midi.Msg
}

func (e Event) String() string {
	switch e.Type {
	case Button:
		if e.Pressed {
			return e.Control + " press"
		}
		return e.Control + " release"
	case Absolute:
		return fmt.Sprintf("%s %.2f", e.Control, e.Value)
	}
	return fmt.Sprintf("%s %+d", e.Control, e.Delta)
}

// Builtin loads a profile shipped with the deck by name ("ddj-flx4").
func Builtin(name string) (*Profile, error) {
	raw, err := builtin.ReadFile("profiles/" + name + ".json")
	if err != nil {
		return nil, fmt.Errorf("no built-in profile %q", name)
	}
	return Parse(raw)
}

// Load reads a profile: a built-in name, or a path to a JSON file.
func Load(nameOrPath string) (*Profile, error) {
	if !strings.ContainsAny(nameOrPath, `/\.`) {
		return Builtin(nameOrPath)
	}
	raw, err := os.ReadFile(nameOrPath)
	if err != nil {
		return nil, err
	}
	return Parse(raw)
}

// Parse reads and checks a profile.
func Parse(raw []byte) (*Profile, error) {
	var p Profile
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("profile: %v", err)
	}
	p.byKey, p.ignored, p.byName = map[[2]byte]*Control{}, map[[2]byte]bool{}, map[string]*Control{}
	for i := range p.Controls {
		c := &p.Controls[i]
		if c.Name == "" {
			return nil, fmt.Errorf("profile %s: control %d has no name", p.Name, i)
		}
		if p.byName[c.Name] != nil {
			return nil, fmt.Errorf("profile %s: two controls named %s", p.Name, c.Name)
		}
		switch c.Type {
		case Button, Absolute, Relative:
		default:
			return nil, fmt.Errorf("profile %s: %s: type %q isn't button, absolute or relative", p.Name, c.Name, c.Type)
		}
		p.byName[c.Name] = c
		for _, s := range append(append([]string{}, c.Msgs...), c.Lights...) {
			if _, err := key(s); err != nil {
				return nil, fmt.Errorf("profile %s: %s: %v", p.Name, c.Name, err)
			}
		}
		for _, s := range c.Msgs {
			k, _ := key(s)
			if o := p.byKey[k]; o != nil {
				return nil, fmt.Errorf("profile %s: %s and %s both send %s", p.Name, o.Name, c.Name, s)
			}
			p.byKey[k] = c
		}
		for _, s := range c.Ignore {
			k, err := key(s)
			if err != nil {
				return nil, fmt.Errorf("profile %s: %s: %v", p.Name, c.Name, err)
			}
			p.ignored[k] = true
		}
	}
	return &p, nil
}

// key is a message's status (note off read as note on) and first data byte.
func key(s string) ([2]byte, error) {
	f := strings.Fields(s)
	if len(f) != 2 {
		return [2]byte{}, fmt.Errorf("message %q: want two hex bytes, status and number, such as \"97 00\"", s)
	}
	var k [2]byte
	for i, x := range f {
		v, err := strconv.ParseUint(x, 16, 8)
		if err != nil {
			return k, fmt.Errorf("message %q: %v", s, err)
		}
		k[i] = byte(v)
	}
	if k[0] < 0x80 || k[1] > 0x7F {
		return k, fmt.Errorf("message %q: a status byte is 80-EF and a number 00-7F", s)
	}
	return k, nil
}

func msgKey(m midi.Msg) [2]byte {
	s := m[0]
	if s&0xF0 == 0x80 {
		s = 0x90 | s&0x0F
	}
	return [2]byte{s, m[1]}
}

// Control returns the named control.
func (p *Profile) Control(name string) (*Control, bool) {
	c, ok := p.byName[name]
	return c, ok
}

// Decode reads one message. ok is false for a message the profile doesn't
// know; ignored is true for one it knows carries nothing (a low byte).
func (p *Profile) Decode(m midi.Msg) (e Event, ok, ignored bool) {
	k := msgKey(m)
	c := p.byKey[k]
	if c == nil {
		return Event{Raw: m}, false, p.ignored[k]
	}
	e = Event{Control: c.Name, Type: c.Type, Raw: m}
	switch c.Type {
	case Button:
		e.Pressed = m.Status() == 0x90 && m[2] > 0
	case Absolute:
		e.Value = float64(m[2]) / 127
	case Relative:
		v := int(m[2])
		if c.Encoding == "twos" {
			if v >= 64 {
				v -= 128
			}
		} else {
			v -= 64
		}
		e.Delta = v
	}
	return e, true, false
}

// Light is a light's level: off, on, or on at a given velocity (which picks
// a colour or brightness on surfaces that have them).
type Light struct {
	On       bool
	Velocity byte // 0 means full (0x7F)
}

// LightMsgs are the messages that set control name's light.
func (p *Profile) LightMsgs(name string, l Light) []midi.Msg {
	c := p.byName[name]
	if c == nil {
		return nil
	}
	v := byte(0)
	if l.On {
		v = l.Velocity
		if v == 0 {
			v = 0x7F
		}
	}
	var out []midi.Msg
	for _, s := range c.Lights {
		k, _ := key(s)
		out = append(out, midi.Msg{k[0], k[1], v})
	}
	return out
}

// Lit reports whether a control has a light.
func (p *Profile) Lit(name string) bool {
	c := p.byName[name]
	return c != nil && len(c.Lights) > 0
}
