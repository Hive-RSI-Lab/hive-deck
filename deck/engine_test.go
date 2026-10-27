package deck

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Hive-RSI-Lab/hive-deck/hive"
	"github.com/Hive-RSI-Lab/hive-deck/midi"
	"github.com/Hive-RSI-Lab/hive-deck/surface"
)

// fakeHive records what the deck asked of it.
type fakeHive struct {
	st     hive.State
	calls  []string
	noOp   bool
	config map[string]any
}

func (f *fakeHive) State() (hive.State, error)              { return f.st, nil }
func (f *fakeHive) Events(int64) ([]hive.Event, error)      { return nil, nil }
func (f *fakeHive) Messages(string) ([]hive.Message, error) { return nil, nil }
func (f *fakeHive) rec(format string, a ...any) error {
	f.calls = append(f.calls, fmt.Sprintf(format, a...))
	return nil
}
func (f *fakeHive) Answer(id, ans, _ string) error { return f.rec("answer %s %s", id, ans) }
func (f *fakeHive) Hold(p, _ string) error         { return f.rec("hold %s", p) }
func (f *fakeHive) Lift(p, _ string) error         { return f.rec("lift %s", p) }
func (f *fakeHive) Retry(id string) error          { return f.rec("retry %s", id) }
func (f *fakeHive) Note(text string) error         { return f.rec("note %s", text) }
func (f *fakeHive) SetConfig(change func(map[string]any)) error {
	if f.config == nil {
		f.config = map[string]any{"max_workers": float64(f.st.Config.MaxWorkers)}
	}
	change(f.config)
	raw, _ := json.Marshal(f.config)
	return f.rec("config %s", raw)
}
func (f *fakeHive) Op(name string, args map[string]any) (json.RawMessage, error) {
	if f.noOp {
		return nil, hive.ErrNoOp
	}
	raw, _ := json.Marshal(args)
	return nil, f.rec("op %s %s", name, raw)
}

func (f *fakeHive) take() []string { c := f.calls; f.calls = nil; return c }

var t0 = time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

func setup(t *testing.T, mapping string, st hive.State, dry bool) (*Engine, *fakeHive, *midi.Fake, *surface.Profile) {
	t.Helper()
	raw := DefaultMapping()
	if mapping != "" {
		raw = []byte(mapping)
	}
	m, err := ParseMapping(raw)
	if err != nil {
		t.Fatal(err)
	}
	p, err := surface.Load(m.Profile)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Check(p); err != nil {
		t.Fatal(err)
	}
	h := &fakeHive{st: st}
	e := New(m, p, h, dry)
	e.Poll()
	return e, h, midi.NewFake("DDJ-FLX4"), p
}

// press sends a pad's raw MIDI through the profile, as the device would.
func press(t *testing.T, e *Engine, p *surface.Profile, now time.Time, msgs ...midi.Msg) {
	t.Helper()
	for _, m := range msgs {
		ev, ok, ignored := p.Decode(m)
		if ignored {
			continue
		}
		if !ok {
			t.Fatalf("%s: not a control", m)
		}
		e.Handle(ev, now)
	}
}

var (
	approve = []midi.Msg{{0x97, 0x00, 0x7F}, {0x97, 0x00, 0x00}} // pad.l.1 down and up
	refuse  = []midi.Msg{{0x97, 0x01, 0x7F}, {0x97, 0x01, 0x00}}
	next    = []midi.Msg{{0x97, 0x03, 0x7F}, {0x97, 0x03, 0x00}}
	hold    = []midi.Msg{{0x97, 0x04, 0x7F}, {0x97, 0x04, 0x00}}
	lift    = []midi.Msg{{0x97, 0x05, 0x7F}, {0x97, 0x05, 0x00}}
	rerun   = []midi.Msg{{0x97, 0x06, 0x7F}, {0x97, 0x06, 0x00}}
	retry   = []midi.Msg{{0x97, 0x07, 0x7F}, {0x97, 0x07, 0x00}}
	cue     = []midi.Msg{{0x90, 0x0C, 0x7F}, {0x90, 0x0C, 0x00}}
)

