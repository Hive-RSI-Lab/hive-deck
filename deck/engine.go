// Package deck is the action layer: what a control does to the Hive and what
// a light shows. It reads named controls from package surface and calls the
// Hive through package hive, so a new surface needs a profile, not new code.
package deck

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/Hive-RSI-Lab/hive-deck/hive"
	"github.com/Hive-RSI-Lab/hive-deck/midi"
	"github.com/Hive-RSI-Lab/hive-deck/surface"
)

// Engine holds the deck's state: the Hive as last read, what is selected,
// a press waiting for its confirm, faders not yet settled, and the lights.
type Engine struct {
	m   *Mapping
	p   *surface.Profile
	h   hive.Hive
	dry bool
	// Out gets every line the deck says (what a control did, or would do);
	// nil keeps them only for the screen.
	Out func(string)

	st        hive.State
	connected bool
	lastErr   string
	polled    bool

	view    string
	sel     map[string]int // per view
	selAsk  string         // the selected Waiting item, kept across refreshes by id
	project string
	scroll  int
	jog     map[string]int // ticks not yet a step

	armed *arm

	feed   []hive.Event
	after  int64
	thread []hive.Message
	chat   string

	// The control center the deck drives: where it last said it was, whether
	// this Hive has /api/ui, and the last command sent that it hasn't shown yet.
	ui       *hive.UIView
	noUI     bool
	saidNoUI bool
	uiWant   int64     // the seq of the last move sent
	uiSentAt time.Time // when it was sent
	uiView   string    // the view it asked for, lit until the control center shows it
	uiStale  bool      // a move went unshown: no control center is following
	now      time.Time

	// The outbox, while the loop runs the engine (Run): moves waiting to be
	// sent, merged where they add up, and the one on its way.
	async  bool
	outbox []sending
	flight *sending

	lagging []lagging // moves the Hive took and the control center hasn't shown
	lags    []Lag     // the last moves' times, press to lights

	pending map[string]pend // a fader or knob's value waiting to settle
	holds   map[string]int  // the value the Hive holds that the control has picked up
	lastPos map[string]int  // each fader and knob's last position, for picking up
	told    map[string]bool // said where to move it to pick up

	log  []string
	lit  map[string]surface.Light
	sent bool
}

type arm struct {
	control, key, desc string
	at                 time.Time
}

type pend struct {
	value int
	at    time.Time
}

// New makes an engine; dry makes every action say what it would do instead.
func New(m *Mapping, p *surface.Profile, h hive.Hive, dry bool) *Engine {
	return &Engine{m: m, p: p, h: h, dry: dry, view: "waiting", sel: map[string]int{}, jog: map[string]int{},
		pending: map[string]pend{}, holds: map[string]int{}, lastPos: map[string]int{}, told: map[string]bool{}, lit: map[string]surface.Light{}}
}

func (e *Engine) say(s string) {
	e.log = append(e.log, s)
	if len(e.log) > 8 {
		e.log = e.log[len(e.log)-8:]
	}
	if e.Out != nil {
		e.Out(s)
	}
}

// Log is what the deck said last, oldest first.
func (e *Engine) Log() []string { return slices.Clone(e.log) }

// View is the current view.
func (e *Engine) View() string { return e.view }

// Poll reads the Hive's state and the journal since the last poll.
func (e *Engine) Poll() {
	e.Apply(e.Reader()())
	if !e.async {
		e.PollUI()
	}
}

// Read is one read of the Hive: its state, the journal since the last read
// and, on the threads view, the selected chat.
type Read struct {
	State  hive.State
	Err    error
	Events []hive.Event
	Chat   string
	Thread []hive.Message
	sel    int // the chat's row, clamped to the chats read
	asked  int // the threads selection when the read began
	thread bool
}

// Reader is the next read, to run off the deck's loop: it touches only the
// Hive, so a press never waits on a slow one. Apply takes its result.
func (e *Engine) Reader() func() Read {
	h, after, thread, sel := e.h, e.after, e.view == "threads", e.sel["threads"]
	return func() Read {
		r := Read{thread: thread, asked: sel}
		if r.State, r.Err = h.State(); r.Err != nil {
			return r
		}
		r.Events, _ = h.Events(after)
		if chats := r.State.Chats; thread && len(chats) > 0 {
			r.sel = min(max(sel, 0), len(chats)-1)
			r.Chat = chats[r.sel].ID
			var err error
			if r.Thread, err = h.Messages(r.Chat); err != nil {
				r.Chat = ""
			}
		}
		return r
	}
}

