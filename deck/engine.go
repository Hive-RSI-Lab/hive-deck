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
	st, err := e.h.State()
	if err != nil {
		if e.connected || !e.polled {
			e.say("can't reach the Hive: " + err.Error())
		}
		e.connected, e.lastErr, e.polled = false, err.Error(), true
		return
	}
	if !e.connected && e.polled {
		e.say("the Hive answers again")
	}
	e.connected, e.lastErr, e.polled = true, "", true
	e.SetState(st)
	if evs, err := e.h.Events(e.after); err == nil {
		for _, ev := range evs {
			if ev.Seq > e.after {
				e.after = ev.Seq
			}
		}
		e.feed = append(e.feed, evs...)
		if len(e.feed) > 500 {
			e.feed = e.feed[len(e.feed)-500:]
		}
	}
	if e.view == "threads" {
		e.loadThread()
	}
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

// selected is the selected Waiting item.
func (e *Engine) selected() (hive.Ask, bool) {
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

// Handle acts on one control's event.
func (e *Engine) Handle(ev surface.Event, now time.Time) {
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
			e.move(b, steps)
		}
	case surface.Absolute:
		e.absolute(ev.Control, b, ev.Value, now)
	}
}

func (e *Engine) press(ctrl string, b Binding, now time.Time) {
	switch b.Do {
	case "approve", "refuse":
		e.answer(ctrl, b, b.Do == "approve", now)
	case "select", "scroll", "project":
		step := b.Step
		if step == 0 {
			step = 1
		}
		e.move(b, step)
	case "view":
		e.setView(b.View)
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

func (e *Engine) answer(ctrl string, b Binding, approve bool, now time.Time) {
	a, ok := e.selected()
	if !ok {
		e.say("nothing is waiting")
		return
	}
	ans, ok := Option(a, approve)
	if !ok {
		e.say(fmt.Sprintf("%q takes only %s: approve it", clip(a.Title, 50), strings.Join(a.Options, " or ")))
		return
	}
	confirm := need(b, true) || AlwaysPersons(a.Kind)
	desc := fmt.Sprintf("answer %s %q with %q", a.Kind, clip(a.Title, 50), ans)
	e.confirmThen(ctrl, confirm, "answer:"+a.ID+":"+ans, desc, now, func() error { return e.h.Answer(a.ID, ans, "answered from the deck") })
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
	if a, ok := e.selected(); ok && e.view == "waiting" {
		for _, t := range all {
			if t.ID == a.Ref && fits(t) {
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

// move is a jog's or a pad's step: the selection, the scroll, or the project.
func (e *Engine) move(b Binding, steps int) {
	switch b.Do {
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
	case "approve", "refuse":
		if armedOn(name) {
			return Fast
		}
		if a, ok := e.selected(); ok {
			_, can := Option(a, name == "approve")
			return lv(can)
		}
	case "armed":
		if e.armed != nil {
			return Fast
		}
	default:
		if v, ok := strings.CutPrefix(name, "view."); ok {
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