func waiting() hive.State {
	return hive.State{
		Projects: []hive.Project{{Name: "deck", Tasks: []hive.Task{
			{ID: "t1", Project: "deck", Title: "the deck", Stage: "waiting-person", SHA: "abc", PR: struct{ Number int }{7}, Created: t0},
			{ID: "t0", Project: "deck", Title: "older", Stage: "blocked", Created: t0.Add(-time.Hour)},
		}}},
		Asks: []hive.Ask{
			{ID: "a1", Kind: "merge", Title: "Merge #7?", Options: []string{"merge", "leave open"}, Ref: "t1", Project: "deck"},
			{ID: "a2", Kind: "question", Title: "Step back read", Options: []string{"ok"}},
			{ID: "a3", Kind: "route", Title: "Run the reviewer seat on qwen?"},
			{ID: "done", Kind: "merge", Title: "answered already", Answer: "merge"},
		},
		Config: hive.Config{MaxWorkers: 3, Models: map[string]any{"qwen": map[string]any{}}, ModelChoices: []string{"claude:opus"}},
	}
}

func TestApproveTakesTheSecondPress(t *testing.T) {
	e, h, _, p := setup(t, "", waiting(), false)
	press(t, e, p, t0, approve...)
	if c := h.take(); len(c) != 0 {
		t.Fatalf("one press answered: %v", c)
	}
	if !strings.Contains(strings.Join(e.Log(), "\n"), `press pad.l.1 again to answer merge "Merge #7?" with "merge"`) {
		t.Fatalf("no prompt for the second press: %v", e.Log())
	}
	press(t, e, p, t0.Add(time.Second), approve...)
	if c := h.take(); len(c) != 1 || c[0] != "answer a1 merge" {
		t.Fatalf("second press: %v", c)
	}
}

func TestRefuseIsTheSecondOption(t *testing.T) {
	e, h, _, p := setup(t, "", waiting(), false)
	press(t, e, p, t0, refuse...)
	press(t, e, p, t0, refuse...)
	if c := h.take(); len(c) != 1 || c[0] != "answer a1 leave open" {
		t.Fatalf("refuse: %v", c)
	}
}

func TestConfirmLapses(t *testing.T) {
	e, h, _, p := setup(t, "", waiting(), false)
	press(t, e, p, t0, approve...)
	e.Tick(t0.Add(5 * time.Second)) // the window is 4s
	press(t, e, p, t0.Add(5*time.Second), approve...)
	if c := h.take(); len(c) != 0 {
		t.Fatalf("a press after the window answered: %v", c)
	}
	press(t, e, p, t0.Add(6*time.Second), approve...) // this one is the second of a new pair
	if c := h.take(); len(c) != 1 {
		t.Fatalf("new pair: %v", c)
	}
}

func TestAnotherControlCancelsTheConfirm(t *testing.T) {
	e, h, _, p := setup(t, "", waiting(), false)
	for name, between := range map[string][]midi.Msg{"refuse": refuse, "cue": cue, "next": next} {
		press(t, e, p, t0, approve...)
		press(t, e, p, t0, between...)
		press(t, e, p, t0, approve...)
		if c := h.take(); len(c) != 0 {
			t.Errorf("%s between the presses: answered %v", name, c)
		}
	}
}

func TestSelectionMovesTheAnswer(t *testing.T) {
	e, h, _, p := setup(t, "", waiting(), false)
	press(t, e, p, t0, next...)
	press(t, e, p, t0, next...) // a3: a route ask with no options
	press(t, e, p, t0, approve...)
	press(t, e, p, t0, approve...)
	if c := h.take(); len(c) != 1 || c[0] != "answer a3 yes" {
		t.Fatalf("got %v", c)
	}
	// the jog moves it too: 6 ticks a step, back up to a2
	press(t, e, p, t0, midi.Msg{0xB0, 0x22, 0x40 - 3}, midi.Msg{0xB0, 0x22, 0x40 - 3})
	press(t, e, p, t0, refuse...)
	if c := h.take(); len(c) != 0 || !strings.Contains(e.Log()[len(e.Log())-1], "takes only ok") {
		t.Fatalf("refusing an ok-only ask: %v %v", c, e.Log())
	}
}

