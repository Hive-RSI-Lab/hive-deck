package deck

import (
	"strings"
	"testing"
	"time"

	"github.com/Hive-RSI-Lab/hive-deck/hive"
	"github.com/Hive-RSI-Lab/hive-deck/midi"
)

var (
	jogDown = []midi.Msg{{0xB0, 0x22, 0x40 + 3}, {0xB0, 0x22, 0x40 + 3}} // jog.l, one step of 6 ticks
	jogUp   = []midi.Msg{{0xB0, 0x22, 0x40 - 3}, {0xB0, 0x22, 0x40 - 3}}
	agents  = []midi.Msg{{0x99, 0x03, 0x7F}, {0x99, 0x03, 0x00}} // pad.r.4
	enter   = []midi.Msg{{0x96, 0x41, 0x7F}, {0x96, 0x41, 0x00}} // browse.push
	esc     = []midi.Msg{{0x96, 0x46, 0x7F}, {0x96, 0x46, 0x00}} // load.l
	pickR   = []midi.Msg{{0x90, 0x11, 0x7F}, {0x90, 0x11, 0x00}} // loopout.l
	picked  = []midi.Msg{{0x90, 0x58, 0x7F}, {0x90, 0x58, 0x00}} // sync.l
)

// open is a control center showing Waiting on item id.
func open(id string, applied int64, at time.Time) *hive.UIView {
	v := &hive.UIView{By: "laptop pid 7", At: at, View: "waiting", Focus: "main", Sel: 0, Rows: 3, Item: id, Applied: applied}
	for _, a := range waiting().Asks {
		if a.ID == id {
			v.Title, v.Options = a.Title, a.Options
		}
	}
	return v
}

// drive is an engine with a control center open on Waiting's first item.
func drive(t *testing.T, dry bool) (*Engine, *fakeHive) {
	t.Helper()
	e, h, _, _ := setup(t, "", waiting(), dry)
	h.ui = open("a1", 0, t0)
	e.PollUI()
	if !e.Driving() {
		t.Fatal("a control center that reported its view isn't driven")
	}
	h.take()
	return e, h
}

// shows is the control center acting on everything sent and reporting v.
func shows(e *Engine, h *fakeHive, v *hive.UIView) {
	v.Applied = h.uiSeq
	v.At = v.At.Add(time.Duration(h.uiSeq+1) * time.Second)
	h.ui = v
	e.PollUI()
}