// Apply takes a read of the Hive.
func (e *Engine) Apply(r Read) {
	if r.Err != nil {
		if e.connected || !e.polled {
			e.say("can't reach the Hive: " + r.Err.Error())
		}
		e.connected, e.lastErr, e.polled = false, r.Err.Error(), true
		return
	}
	if !e.connected && e.polled {
		e.say("the Hive answers again")
	}
	e.connected, e.lastErr, e.polled = true, "", true
	e.SetState(r.State)
	for _, ev := range r.Events {
		if ev.Seq <= e.after {
			continue // a read that crossed one already taken
		}
		e.after = ev.Seq
		e.feed = append(e.feed, ev)
	}
	if len(e.feed) > 500 {
		e.feed = e.feed[len(e.feed)-500:]
	}
	if e.view == "threads" {
		if !r.thread {
			e.loadThread()
		} else if r.Chat != "" && e.sel["threads"] == r.asked {
			// A jog during the read moved on and loaded its own chat: keep it.
			if r.Chat != e.chat {
				e.scroll = 0
			}
			e.sel["threads"], e.thread, e.chat = r.sel, r.Thread, r.Chat
		}
	}
	e.noUI = false // a Hive updated since says so again
}

// PollUI reads where the control center is. The deck drives it while one is
// open; with none, or a Hive without /api/ui, it moves its own view.
func (e *Engine) PollUI() {
	if read := e.UIReader(); read != nil {
		v, err := read()
		e.ApplyUI(v, err, time.Now())
	}
}

// UIReader is the next read of the control center's view, to run off the
// deck's loop; nil when there is none to read. In the loop it doesn't wait
// on a read of the state: the view is cheap to read when the state is slow.
func (e *Engine) UIReader() func() (*hive.UIView, error) {
	if e.noUI || !e.connected && !e.async {
		return nil
	}
	return e.h.UIView
}

// ApplyUI takes a read of the control center's view, made at now.
func (e *Engine) ApplyUI(v *hive.UIView, err error, now time.Time) {
	if errors.Is(err, hive.ErrNoUI) {
		if e.ui != nil || !e.saidNoUI {
			e.say(err.Error())
			e.saidNoUI = true
		}
		e.noUI, e.ui = true, nil
		return
	}
	if err == nil {
		e.SetUI(v)
		e.shown(now)
	}
}

// SetUI takes the control center's view as it reported it: the deck's own
// view and Waiting selection follow it, and a confirm armed on an item the
// control center has moved off is dropped.
func (e *Engine) SetUI(v *hive.UIView) {
	was := e.Driving()
	if v != nil && (e.ui == nil || v.At.After(e.ui.At)) {
		e.uiStale = false // a control center moved: one is open
	}
	e.ui = v
	if e.Driving() != was {
		if was {
			e.say("no control center is following: the deck moves its own view")
		} else {
			e.say(strings.TrimSpace("driving the control center " + v.By))
		}
	}
	if !e.Driving() {
		return
	}
	if own := fromControl(e.uiShown()); own != "" && own != e.view {
		e.setView(own)
	}
	if e.armed != nil && strings.HasPrefix(e.armed.key, "answer:") {
		if a, ok := e.selected(); !ok || a.ID != strings.SplitN(e.armed.key, ":", 3)[1] {
			e.say("cancelled: " + e.armed.desc + " (the control center moved off it)")
			e.armed = nil
		}
	}
}

// Driving reports whether the deck drives an open control center rather
// than its own view.
func (e *Engine) Driving() bool { return e.ui != nil && e.ui.View != "" && !e.uiStale }

// caughtUp is whether the control center has shown every move the deck sent,
// so its selection is the one the person sees.
func (e *Engine) caughtUp() bool {
	return e.ui != nil && e.flight == nil && len(e.outbox) == 0 && e.ui.Applied >= e.uiWant
}

// uiShown is the view the control center shows, or the one the deck just
// asked for until it does.
func (e *Engine) uiShown() string {
	if e.uiView != "" && !e.caughtUp() {
		return e.uiView
	}
	return e.ui.View
}

// toControl is a view as the control center names it.
func toControl(v string) string {
	switch v {
	case "feed":
		return "dashboard"
	case "runs":
		return "agents"
	}
	return v
}

// fromControl is the deck's own view for one of the control center's, ""
// for one the deck's screen doesn't have.
func fromControl(v string) string {
	switch v {
	case "dashboard":
		return "feed"
	case "agents":
		return "runs"
	case "chats", "threads", "thread":
		return "threads"
	case "waiting":
		return "waiting"
	}
	return ""
}

// steer sends a command to the control center, as the keys it stands for.
// Inline, it waits for the Hive to take it; run by the loop (Run), it goes
// in the outbox the loop sends in order, and the deck carries on at once.
func (e *Engine) steer(ctrl string, c hive.UICommand, desc string) {
	c.Client, c.Control = "deck", ctrl
	if e.armed != nil && c.Action != "answer" {
		e.say("cancelled: " + e.armed.desc)
		e.armed = nil
	}
	if e.dry {
		e.say("would " + desc + " in the control center")
		return
	}
	s := sending{c: c, desc: desc, at: e.now}
	if e.async {
		e.queue(s)
		e.asked(c)
		return
	}
	start := time.Now()
	seq, err := e.h.UI(c)
	if e.posted(s, seq, err, e.now.Add(time.Since(start))) {
		e.asked(c)
	}
}