func TestOneTouchNeverForWhatIsAlwaysThePersons(t *testing.T) {
	oneTouch := `{"controls": {"pad.l.1": {"do": "approve", "confirm": false}, "pad.l.4": {"do": "select", "step": 1}}}`
	e, h, _, p := setup(t, oneTouch, waiting(), false)
	press(t, e, p, t0, approve...) // a1 is a merge: always the person's
	if c := h.take(); len(c) != 0 {
		t.Fatalf("a merge answered on one touch: %v", c)
	}
	press(t, e, p, t0, next...) // a2, a plain question
	press(t, e, p, t0, approve...)
	if c := h.take(); len(c) != 1 || c[0] != "answer a2 ok" {
		t.Fatalf("a question with confirm off: %v", c)
	}
	for _, k := range []string{"merge", "step", "request", "release", "baseline", "bare", "login", "route", "install", "something-new"} {
		if !AlwaysPersons(k) {
			t.Errorf("%s should always take the confirm", k)
		}
	}
}

func TestAnsweredElsewhereDropsTheConfirm(t *testing.T) {
	e, h, _, p := setup(t, "", waiting(), false)
	press(t, e, p, t0, approve...)
	st := waiting()
	st.Asks[0].Answer = "merge" // answered in the control center
	h.st = st
	e.Poll()
	press(t, e, p, t0, approve...)
	if c := h.take(); len(c) != 0 {
		t.Fatalf("answered the next item on the first item's confirm: %v", c)
	}
}

func TestDryRunSendsNothing(t *testing.T) {
	e, h, _, p := setup(t, "", waiting(), true)
	press(t, e, p, t0, approve...)
	press(t, e, p, t0, approve...)
	press(t, e, p, t0, hold...)
	press(t, e, p, t0, midi.Msg{0xB0, 0x13, 0x7F}) // the fader to the top
	e.Tick(t0.Add(time.Second))
	if c := h.take(); len(c) != 0 {
		t.Fatalf("a dry run called the Hive: %v", c)
	}
	log := strings.Join(e.Log(), "\n")
	for _, want := range []string{`would answer merge "Merge #7?" with "merge"`, "would hold deck's merges"} {
		if !strings.Contains(log, want) {
			t.Errorf("dry run didn't say %q:\n%s", want, log)
		}
	}
}

func TestHoldLiftRerunRetry(t *testing.T) {
	e, h, _, p := setup(t, "", waiting(), false)
	press(t, e, p, t0, hold...) // one press
	press(t, e, p, t0, lift...)
	press(t, e, p, t0, lift...) // lift confirms
	press(t, e, p, t0, rerun...)
	press(t, e, p, t0, rerun...) // the selected ask's task, t1
	press(t, e, p, t0, retry...)
	press(t, e, p, t0, retry...) // the project's newest blocked task, t0
	want := []string{"hold deck", "lift deck", `op rerun_checks {"task":"t1"}`, "retry t0"}
	if c := h.take(); strings.Join(c, "|") != strings.Join(want, "|") {
		t.Fatalf("got %v\nwant %v", c, want)
	}
	h.noOp = true
	press(t, e, p, t0, rerun...)
	press(t, e, p, t0, rerun...)
	if c := h.take(); len(c) != 0 || !strings.Contains(e.Log()[len(e.Log())-1], "no /api/op") {
		t.Fatalf("rerun without the seam: %v %v", c, e.Log())
	}
}

