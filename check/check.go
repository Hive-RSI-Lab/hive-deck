// Package check is `deck check` on a real device: it walks every light the
// profile names, asks the person which didn't light, then listens while they
// touch every control, and reports each profile control as heard or not
// heard, and every message the deck sent that the profile doesn't know. It
// needs no Hive: it is the first thing to run on a new laptop.
package check

import (
	"fmt"
	"io"
	"runtime"
	"strings"
	"time"

	"github.com/Hive-RSI-Lab/hive-deck/midi"
	"github.com/Hive-RSI-Lab/hive-deck/surface"
)

// Config is one check.
type Config struct {
	Profile *surface.Profile
	Port    midi.Port
	Lines   <-chan string   // what the person types, a line at a time; closed when they can't type more
	Out     io.Writer       // the prompts and progress
	Used    map[string]bool // the controls the mapping binds or lights, to mark in the report
	Step    time.Duration   // how long each light stays on in the walk
	Wait    time.Duration   // the longest the touching part runs
}

// Unknown is a message the deck sent that the profile doesn't know.
type Unknown struct {
	Msg   midi.Msg // the first one heard (status and number are what count)
	Count int
	After string // the profile control heard just before it, if any: often the one with the wrong number
}

// Result is what the check found.
type Result struct {
	Profile, Device string
	At              time.Time
	Heard           map[string][]midi.Msg // control -> the distinct messages it was heard by
	Unmapped        []Unknown
	Lights          int      // lights walked
	LightsAnswered  bool     // the person answered whether every light lit
	LightsAnswer    string   // what they said
	Unlit           []string // the lights the person named as not lighting
	LightsBad       bool     // the person said some didn't light
	Ended           string
	controls        []surface.Control
	used            map[string]bool
}

// Run runs the check and turns every light off after.
func Run(c Config) Result {
	p := c.Profile
	r := Result{Profile: p.Name, Device: c.Port.Name(), At: time.Now(), Heard: map[string][]midi.Msg{},
		controls: p.Controls, used: c.Used}
	unknown := map[[2]byte]int{} // index into r.Unmapped
	last := ""
	hear := func(m midi.Msg) {
		ev, known, ignored := p.Decode(m)
		switch {
		case known:
			last = ev.Control
			seen := r.Heard[ev.Control]
			first := len(seen) == 0
			if !hasKey(seen, m) {
				r.Heard[ev.Control] = append(seen, m)
			}
			if first {
				left := r.Missing()
				fmt.Fprintf(c.Out, "heard %-16s %s  (%d left)\n", ev.Control, m, len(left))
				if n := len(left); n > 0 && n <= 8 {
					fmt.Fprintf(c.Out, "  left: %s\n", strings.Join(left, ", "))
				}
				for _, l := range p.LightMsgs(ev.Control, surface.Light{}) {
					c.Port.Send(l)
				}
			}
		case ignored:
		default:
			k := key(m)
			if i, ok := unknown[k]; ok {
				r.Unmapped[i].Count++
				return
			}
			unknown[k] = len(r.Unmapped)
			r.Unmapped = append(r.Unmapped, Unknown{Msg: m, Count: 1, After: last})
			fmt.Fprintf(c.Out, "heard %s: not in the profile\n", m)
		}
	}
	drain := func() bool {
		for {
			select {
			case m, ok := <-c.Port.In():
				if !ok {
					return false
				}
				hear(m)
			default:
				return true
			}
		}
	}
	gone := false
	defer func() {
		if !gone {
			for _, ctl := range p.Controls {
				for _, l := range p.LightMsgs(ctl.Name, surface.Light{}) {
					c.Port.Send(l)
				}
			}
		}
	}()

	// The walk: each light on alone, in the profile's order.
	var lit []string
	for _, ctl := range p.Controls {
		if p.Lit(ctl.Name) {
			lit = append(lit, ctl.Name)
		}
	}
	r.Lights = len(lit)
	fmt.Fprintf(c.Out, "checking the %s on %s: %d controls, %d lights\n\n", p.Name, r.Device, len(p.Controls), len(lit))
	fmt.Fprintf(c.Out, "1. the lights: watch the deck; each one lights alone, in turn\n")
	for _, name := range lit {
		fmt.Fprintf(c.Out, "  %s\n", name)
		for _, l := range p.LightMsgs(name, surface.Light{On: true}) {
			c.Port.Send(l)
		}
		if !pause(c.Step, drain) {
			gone = true
			r.Ended = "the deck went away (unplugged?)"
			return r
		}
		for _, l := range p.LightMsgs(name, surface.Light{}) {
			c.Port.Send(l)
		}
	}
	fmt.Fprintf(c.Out, "did every one light? Enter for yes, or type the ones that didn't (such as \"pad.l.3 play.r\", or \"no\"): ")
	select {
	case line, ok := <-c.Lines:
		if ok {
			r.answerLights(p, line)
		}
	case <-time.After(c.Wait):
	}
	fmt.Fprintln(c.Out)

	// The touching: every unheard lit control stays lit until it's heard.
	for _, name := range lit {
		if len(r.Heard[name]) == 0 {
			for _, l := range p.LightMsgs(name, surface.Light{On: true}) {
				c.Port.Send(l)
			}
		}
	}
	fmt.Fprintf(c.Out, "2. the controls: press every pad and button, turn both jogs and every knob, move every fader.\n")
	fmt.Fprintf(c.Out, "   A lit control goes dark once it's heard. Enter ends early; %s at most.\n", c.Wait.Round(time.Second))
	if left := r.Missing(); len(left) > 0 {
		fmt.Fprintf(c.Out, "   to touch: %s\n", strings.Join(left, ", "))
	}
	timeout := time.After(c.Wait)
	lines := c.Lines
	for len(r.Missing()) > 0 {
		select {
		case m, ok := <-c.Port.In():
			if !ok {
				gone = true
				r.Ended = "the deck went away (unplugged?)"
				return r
			}
			hear(m)
		case _, ok := <-lines:
			if !ok {
				lines = nil // nothing more to read; listen until the time runs out
				continue
			}
			if !drain() {
				gone = true
				r.Ended = "the deck went away (unplugged?)"
				return r
			}
			r.Ended = "ended by the person"
			if len(r.Missing()) == 0 {
				r.Ended = "every control heard"
			}
			return r
		case <-timeout:
			if !drain() {
				gone = true
			}
			r.Ended = "the time ran out"
			return r
		}
	}
	r.Ended = "every control heard"
	return r
}