// sending is a command on its way to the control center.
type sending struct {
	c    hive.UICommand
	desc string
	at   time.Time // the press
}

// steps are the moves that add up: two jog steps down are one move of two.
var steps = map[string]bool{"up": true, "down": true, "pageup": true, "pagedown": true}

// queue puts a command in the outbox; a step behind one the same way that
// hasn't gone yet joins it, so a fast jog is one request, not twenty.
func (e *Engine) queue(s sending) {
	if n := len(e.outbox); n > 0 {
		last := &e.outbox[n-1]
		if steps[s.c.Action] && last.c.Action == s.c.Action && max(last.c.N, 1)+max(s.c.N, 1) <= 100 {
			last.c.N = max(last.c.N, 1) + max(s.c.N, 1)
			last.c.Control = s.c.Control
			last.desc = fmt.Sprintf("%s ×%d", s.c.Action, last.c.N)
			return
		}
	}
	e.outbox = append(e.outbox, s)
}

// asked is what a command does on the deck before the control center shows
// it: a view pad lights its view at once.
func (e *Engine) asked(c hive.UICommand) {
	e.uiView = ""
	if c.Action == "view" {
		e.uiView = c.View
		if own := fromControl(c.View); own != "" {
			e.setView(own)
		}
	}
}

// Next is the next command for the loop to send, nil when none waits or one
// is still on its way; Sent takes its answer.
func (e *Engine) Next() *hive.UICommand {
	if e.flight != nil || len(e.outbox) == 0 {
		return nil
	}
	s := e.outbox[0]
	e.outbox = e.outbox[1:]
	e.flight = &s
	return &s.c
}

// Sent takes the Hive's answer to the command Next gave, at now.
func (e *Engine) Sent(seq int64, err error, now time.Time) {
	if e.flight == nil {
		return
	}
	s := *e.flight
	e.flight = nil
	if !e.posted(s, seq, err, now) && s.c.Action == "view" && e.uiView == s.c.View {
		e.uiView = ""
	}
}

// posted takes the Hive's answer to a command, and says whether it took it.
func (e *Engine) posted(s sending, seq int64, err error, now time.Time) bool {
	switch {
	case errors.Is(err, hive.ErrNoUI):
		e.noUI, e.ui = true, nil
		e.outbox = nil
		e.say(err.Error())
		return false
	case err != nil:
		e.say(s.desc + ": " + err.Error())
		return false
	}
	e.uiWant, e.uiSentAt = seq, now // a restarted daemon counts from 1 again
	e.lagging = slices.DeleteFunc(e.lagging, func(l lagging) bool { return l.seq >= seq })
	e.lagging = append(e.lagging, lagging{seq: seq, at: s.at, sent: now.Sub(s.at)})
	return true
}

// SetState takes a fresh read of the Hive: the selection follows its item,
// and a confirm whose item is gone is dropped.
func (e *Engine) SetState(st hive.State) {
	e.st, e.connected = st, true
	if e.project == "" || !slices.ContainsFunc(st.Projects, func(p hive.Project) bool { return p.Name == e.project }) {
		e.project = ""
		if len(st.Projects) > 0 {
			e.project = st.Projects[0].Name
		}
	}
	e.selected()
	if e.armed != nil && strings.HasPrefix(e.armed.key, "answer:") {
		id := strings.SplitN(e.armed.key, ":", 3)[1]
		if !slices.ContainsFunc(st.Pending(), func(a hive.Ask) bool { return a.ID == id }) {
			e.armed = nil
			e.say("the item waiting for the confirm was answered elsewhere")
		}
	}
}

// selected is the selected Waiting item: the control center's while the
// deck drives one, and none while it hasn't shown the deck's last move.
func (e *Engine) selected() (hive.Ask, bool) {
	if e.Driving() {
		return e.uiAsk()
	}
	asks := e.st.Pending()
	if len(asks) == 0 {
		e.selAsk = ""
		return hive.Ask{}, false
	}
	for i, a := range asks {
		if a.ID == e.selAsk {
			e.sel["waiting"] = i
			return a, true
		}
	}
	i := min(max(e.sel["waiting"], 0), len(asks)-1)
	e.sel["waiting"], e.selAsk = i, asks[i].ID
	return asks[i], true
}