func TestWorkerFaderPicksUpThenSettles(t *testing.T) {
	e, h, _, p := setup(t, "", waiting(), false) // the Hive has max_workers 3; the fader runs 0 to 8
	fader := func(at time.Duration, v byte) { press(t, e, p, t0.Add(at), midi.Msg{0xB0, 0x13, v}) }
	fader(0, 0)   // 0: not picked up
	fader(10, 16) // 1
	e.Tick(t0.Add(time.Second))
	if c := h.take(); len(c) != 0 {
		t.Fatalf("a fader away from the Hive's value changed it: %v", c)
	}
	fader(1100*time.Millisecond, 79) // 5: it crossed 3, so it holds now
	fader(1200*time.Millisecond, 95) // 6
	e.Tick(t0.Add(1500 * time.Millisecond))
	if c := h.take(); len(c) != 0 {
		t.Fatalf("sent before it settled: %v", c)
	}
	e.Tick(t0.Add(1900 * time.Millisecond))
	if c := h.take(); len(c) != 1 || c[0] != `op workers {"max":6}` {
		t.Fatalf("settled: %v", c)
	}
	// a Hive without /api/op: Settings' route, and a line on the journal
	h.noOp, h.st.Config.MaxWorkers = true, 6
	e.Poll()
	fader(2*time.Second, 127) // 8
	e.Tick(t0.Add(3 * time.Second))
	c := h.take()
	if len(c) != 2 || !strings.Contains(c[0], `"max_workers":8`) || c[1] != "note the person set max_workers from 6 to 8 from the deck" {
		t.Fatalf("without the op: %v", c)
	}
}

func TestRouteKnob(t *testing.T) {
	e, h, _, p := setup(t, "", waiting(), false)
	e.Poll()
	e.Poll() // ensure state read
	e.st.Config.Routes = map[string][]string{"worker": {"unknown"}}
	e.Poll()
	e.Poll()
	e, h, _, p = setup(t, "", waiting(), false)

	e, h, _, p := setup(t, "", waiting(), false)
	// choices: no route, claude:opus, qwen; the worker has no route, so the knob holds at the bottom
	press(t, e, p, t0, midi.Msg{0xB0, 0x07, 0x00}, midi.Msg{0xB0, 0x07, 0x7F})
	e.Tick(t0.Add(time.Second))
	if c := h.take(); len(c) != 1 || c[0] != `op route {"models":"qwen","role":"worker"}` {
		t.Fatalf("route: %v", c)
	}
}

func TestLightsFollowTheHive(t *testing.T) {
	st := waiting()
	st.Checks.Running = []hive.Slot{{Project: "deck", Kind: "test"}}
	st.Chats = []hive.Chat{{ID: "lead", LeadStatus: "running"}}
	e, h, port, p := setup(t, "", st, false)
	lit := func(now time.Time) map[string]bool {
		e.Flush(port, now)
		out := map[string]bool{}
		for _, m := range port.Sent() {
			ev, ok, _ := p.Decode(midi.Msg{m[0], m[1], 0x7F})
			if ok {
				out[ev.Control] = out[ev.Control] || m[2] > 0
			}
		}
		return out
	}
	first := lit(t0)
	for ctrl, on := range map[string]bool{"pad.l.1": true, "pad.l.2": true, "pad.r.1": true, "pad.r.2": false, "pad.r.7": true, "pad.r.8": true, "play.l": true, "pad.r.5": false, "pad.l.5": false, "pad.l.3": true} {
		if first[ctrl] != on {
			t.Errorf("%s: lit %v, want %v", ctrl, first[ctrl], on)
		}
	}
	if len(first) != len(e.m.Lights) {
		t.Errorf("the first flush should set every light: %d of %d", len(first), len(e.m.Lights))
	}
	if again := lit(t0.Add(10 * time.Millisecond)); len(again) != 0 {
		t.Errorf("nothing changed, yet it sent %v", again)
	}
	// main goes red, the merges are held, the gate and the lead are quiet
	st.Projects[0].Scores = []hive.Measurement{{TestsOK: true, At: t0.Add(-time.Hour)}, {TestsOK: false, At: t0}}
	st.Projects[0].Hold = &hive.Hold{Why: "release"}
	st.Checks.Running, st.Chats = nil, nil
	h.st = st
	e.Poll()
	got := lit(t0.Add(20 * time.Millisecond)) // the first half of a blink: on
	for ctrl, on := range map[string]bool{"pad.r.6": true, "pad.l.5": true, "pad.l.6": true, "pad.r.7": false, "pad.r.8": false} {
		if v, sent := got[ctrl]; !sent || v != on {
			t.Errorf("%s: sent %v lit %v, want lit %v", ctrl, sent, v, on)
		}
	}
	if b := lit(t0.Add(520 * time.Millisecond)); b["pad.r.6"] || !contains(b, "pad.r.6") {
		t.Errorf("main.red should blink off in its second half: %v", b)
	}
	// a press waiting for its confirm blinks fast
	press(t, e, p, t0, approve...)
	if a := lit(t0.Add(1125 * time.Millisecond)); a["pad.l.1"] || !contains(a, "pad.l.1") {
		t.Errorf("armed, off phase: %v", a)
	}
	if a := lit(t0.Add(1250 * time.Millisecond)); !a["pad.l.1"] || !a["cue.l"] {
		t.Errorf("armed, on phase: %v", a)
	}
}

