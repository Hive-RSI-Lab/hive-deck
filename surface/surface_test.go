package surface

import (
	"testing"

	"github.com/Hive-RSI-Lab/hive-deck/midi"
)

func flx4(t *testing.T) *Profile {
	t.Helper()
	p, err := Builtin("ddj-flx4")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDecodeFLX4(t *testing.T) {
	p := flx4(t)
	cases := []struct {
		msg  midi.Msg
		want string
	}{
		{midi.Msg{0x97, 0x00, 0x7F}, "pad.l.1 press"},   // hot cue mode
		{midi.Msg{0x97, 0x20, 0x7F}, "pad.l.1 press"},   // beat jump mode: the same pad
		{midi.Msg{0x97, 0x37, 0x7F}, "pad.l.8 press"},   // sampler mode
		{midi.Msg{0x97, 0x00, 0x00}, "pad.l.1 release"}, // note on, velocity 0
		{midi.Msg{0x87, 0x00, 0x40}, "pad.l.1 release"}, // note off
		{midi.Msg{0x99, 0x03, 0x7F}, "pad.r.4 press"},
		{midi.Msg{0x90, 0x0B, 0x7F}, "play.l press"},
		{midi.Msg{0xB0, 0x22, 0x42}, "jog.l +2"},
		{midi.Msg{0xB1, 0x21, 0x3D}, "jog.r -3"},
		{midi.Msg{0xB6, 0x40, 0x01}, "browse +1"},
		{midi.Msg{0xB6, 0x40, 0x7F}, "browse -1"},
		{midi.Msg{0xB0, 0x13, 0x7F}, "fader.l 1.00"},
		{midi.Msg{0xB1, 0x07, 0x00}, "knob.r.hi 0.00"},
	}
	for _, c := range cases {
		ev, ok, _ := p.Decode(c.msg)
		if !ok || ev.String() != c.want {
			t.Errorf("%s: got %q (known %v), want %q", c.msg, ev.String(), ok, c.want)
		}
	}
	if _, ok, ignored := p.Decode(midi.Msg{0xB0, 0x33, 0x10}); ok || !ignored {
		t.Errorf("a fader's low byte should be known and ignored")
	}
	if _, ok, ignored := p.Decode(midi.Msg{0x9F, 0x7F, 0x7F}); ok || ignored {
		t.Errorf("an unknown message should be unknown")
	}
}

func TestLightMsgsCoverEveryPadMode(t *testing.T) {
	p := flx4(t)
	on := p.LightMsgs("pad.r.2", Light{On: true})
	if len(on) != 8 {
		t.Fatalf("want a message per pad mode, got %v", on)
	}
	for _, m := range on {
		if m[0] != 0x99 || m[1]&0x0F != 0x01 || m[2] != 0x7F {
			t.Errorf("pad.r.2 on: %s", m)
		}
	}
	if off := p.LightMsgs("pad.r.2", Light{}); off[0][2] != 0 {
		t.Errorf("off should be velocity 0: %s", off[0])
	}
	if p.Lit("jog.l") || !p.Lit("play.r") {
		t.Errorf("jog.l has no light and play.r has one")
	}
}

func TestParseRefusesBadProfiles(t *testing.T) {
	for name, raw := range map[string]string{
		"duplicate message": `{"name":"x","controls":[{"name":"a","type":"button","msgs":["90 01"]},{"name":"b","type":"button","msgs":["90 01"]}]}`,
		"duplicate name":    `{"name":"x","controls":[{"name":"a","type":"button","msgs":["90 01"]},{"name":"a","type":"button","msgs":["90 02"]}]}`,
		"bad type":          `{"name":"x","controls":[{"name":"a","type":"slider","msgs":["90 01"]}]}`,
		"bad message":       `{"name":"x","controls":[{"name":"a","type":"button","msgs":["9001"]}]}`,
		"data as status":    `{"name":"x","controls":[{"name":"a","type":"button","msgs":["10 01"]}]}`,
	} {
		if _, err := Parse([]byte(raw)); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
}