func (e *Engine) uiAsk() (hive.Ask, bool) {
	v := e.ui
	if !e.caughtUp() || v.View != "waiting" || v.Item == "" {
		return hive.Ask{}, false
	}
	for _, a := range e.st.Pending() {
		if a.ID == v.Item {
			e.selAsk = a.ID
			return a, true
		}
	}
	if v.Detail != "" {
		return hive.Ask{}, false
	}
	// Not in the deck's read of the Hive yet. Its kind is unknown, so it
	// takes the second press whatever the mapping says.
	return hive.Ask{ID: v.Item, Title: v.Title, Options: v.Options}, true
}

// Handle acts on one control's event.
func (e *Engine) Handle(ev surface.Event, now time.Time) {
	e.now = now
	b, ok := e.m.Controls[ev.Control]
	if !ok {
		if ev.Type != surface.Button || ev.Pressed {
			e.say(ev.String() + ": not mapped")
		}
		return
	}
	switch ev.Type {
	case surface.Button:
		if !ev.Pressed {
			return
		}
		if e.armed != nil && e.armed.control != ev.Control {
			e.say("cancelled: " + e.armed.desc)
			e.armed = nil
			if b.Do == "cancel" {
				return
			}
		}
		e.press(ev.Control, b, now)
	case surface.Relative:
		per := b.Per
		if per <= 0 {
			per = 4
		}
		e.jog[ev.Control] += ev.Delta
		steps := e.jog[ev.Control] / per
		e.jog[ev.Control] %= per
		if steps != 0 {
			e.move(ev.Control, b, steps)
		}
	case surface.Absolute:
		e.absolute(ev.Control, b, ev.Value, now)
	}
}

func (e *Engine) press(ctrl string, b Binding, now time.Time) {
	switch b.Do {
	case "approve", "refuse", "answer":
		e.answer(ctrl, b, now)
	case "select", "scroll", "project", "page", "pick":
		step := b.Step
		if step == 0 {
			step = 1
		}
		e.move(ctrl, b, step)
	case "view":
		if e.Driving() {
			e.steer(ctrl, hive.UICommand{Action: "view", View: toControl(b.View)}, "show "+toControl(b.View))
			return
		}
		own := fromControl(toControl(b.View))
		if own == "" {
			e.say("no control center is open to show " + b.View + ": start hive")
			return
		}
		e.setView(own)
	case "open", "back":
		if !e.Driving() {
			e.say("no control center is open to " + b.Do + " in: start hive")
			return
		}
		e.steer(ctrl, hive.UICommand{Action: b.Do}, b.Do)
	case "hold", "lift":
		if e.project == "" {
			e.say("no project to " + b.Do)
			return
		}
		p := e.project
		if b.Do == "hold" {
			e.confirmThen(ctrl, need(b, false), "hold:"+p, "hold "+p+"'s merges", now, func() error { return e.h.Hold(p, "held from the deck") })
		} else {
			e.confirmThen(ctrl, need(b, false), "lift:"+p, "lift the hold on "+p+"'s merges", now, func() error { return e.h.Lift(p, "lifted from the deck") })
		}
	case "rerun":
		t, ok := e.task(func(t hive.Task) bool { return t.SHA != "" && t.PR.Number > 0 && !slices.Contains(closed, t.Stage) })
		if !ok {
			e.say("no task with an open pull request to re-run checks on" + e.where())
			return
		}
		e.confirmThen(ctrl, need(b, false), "rerun:"+t.ID, fmt.Sprintf("re-run the failed checks on %s (%s)", t.ID, clip(t.Title, 40)), now, func() error {
			_, err := e.h.Op("rerun_checks", map[string]any{"task": t.ID})
			if errors.Is(err, hive.ErrNoOp) {
				return errors.New("this Hive has no /api/op yet, which re-running checks as the person needs; retry works today")
			}
			return err
		})
	case "retry":
		t, ok := e.task(func(t hive.Task) bool { return t.Stage == "blocked" || t.Stage == "stopped" })
		if !ok {
			e.say("no blocked or stopped task to retry" + e.where())
			return
		}
		e.confirmThen(ctrl, need(b, false), "retry:"+t.ID, fmt.Sprintf("retry %s (%s)", t.ID, clip(t.Title, 40)), now, func() error { return e.h.Retry(t.ID) })
	case "cancel":
		if e.armed == nil {
			e.say("nothing to cancel")
		} else {
			e.say("cancelled: " + e.armed.desc)
			e.armed = nil
		}
	case "refresh":
		e.Poll()
		e.say("read the Hive")
	case "nothing":
	}
}

var closed = []string{"merged", "done", "stopped"}

func (e *Engine) where() string {
	if e.project == "" {
		return ""
	}
	return " in " + e.project
}

// AlwaysPersons reports whether an answer to an ask of this kind must take
// the confirm whatever the mapping says: every kind but a plain question,
// and any kind the deck doesn't know yet.
func AlwaysPersons(kind string) bool { return kind != "question" }

// need is whether a press waits for a second; answer is true for answers,
// which confirm by default and always for what is always the person's.
func need(b Binding, answer bool) bool {
	if b.Confirm != nil {
		return *b.Confirm
	}
	return answer
}