func contains(m map[string]bool, k string) bool { _, ok := m[k]; return ok }

func TestViews(t *testing.T) {
	e, _, _, p := setup(t, "", waiting(), false)
	press(t, e, p, t0, midi.Msg{0x99, 0x02, 0x7F}) // pad.r.3
	if e.View() != "threads" {
		t.Fatalf("view %s", e.View())
	}
	if s := e.Screen(80, 20); !strings.Contains(s, "[THREADS]") {
		t.Fatalf("screen:\n%s", s)
	}
	press(t, e, p, t0, midi.Msg{0x99, 0x00, 0x7F}) // pad.r.1
	if s := e.Screen(100, 20); !strings.Contains(s, "▶ merge") || !strings.Contains(s, "[merge / leave open]") {
		t.Fatalf("waiting screen:\n%s", s)
	}
}

func TestMappingCheck(t *testing.T) {
	p, _ := surface.Builtin("ddj-flx4")
	for name, raw := range map[string]string{
		"no such control": `{"controls": {"pad.l.9": {"do": "approve"}}}`,
		"no such action":  `{"controls": {"pad.l.1": {"do": "merge-everything"}}}`,
		"wrong type":      `{"controls": {"pad.l.1": {"do": "workers"}}}`,
		"answer on a jog": `{"controls": {"jog.l": {"do": "approve"}}}`,
		"no such view":    `{"controls": {"pad.l.1": {"do": "view", "view": "kb"}}}`,
		"route, no role":  `{"controls": {"knob.l.hi": {"do": "route"}}}`,
		"unlit light":     `{"lights": {"jog.l": "asks"}}`,
		"no such signal":  `{"lights": {"pad.l.1": "disco"}}`,
	} {
		m, err := ParseMapping([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		if m.Check(p) == nil {
			t.Errorf("%s: passed", name)
		}
	}
}

// The picture in docs/ is drawn from the default mapping; regenerate it with
// UPDATE_LAYOUT=1 go test ./deck.
func TestLayoutPictureIsCurrent(t *testing.T) {
	m, _ := ParseMapping(DefaultMapping())
	p, _ := surface.Builtin(m.Profile)
	svg := SVG(p, m)
	if os.Getenv("UPDATE_LAYOUT") != "" {
		os.WriteFile("../docs/layout.svg", []byte(svg), 0o644)
	}
	raw, err := os.ReadFile("../docs/layout.svg")
	if err != nil || string(raw) != svg {
		t.Fatal("docs/layout.svg is out of date: UPDATE_LAYOUT=1 go test ./deck")
	}
}