// pause waits d, reading the deck as it goes; false if the deck went away.
func pause(d time.Duration, drain func() bool) bool {
	end := time.Now().Add(d)
	for {
		if !drain() {
			return false
		}
		left := time.Until(end)
		if left <= 0 {
			return true
		}
		time.Sleep(min(left, 20*time.Millisecond))
	}
}

func (r *Result) answerLights(p *surface.Profile, line string) {
	r.LightsAnswered, r.LightsAnswer = true, strings.TrimSpace(line)
	switch strings.ToLower(r.LightsAnswer) {
	case "", "y", "yes":
		return
	}
	r.LightsBad = true
	for _, f := range strings.FieldsFunc(r.LightsAnswer, func(c rune) bool { return c == ' ' || c == ',' }) {
		if p.Lit(f) {
			r.Unlit = append(r.Unlit, f)
		}
	}
}

// Missing are the profile's controls never heard, in the profile's order.
func (r Result) Missing() []string {
	var out []string
	for _, c := range r.controls {
		if len(r.Heard[c.Name]) == 0 {
			out = append(out, c.Name)
		}
	}
	return out
}

// OK is true when every control was heard and no light was named as dark.
// A message the profile doesn't know is reported but doesn't fail the check:
// SHIFT with a pad, say, sends notes the deck has no use for.
func (r Result) OK() bool {
	return len(r.Missing()) == 0 && !r.LightsBad
}

// Report is the result as markdown, to paste into the deck's chat.
func (r Result) Report() string {
	var b strings.Builder
	missing := r.Missing()
	fmt.Fprintf(&b, "## deck check: %s\n\n", r.Profile)
	fmt.Fprintf(&b, "- device: %s\n- when: %s, on %s/%s\n- ended: %s\n", r.Device, r.At.Format("2006-01-02 15:04"), runtime.GOOS, runtime.GOARCH, r.Ended)
	fmt.Fprintf(&b, "- heard %d of %d controls; %d not heard; %d messages heard but not in the profile\n",
		len(r.controls)-len(missing), len(r.controls), len(missing), len(r.Unmapped))
	switch {
	case !r.LightsAnswered:
		fmt.Fprintf(&b, "- lights: %d walked; no answer on whether they lit\n", r.Lights)
	case !r.LightsBad:
		fmt.Fprintf(&b, "- lights: %d walked; every one lit\n", r.Lights)
	case len(r.Unlit) > 0:
		fmt.Fprintf(&b, "- lights: %d walked; didn't light: %s\n", r.Lights, strings.Join(r.Unlit, ", "))
	default:
		fmt.Fprintf(&b, "- lights: %d walked; the person said: %q\n", r.Lights, r.LightsAnswer)
	}
	b.WriteString("\n| control | type | in the mapping | heard | by |\n|---|---|---|---|---|\n")
	for _, c := range r.controls {
		heard, by := "**not heard**", "expected "+strings.Join(c.Msgs, ", ")
		if ms := r.Heard[c.Name]; len(ms) > 0 {
			heard = "heard"
			var s []string
			for _, m := range ms {
				s = append(s, m.String())
			}
			by = strings.Join(s, ", ")
		}
		used := ""
		if r.used[c.Name] {
			used = "yes"
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s |\n", c.Name, c.Type, used, heard, by)
	}
	if len(r.Unmapped) > 0 {
		b.WriteString("\n### Heard but not in the profile\n\n| message | times | heard after |\n|---|---|---|\n")
		for _, x := range r.Unmapped {
			fmt.Fprintf(&b, "| %s | %d | %s |\n", x.Msg, x.Count, x.After)
		}
	}
	if r.OK() {
		b.WriteString("\nEvery control in the profile was heard.\n")
	}
	return b.String()
}

func key(m midi.Msg) [2]byte {
	s := m[0]
	if s&0xF0 == 0x80 {
		s = 0x90 | s&0x0F
	}
	return [2]byte{s, m[1]}
}

func hasKey(ms []midi.Msg, m midi.Msg) bool {
	for _, x := range ms {
		if key(x) == key(m) {
			return true
		}
	}
	return false
}