func (e *Engine) answer(ctrl string, b Binding, now time.Time) {
	a, ok := e.selected()
	switch {
	case !ok && e.Driving() && !e.caughtUp():
		e.say("the control center hasn't shown the last move yet: press again once it has")
		return
	case !ok && e.Driving():
		e.say("select a Waiting item in the control center first")
		return
	case !ok:
		e.say("nothing is waiting")
		return
	}
	var ans string
	if b.Do == "answer" {
		if ans, ok = e.picked(a); !ok {
			e.say("pick an answer in the control center first")
			return
		}
	} else if ans, ok = Option(a, b.Do == "approve"); !ok {
		e.say(fmt.Sprintf("%q takes only %s: approve it", clip(a.Title, 50), strings.Join(a.Options, " or ")))
		return
	}
	confirm := need(b, true) || AlwaysPersons(a.Kind)
	desc := fmt.Sprintf("answer %s %q with %q", a.Kind, clip(a.Title, 50), ans)
	e.confirmThen(ctrl, confirm, "answer:"+a.ID+":"+ans, desc, now, func() error { return e.h.Answer(a.ID, ans, "answered from the deck") })
}

// picked is the answer picked for a in the control center.
func (e *Engine) picked(a hive.Ask) (string, bool) {
	if !e.Driving() || e.ui.Item != a.ID || e.ui.Option < 0 || e.ui.Option >= len(e.ui.Options) {
		return "", false
	}
	return e.ui.Options[e.ui.Option], true
}

// Option is the answer approve or refuse gives an ask: its first or second
// option, or yes and no for an ask with none. A single option has no refuse.
func Option(a hive.Ask, approve bool) (string, bool) {
	switch {
	case len(a.Options) == 0 && approve:
		return "yes", true
	case len(a.Options) == 0:
		return "no", true
	case approve:
		return a.Options[0], true
	case len(a.Options) >= 2:
		return a.Options[1], true
	}
	return "", false
}

// confirmThen runs fn, or with confirm, arms on the first press and runs
// on a second press of the same control within the window.
func (e *Engine) confirmThen(ctrl string, confirm bool, key, desc string, now time.Time, fn func() error) {
	window := time.Duration(e.m.ConfirmSeconds * float64(time.Second))
	if confirm && (e.armed == nil || e.armed.key != key || e.armed.control != ctrl || now.Sub(e.armed.at) > window) {
		e.armed = &arm{control: ctrl, key: key, desc: desc, at: now}
		e.say(fmt.Sprintf("press %s again to %s", ctrl, desc))
		return
	}
	e.armed = nil
	e.run(desc, fn)
}

func (e *Engine) run(desc string, fn func() error) bool {
	if e.dry {
		e.say("would " + desc)
		return true
	}
	if err := fn(); err != nil {
		e.say(desc + ": " + err.Error())
		return false
	}
	e.say(desc + ": done")
	e.Poll()
	return true
}

// task is the selected Waiting item's task if it has one and fits, else the
// project's newest task that fits.
func (e *Engine) task(fits func(hive.Task) bool) (hive.Task, bool) {
	var all []hive.Task
	for _, p := range e.st.Projects {
		all = append(all, p.Tasks...)
	}
	var ids []string
	if e.Driving() && e.caughtUp() {
		ids = append(ids, e.ui.Item) // a task or run selected in the control center
	}
	if a, ok := e.selected(); ok && (e.Driving() || e.view == "waiting") {
		ids = append(ids, a.Ref)
	}
	for _, id := range ids {
		for _, t := range all {
			if id != "" && t.ID == id && fits(t) {
				return t, true
			}
		}
	}
	var best hive.Task
	found := false
	for _, t := range all {
		if t.Project == e.project && fits(t) && (!found || t.Created.After(best.Created)) {
			best, found = t, true
		}
	}
	return best, found
}

func (e *Engine) setView(v string) {
	if e.view != v {
		e.view, e.scroll = v, 0
	}
	if v == "threads" {
		e.loadThread()
	}
}

func (e *Engine) loadThread() {
	chats := e.st.Chats
	if len(chats) == 0 {
		e.thread, e.chat = nil, ""
		return
	}
	i := min(max(e.sel["threads"], 0), len(chats)-1)
	e.sel["threads"] = i
	id := chats[i].ID
	ms, err := e.h.Messages(id)
	if err != nil {
		return
	}
	if id != e.chat {
		e.scroll = 0
	}
	e.thread, e.chat = ms, id
}