func TestControlsDriveTheControlCenter(t *testing.T) {
	e, h := drive(t, false)
	p := e.p
	press(t, e, p, t0, jogDown...)
	press(t, e, p, t0, jogUp...)
	press(t, e, p, t0, agents...)
	press(t, e, p, t0, enter...)
	press(t, e, p, t0, esc...)
	press(t, e, p, t0, pickR...)
	press(t, e, p, t0, midi.Msg{0xB1, 0x22, 0x40 + 20}) // jog.r, two pages of 10 ticks
	want := []string{
		"ui down 1   deck jog.l",
		"ui up 1   deck jog.l",
		"ui view 0 agents  deck pad.r.4",
		"ui open 0   deck browse.push",
		"ui back 0   deck load.l",
		"ui right 0   deck loopout.l",
		"ui pagedown 2   deck jog.r",
	}
	if got := h.take(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("sent:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestLightsFollowTheControlCenter(t *testing.T) {
	e, h := drive(t, false)
	l := func(sig string) int { return e.Signal(sig) }
	if l("view.waiting") != On || l("view.runs") != Off || l("steering") != On || l("approve") != On {
		t.Fatal("lights on Waiting:", l("view.waiting"), l("view.runs"), l("steering"), l("approve"))
	}
	press(t, e, e.p, t0, agents...)
	// lit at once, before the control center reports
	if l("view.runs") != On || l("view.agents") != On || l("view.waiting") != Off || l("approve") != Off {
		t.Fatal("the view pad isn't lit at once:", l("view.runs"), l("view.waiting"), l("approve"))
	}
	if e.View() != "runs" {
		t.Fatalf("the deck's own view didn't follow: %s", e.View())
	}
	// the person moves the control center by its keys: the lights follow
	shows(e, h, &hive.UIView{View: "thread", Focus: "thread", At: t0, Sel: -1})
	if l("view.threads") != On || l("view.runs") != Off || e.View() != "threads" {
		t.Fatal("lights on a thread:", l("view.threads"), l("view.runs"), e.View())
	}
	shows(e, h, &hive.UIView{View: "settings", At: t0, Sel: 0})
	if l("view.settings") != On || l("view.threads") != Off {
		t.Fatal("lights on settings:", l("view.settings"), l("view.threads"))
	}
}

func TestApproveAnswersTheControlCentersSelection(t *testing.T) {
	e, h := drive(t, false)
	p := e.p
	press(t, e, p, t0, jogDown...)
	h.take()
	// the control center hasn't shown the move: nothing to answer yet
	press(t, e, p, t0, approve...)
	if !strings.Contains(strings.Join(e.Log(), "\n"), "hasn't shown the last move yet") || e.armed != nil {
		t.Fatalf("approve before the control center caught up: %v", e.Log())
	}
	shows(e, h, open("a3", 0, t0))
	press(t, e, p, t0, approve...)
	press(t, e, p, t0, approve...)
	if c := h.take(); len(c) != 1 || c[0] != "answer a3 yes" {
		t.Fatalf("answered %v, want a3, the control center's selection", c)
	}
}

func TestControlCenterMovingOffDropsTheConfirm(t *testing.T) {
	e, h := drive(t, false)
	press(t, e, e.p, t0, approve...)
	if e.armed == nil {
		t.Fatal("not armed")
	}
	shows(e, h, open("a2", 0, t0)) // the person moved it with the keys
	press(t, e, e.p, t0, approve...)
	if c := h.take(); len(c) != 0 {
		t.Fatalf("answered after the control center moved off the item: %v", c)
	}
	// and a jog between the presses cancels too, before any report
	e, h = drive(t, false)
	press(t, e, e.p, t0, approve...)
	press(t, e, e.p, t0, jogDown...)
	press(t, e, e.p, t0, approve...)
	for _, c := range h.take() {
		if strings.HasPrefix(c, "answer") {
			t.Fatalf("a jog between the presses: %v", c)
		}
	}
}

func TestAnswerSendsThePickedOption(t *testing.T) {
	e, h := drive(t, false)
	v := open("a1", 0, t0)
	v.Option = 1
	shows(e, h, v)
	if e.Signal("answer") != On {
		t.Fatal("the answer pad isn't lit with an answer picked")
	}
	press(t, e, e.p, t0, picked...)
	press(t, e, e.p, t0, picked...)
	if c := h.take(); len(c) != 1 || c[0] != "answer a1 leave open" {
		t.Fatalf("answered %v", c)
	}
}

func TestRetryTakesTheControlCentersTask(t *testing.T) {
	e, h := drive(t, false)
	shows(e, h, &hive.UIView{View: "agents", At: t0, Sel: 2, Rows: 4, Item: "t0", Title: "older"})
	press(t, e, e.p, t0, retry...)
	press(t, e, e.p, t0, retry...)
	if c := h.take(); len(c) != 1 || c[0] != "retry t0" {
		t.Fatalf("retried %v", c)
	}
}

func TestWithoutAControlCenterTheDeckMovesItsOwnView(t *testing.T) {
	e, h, _, p := setup(t, "", waiting(), false) // no control center has reported
	press(t, e, p, t0, jogDown...)
	press(t, e, p, t0, enter...)
	if c := h.take(); len(c) != 0 {
		t.Fatalf("sent with no control center: %v", c)
	}
	if a, _ := e.selected(); a.ID != "a2" {
		t.Fatalf("the jog didn't move the deck's own selection: %s", a.ID)
	}
	if !strings.Contains(e.steering(), "open the control center") {
		t.Fatal(e.steering())
	}

	// a Hive from before /api/ui
	e, h, _, p = setup(t, "", waiting(), false)
	h.noUI = true
	e.Poll()
	press(t, e, p, t0, jogDown...)
	if c := h.take(); len(c) != 0 || e.Driving() || !strings.Contains(e.steering(), "hive update") {
		t.Fatalf("a Hive without /api/ui: %v %s", c, e.steering())
	}
}

func TestAControlCenterThatDoesntFollowIsLetGo(t *testing.T) {
	e, h := drive(t, false) // its last report stays on the daemon after it closed
	press(t, e, e.p, t0, jogDown...)
	e.Tick(t0.Add(6 * time.Second))
	if e.Driving() {
		t.Fatal("still driving a control center that never showed the move")
	}
	h.take()
	press(t, e, e.p, t0, jogDown...)
	if c := h.take(); len(c) != 0 {
		t.Fatalf("sent to a control center that isn't there: %v", c)
	}
	e.PollUI() // the same old report: still let go
	if e.Driving() {
		t.Fatal("the old report took it back")
	}
	shows(e, h, open("a1", 0, t0)) // it opens again and moves
	if !e.Driving() {
		t.Fatal("a control center that moved again isn't driven")
	}
}

func TestDryRunDoesntSteer(t *testing.T) {
	e, h := drive(t, true)
	press(t, e, e.p, t0, jogDown...)
	press(t, e, e.p, t0, agents...)
	if c := h.take(); len(c) != 0 {
		t.Fatalf("a dry run sent %v", c)
	}
	if !strings.Contains(strings.Join(e.Log(), "\n"), "would down in the control center") {
		t.Fatal(e.Log())
	}
}
