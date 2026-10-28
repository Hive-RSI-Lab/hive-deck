package check

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Hive-RSI-Lab/hive-deck/midi"
	"github.com/Hive-RSI-Lab/hive-deck/surface"
)

func flx4(t *testing.T) *surface.Profile {
	t.Helper()
	p, err := surface.Builtin("ddj-flx4")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// touch is the message a control sends when the person touches it: its
// first message, pressed, half way, or a tick clockwise.
func touch(t *testing.T, c surface.Control) midi.Msg {
	t.Helper()
	var m midi.Msg
	for i, f := range strings.Fields(c.Msgs[0]) {
		v, err := strconv.ParseUint(f, 16, 8)
		if err != nil {
			t.Fatal(err)
		}
		m[i] = byte(v)
	}
	switch c.Type {
	case surface.Button:
		m[2] = 0x7F
	case surface.Absolute:
		m[2] = 0x40
	default:
		m[2] = 0x41
	}
	return m
}

// check runs a check on a fake FLX4 that has already been sent msgs, with
// the person typing lines.
func check(t *testing.T, msgs []midi.Msg, lines ...string) (Result, *midi.Fake) {
	t.Helper()
	port := midi.NewFake("DDJ-FLX4 (fake)")
	port.Receive(msgs...)
	in := make(chan string, len(lines))
	for _, l := range lines {
		in <- l
	}
	r := Run(Config{Profile: flx4(t), Port: port, Lines: in, Out: io.Discard,
		Used: map[string]bool{"pad.l.1": true}, Wait: 2 * time.Second})
	return r, port
}

func everything(t *testing.T, skip string) []midi.Msg {
	var out []midi.Msg
	for _, c := range flx4(t).Controls {
		if c.Name != skip {
			out = append(out, touch(t, c))
		}
	}
	return out
}

func TestAllHeard(t *testing.T) {
	r, port := check(t, everything(t, ""), "")
	if !r.OK() || len(r.Missing()) != 0 || r.Ended != "every control heard" {
		t.Fatalf("want every control heard, got missing %v, ended %q", r.Missing(), r.Ended)
	}
	rep := r.Report()
	n := len(flx4(t).Controls)
	for _, want := range []string{fmt.Sprintf("heard %d of %d controls", n, n), "| pad.l.1 | button | yes | heard | 97 00 7F |", "every one lit", "Every control in the profile was heard."} {
		if !strings.Contains(rep, want) {
			t.Errorf("report lacks %q:\n%s", want, rep)
		}
	}
	// The walk lit pad.l.1 in every pad mode, and the check ends dark.
	sent := port.Sent()
	lit := 0
	for _, m := range sent {
		if m[0] == 0x97 && m[1]&0x0F == 0 && m[1] <= 0x70 && m[2] == 0x7F {
			lit++
		}
	}
	if lit < 8 {
		t.Errorf("pad.l.1 lit %d times, want once per pad mode", lit)
	}
	if last := sent[len(sent)-1]; last[2] != 0 {
		t.Errorf("the last message should turn a light off: %s", last)
	}
}

func TestOneMissing(t *testing.T) {
	r, _ := check(t, everything(t, "jog.r"), "", "")
	if r.OK() || strings.Join(r.Missing(), ",") != "jog.r" || r.Ended != "ended by the person" {
		t.Fatalf("want jog.r missing, ended by the person: got %v, %q", r.Missing(), r.Ended)
	}
	rep := r.Report()
	n := len(flx4(t).Controls)
	for _, want := range []string{fmt.Sprintf("heard %d of %d", n-1, n), "| jog.r | relative |  | **not heard** | expected B1 21, B1 22, B1 23 |"} {
		if !strings.Contains(rep, want) {
			t.Errorf("report lacks %q:\n%s", want, rep)
		}
	}
}

func TestHeardButUnmapped(t *testing.T) {
	p := flx4(t)
	odd := midi.Msg{0x97, 0x08, 0x7F} // SHIFT and pad 1 in hot cue mode, say: not in the profile
	if _, known, ignored := p.Decode(odd); known || ignored {
		t.Fatalf("the test needs a message the profile doesn't know")
	}
	msgs := everything(t, "")
	msgs = append(msgs[:5:5], append([]midi.Msg{odd, odd}, msgs[5:]...)...)
	r, _ := check(t, msgs, "")
	if len(r.Unmapped) != 1 || r.Unmapped[0].Count != 2 || r.Unmapped[0].After != p.Controls[4].Name {
		t.Fatalf("unmapped: %+v", r.Unmapped)
	}
	if !r.OK() {
		t.Errorf("an unknown message alone doesn't fail the check")
	}
	if rep := r.Report(); !strings.Contains(rep, "| 97 08 7F | 2 | "+p.Controls[4].Name+" |") {
		t.Errorf("report lacks the unmapped message:\n%s", rep)
	}
}

func TestDarkLights(t *testing.T) {
	r, _ := check(t, everything(t, ""), "pad.l.3, play.r")
	if r.OK() || strings.Join(r.Unlit, " ") != "pad.l.3 play.r" {
		t.Fatalf("want the two named lights reported and the check failed: %v", r.Unlit)
	}
	if !strings.Contains(r.Report(), "didn't light: pad.l.3, play.r") {
		t.Errorf("report:\n%s", r.Report())
	}
}

func TestUnplugged(t *testing.T) {
	port := midi.NewFake("DDJ-FLX4 (fake)")
	port.Close()
	r := Run(Config{Profile: flx4(t), Port: port, Lines: make(chan string), Out: io.Discard, Wait: time.Second})
	if r.Ended != "the deck went away (unplugged?)" || r.OK() {
		t.Fatalf("ended %q", r.Ended)
	}
}