// move is a jog's or a pad's step: the selection, the scroll, or the
// project. While the deck drives the control center, it moves that.
func (e *Engine) move(ctrl string, b Binding, steps int) {
	if b.Do != "project" && e.Driving() {
		n := min(abs(steps), 100)
		var c hive.UICommand
		switch b.Do {
		case "select":
			c = hive.UICommand{Action: dir(steps, "up", "down"), N: n}
		case "scroll", "page":
			c = hive.UICommand{Action: dir(steps, "pageup", "pagedown"), N: n}
		case "pick":
			c = hive.UICommand{Action: dir(steps, "left", "right")}
		}
		desc := c.Action
		if c.N > 1 {
			desc = fmt.Sprintf("%s ×%d", c.Action, c.N)
		}
		e.steer(ctrl, c, desc)
		return
	}
	switch b.Do {
	case "pick":
		e.say("no control center is open to pick an answer in: start hive")
	case "page":
		e.move(ctrl, Binding{Do: "select"}, steps*10)
	case "project":
		names := e.projects()
		if len(names) == 0 {
			return
		}
		i := (slices.Index(names, e.project) + steps) % len(names)
		if i < 0 {
			i += len(names)
		}
		e.project = names[i]
		e.say("project: " + e.project)
	case "scroll":
		e.scroll = max(0, e.scroll+steps)
	case "select":
		n := e.count()
		if e.view == "feed" || n == 0 {
			e.scroll = max(0, e.scroll+steps)
			return
		}
		i := min(max(e.sel[e.view]+steps, 0), n-1)
		if i == e.sel[e.view] {
			return
		}
		e.sel[e.view], e.scroll = i, 0
		if e.armed != nil {
			e.say("cancelled: " + e.armed.desc)
			e.armed = nil
		}
		switch e.view {
		case "waiting":
			e.selAsk = e.st.Pending()[i].ID
		case "threads":
			e.loadThread()
		}
	}
}

