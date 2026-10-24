package midi

import (
	"reflect"
	"testing"
)

func TestParser(t *testing.T) {
	var p Parser
	got := p.Feed([]byte{
		0x97, 0x00, 0x7F, // pad press
		0x00, 0x00, // running status: the same pad's release
		0xF8,                   // clock in between
		0xB0, 0x22, 0xF8, 0x41, // a jog tick split by a clock
		0xF0, 0x00, 0x40, 0x05, 0xF7, // sysex, skipped
		0xC3, 0x05, // program change: one data byte
		0x90, 0x0B, // a message cut off here...
	})
	got = append(got, p.Feed([]byte{0x7F})...) // ...and finished in the next read
	want := []Msg{{0x97, 0x00, 0x7F}, {0x97, 0x00, 0x00}, {0xB0, 0x22, 0x41}, {0xC3, 0x05, 0}, {0x90, 0x0B, 0x7F}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v\nwant %v", got, want)
	}
}

func TestMatch(t *testing.T) {
	if !Match("DDJ-FLX4 MIDI 1", "ddj-flx4") || !Match("Pioneer DDJFLX4", "DDJ FLX4") || Match("DDJ-400", "ddj-flx4") {
		t.Fatal("match")
	}
}