func dir(steps int, neg, pos string) string {
	if steps < 0 {
		return neg
	}
	return pos
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

func (e *Engine) count() int {
	switch e.view {
	case "waiting":
		return len(e.st.Pending())
	case "threads":
		return len(e.st.Chats)
	case "runs":
		return len(e.liveRuns())
	}
	return 0
}

func (e *Engine) liveRuns() []hive.Run {
	var out []hive.Run
	for _, r := range e.st.Runs {
		if !slices.Contains([]string{"done", "stopped", "blocked"}, r.Status) {
			out = append(out, r)
		}
	}
	return out
}

func (e *Engine) projects() []string {
	var out []string
	for _, p := range e.st.Projects {
		out = append(out, p.Name)
	}
	return out
}

// absolute is a fader or knob moving. It takes over softly: until it passes
// the value the Hive holds, it changes nothing, so a fader left somewhere
// else never jumps the setting.
func (e *Engine) absolute(ctrl string, b Binding, v float64, now time.Time) {
	pos, cur, ok := e.position(ctrl, b, v)
	if !ok {
		return
	}
	prev, had := e.lastPos[ctrl]
	e.lastPos[ctrl] = pos
	if h, held := e.holds[ctrl]; !held || h != cur {
		crossed := had && (prev-cur)*(pos-cur) < 0
		if cur >= 0 && pos != cur && !crossed {
			if !e.told[ctrl] {
				e.told[ctrl] = true
				e.say(fmt.Sprintf("%s is at %s and the Hive has %s: move it there to pick it up", ctrl, e.label(b, pos), e.label(b, cur)))
			}
			return
		}
		e.holds[ctrl], e.told[ctrl] = cur, false
	}
	if pos == cur {
		delete(e.pending, ctrl)
		return
	}
	e.pending[ctrl] = pend{value: pos, at: now}
}

// position is where v puts a control, and where the Hive is now, on the
// control's own scale (a worker count, an index into the route's choices).
func (e *Engine) position(ctrl string, b Binding, v float64) (pos, cur int, ok bool) {
	switch b.Do {
	case "workers":
		top := b.Max
		if top == 0 {
			top = 8
		}
		return b.Min + int(math.Round(v*float64(top-b.Min))), e.st.Config.MaxWorkers, true
	case "route":
		choices := e.Choices(b)
		if len(choices) < 2 {
			if e.lastPos[ctrl] != -2 {
				e.say(ctrl + ": no models to route " + b.Role + " to: name some in config.json (models, model_choices) or list them in the mapping")
				e.lastPos[ctrl] = -2
			}
			return 0, 0, false
		}
		pos = min(int(v*float64(len(choices))), len(choices)-1)
		return pos, slices.Index(choices, e.route(b.Role)), true
	}
	return 0, 0, false
}

func (e *Engine) route(role string) string {
	if r := e.st.Config.Routes[role]; len(r) > 0 {
		return r[0]
	}
	return ""
}

// Choices are a route knob's stops: no route first, then the mapping's
// models or the Hive's named models and model choices.
func (e *Engine) Choices(b Binding) []string {
	out := []string{""}
	if len(b.Models) > 0 {
		return append(out, b.Models...)
	}
	names := append([]string{}, e.st.Config.ModelChoices...)
	for k := range e.st.Config.Models {
		names = append(names, k)
	}
	sort.Strings(names)
	return append(out, slices.Compact(names)...)
}

func (e *Engine) label(b Binding, pos int) string {
	if b.Do == "route" {
		if c := e.Choices(b); pos >= 0 && pos < len(c) {
			return orNone(c[pos])
		}
		return "a model not on the knob"
	}
	return fmt.Sprint(pos)
}

func orNone(s string) string {
	if s == "" {
		return "no route"
	}
	return s
}

// Tick sends faders and knobs that have settled and drops a confirm that
// waited too long.
func (e *Engine) Tick(now time.Time) {
	if e.Driving() && !e.caughtUp() && e.flight == nil && len(e.outbox) == 0 && !e.uiSentAt.IsZero() && now.Sub(e.uiSentAt) > uiPatience {
		e.uiStale, e.uiWant, e.lagging = true, 0, nil
		e.say("no control center is following: the deck moves its own view")
	}
	if e.armed != nil && now.Sub(e.armed.at) > time.Duration(e.m.ConfirmSeconds*float64(time.Second)) {
		e.say("no second press: " + e.armed.desc + " not done")
		e.armed = nil
	}
	settle := time.Duration(e.m.SettleMillis) * time.Millisecond
	for _, ctrl := range sortedKeys(e.pending) {
		p := e.pending[ctrl]
		if now.Sub(p.at) < settle {
			continue
		}
		delete(e.pending, ctrl)
		b := e.m.Controls[ctrl]
		switch b.Do {
		case "workers":
			e.setWorkers(ctrl, p.value)
		case "route":
			e.setRoute(ctrl, b.Role, e.Choices(b)[p.value])
		}
	}
}

func (e *Engine) setWorkers(ctrl string, n int) {
	was := e.st.Config.MaxWorkers
	desc := fmt.Sprintf("set the worker limit to %d (was %d)", n, was)
	ok := e.run(desc, func() error {
		_, err := e.h.Op("workers", map[string]any{"max": n})
		if errors.Is(err, hive.ErrNoOp) {
			err = e.viaConfig(fmt.Sprintf("max_workers from %d to %d", was, n), func(c map[string]any) { c["max_workers"] = n })
		}
		return err
	})
	if ok && e.st.Config.MaxWorkers == was { // a dry run, or Poll didn't see it yet: keep the pickup in step
		e.st.Config.MaxWorkers = n
	}
	e.holds[ctrl] = e.st.Config.MaxWorkers
}

func (e *Engine) setRoute(ctrl, role, model string) {
	was := e.route(role)
	var models []string
	if model != "" {
		models = []string{model}
	}
	desc := fmt.Sprintf("route %s to %s (was %s)", role, orNone(model), orNone(was))
	ok := e.run(desc, func() error {
		_, err := e.h.Op("route", map[string]any{"role": role, "models": strings.Join(models, ",")})
		if errors.Is(err, hive.ErrNoOp) {
			err = e.viaConfig(fmt.Sprintf("the %s route from %s to %s", role, orNone(was), orNone(model)), func(c map[string]any) {
				routes, _ := c["routes"].(map[string]any)
				if routes == nil {
					routes = map[string]any{}
				}
				if models == nil {
					delete(routes, role)
				} else {
					routes[role] = models
				}
				c["routes"] = routes
			})
		}
		return err
	})
	if ok && e.route(role) == was {
		if e.st.Config.Routes == nil {
			e.st.Config.Routes = map[string][]string{}
		}
		e.st.Config.Routes[role] = models
	}
	b := e.m.Controls[ctrl]
	e.holds[ctrl] = slices.Index(e.Choices(b), e.route(role))
}

// viaConfig is the change through Settings' own route, for a Hive without
// /api/op, with a line on the journal saying the person made it from the deck.
func (e *Engine) viaConfig(what string, change func(map[string]any)) error {
	if err := e.h.SetConfig(change); err != nil {
		return err
	}
	return e.h.Note("the person set " + what + " from the deck")
}

// uiPatience is how long the control center has to show a move before the
// deck takes it that none is open (one shows a move well inside a second).
const uiPatience = 5 * time.Second

// Light levels.
const (
	Off = iota
	On
	Slow
	Fast
)

// Signal is a light signal's level now.
func (e *Engine) Signal(name string) int {
	proj := e.currentProject()
	armedOn := func(prefix string) bool {
		return e.armed != nil && strings.HasPrefix(e.armed.key, "answer:") && e.m.Controls[e.armed.control].Do == prefix
	}
	if !e.connected && name != "connected" {
		return Off
	}
	switch name {
	case "connected":
		return lv(e.connected)
	case "main.green":
		return lv(proj.Main() == "green")
	case "main.red":
		if proj.Main() == "red" {
			return Slow
		}
	case "asks":
		if len(e.st.Pending()) > 0 {
			return Slow
		}
	case "gate":
		switch {
		case len(e.st.Checks.Running) > 0:
			return On
		case len(e.st.Checks.Waiting) > 0:
			return Slow
		}
	case "lead":
		for _, c := range e.st.Chats {
			if strings.HasPrefix(c.LeadStatus, "running") || strings.HasPrefix(c.LeadStatus, "working") {
				return On
			}
		}
	case "held":
		return lv(proj.Hold != nil)
	case "approve", "refuse", "answer":
		if armedOn(name) {
			return Fast
		}
		if a, ok := e.selected(); ok {
			_, can := Option(a, name == "approve")
			if name == "answer" {
				_, can = e.picked(a)
			}
			return lv(can)
		}
	case "steering":
		return lv(e.Driving())
	case "armed":
		if e.armed != nil {
			return Fast
		}
	default:
		if v, ok := strings.CutPrefix(name, "view."); ok {
			if e.Driving() {
				cur := e.uiShown()
				return lv(toControl(v) == cur || v == "threads" && (cur == "chats" || cur == "thread"))
			}
			return lv(e.view == v)
		}
	}
	return Off
}

func lv(b bool) int {
	if b {
		return On
	}
	return Off
}

func (e *Engine) currentProject() hive.Project {
	for _, p := range e.st.Projects {
		if p.Name == e.project {
			return p
		}
	}
	return hive.Project{}
}

// Lights is each mapped light now: blinking ones by the clock.
func (e *Engine) Lights(now time.Time) map[string]surface.Light {
	ms := now.UnixMilli()
	out := map[string]surface.Light{}
	for ctrl, sig := range e.m.Lights {
		on := false
		switch e.Signal(sig) {
		case On:
			on = true
		case Slow:
			on = ms/500%2 == 0
		case Fast:
			on = ms/125%2 == 0
		}
		out[ctrl] = surface.Light{On: on}
	}
	return out
}

// Flush sends the lights that changed since the last flush (all of them the
// first time).
func (e *Engine) Flush(port midi.Port, now time.Time) error {
	lights := e.Lights(now)
	for _, ctrl := range sortedKeys(lights) {
		l := lights[ctrl]
		if old, ok := e.lit[ctrl]; e.sent && ok && old == l {
			continue
		}
		for _, m := range e.p.LightMsgs(ctrl, l) {
			if err := port.Send(m); err != nil {
				return err
			}
		}
		e.lit[ctrl] = l
	}
	e.sent = true
	e.litLags(now)
	return nil
}

// Dark turns every mapped light off, for when the deck stops.
func (e *Engine) Dark(port midi.Port) {
	for _, ctrl := range sortedKeys(e.m.Lights) {
		for _, m := range e.p.LightMsgs(ctrl, surface.Light{}) {
			port.Send(m)
		}
	}
}

// Describe is what each mapped control does, for `deck --dry-run` and the docs.
func (e *Engine) Describe() []string {
	var out []string
	for _, ctrl := range sortedKeys(e.m.Controls) {
		out = append(out, fmt.Sprintf("%-14s %s", ctrl, Describe(e.m.Controls[ctrl])))
	}
	for _, ctrl := range sortedKeys(e.m.Lights) {
		out = append(out, fmt.Sprintf("%-14s light: %s", ctrl, Signals[e.m.Lights[ctrl]]))
	}
	return out
}

// Describe is one binding in words.
func Describe(b Binding) string {
	switch b.Do {
	case "approve", "refuse":
		return b.Do + " the selected Waiting item (press twice: the second confirms)"
	case "select":
		if b.Step != 0 {
			return fmt.Sprintf("move the selection by %+d", b.Step)
		}
		return "move the selection"
	case "view":
		return "jump to the " + b.View + " view"
	case "page":
		return "move the selection a page " + dir(b.Step, "up", "down")
	case "pick":
		if b.Step != 0 {
			return "pick the " + dir(b.Step, "previous", "next") + " answer in the control center"
		}
		return Actions[b.Do]
	case "workers":
		top := b.Max
		if top == 0 {
			top = 8
		}
		return fmt.Sprintf("worker limit, %d to %d", b.Min, top)
	case "route":
		return "the " + b.Role + " role's model route"
	}
	s := Actions[b.Do]
	if need(b, false) {
		s += " (press twice: the second confirms)"
	}
	return s
}

func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

// summary is one journal event in a line.
func summary(ev hive.Event) string {
	var d map[string]any
	json.Unmarshal(ev.Data, &d)
	var parts []string
	for _, k := range []string{"project", "title", "text", "id", "status"} {
		if s, ok := d[k].(string); ok && s != "" {
			parts = append(parts, s)
		}
		if len(parts) == 2 {
			break
		}
	}
	return strings.Join(parts, " · ")
}
