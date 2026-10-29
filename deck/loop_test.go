package deck

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/Hive-RSI-Lab/hive-deck/hive"
	"github.com/Hive-RSI-Lab/hive-deck/midi"
	"github.com/Hive-RSI-Lab/hive-deck/surface"
)

// daemon is the slice of the Hive's daemon the loop talks to: /api/state
// as slow as it is under load, and /api/ui as the daemon's bus has it
// (internal/mod/ui): a GET waits for the next command, a view is kept.
type daemon struct {
	mu    sync.Mutex
	slow  time.Duration // each /api/state takes this long
	state hive.State
	seq   int64
	cmds  []hive.UICommand
	got   []time.Time // when each command came
	view  *hive.UIView
	wake  chan struct{}
}

func (d *daemon) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var out any = []any{}
	switch r.URL.Path {
	case "/api/state":
		select {
		case <-time.After(d.slow):
		case <-r.Context().Done():
		}
		d.mu.Lock()
		out = d.state
		d.mu.Unlock()
	case "/api/ui":
		if r.Method == http.MethodPost {
			var c hive.UICommand
			json.NewDecoder(r.Body).Decode(&c)
			d.mu.Lock()
			d.seq++
			c.Seq = d.seq
			d.cmds, d.got = append(d.cmds, c), append(d.got, time.Now())
			if d.wake != nil {
				close(d.wake)
				d.wake = nil
			}
			d.mu.Unlock()
			out = c
			break
		}
		after, err := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
		if err != nil {
			after = -1
		}
		wait, _ := strconv.Atoi(r.URL.Query().Get("wait"))
		end := time.After(time.Duration(wait) * time.Second)
		for {
			d.mu.Lock()
			st := struct {
				Seq      int64            `json:"seq"`
				Commands []hive.UICommand `json:"commands"`
				View     *hive.UIView     `json:"view"`
			}{d.seq, []hive.UICommand{}, d.view}
			for _, c := range d.cmds {
				if after >= 0 && c.Seq > after {
					st.Commands = append(st.Commands, c)
				}
			}
			if after < 0 || len(st.Commands) > 0 || wait <= 0 {
				d.mu.Unlock()
				out = st
				break
			}
			if d.wake == nil {
				d.wake = make(chan struct{})
			}
			wake := d.wake
			d.mu.Unlock()
			select {
			case <-wake:
			case <-end:
				wait = 0
			case <-r.Context().Done():
				return
			}
		}
	case "/api/ui/view":
		var v hive.UIView
		json.NewDecoder(r.Body).Decode(&v)
		v.At = time.Now()
		d.mu.Lock()
		d.view = &v
		d.mu.Unlock()
		out = "ok"
	}
	json.NewEncoder(w).Encode(out)
}

// controlCenter follows the daemon as internal/tui/steer.go does: it waits
// on GET /api/ui, moves its Waiting selection, and reports at once.
func controlCenter(t *testing.T, c *hive.Client, url string, asks []hive.Ask, stop <-chan struct{}) {
	sel, seq := 0, int64(-1)
	report := func() {
		v := hive.UIView{By: "test", View: "waiting", Focus: "main", Sel: sel, Rows: len(asks), Item: asks[sel].ID, Title: asks[sel].Title, Options: asks[sel].Options, Applied: max(seq, 0)}
		raw, _ := json.Marshal(v)
		resp, err := http.Post(url+"/api/ui/view", "application/json", bytesReader(raw))
		if err == nil {
			resp.Body.Close()
		}
	}
	for {
		select {
		case <-stop:
			return
		default:
		}
		wait := 2
		if seq < 0 {
			wait = 0
		}
		resp, err := http.Get(url + "/api/ui?after=" + strconv.FormatInt(seq, 10) + "&wait=" + strconv.Itoa(wait))
		if err != nil {
			return
		}
		var st struct {
			Seq      int64
			Commands []hive.UICommand
		}
		json.NewDecoder(resp.Body).Decode(&st)
		resp.Body.Close()
		for _, cmd := range st.Commands {
			if seq < 0 {
				break
			}
			switch cmd.Action {
			case "down":
				sel = min(sel+max(cmd.N, 1), len(asks)-1)
			case "up":
				sel = max(sel-max(cmd.N, 1), 0)
			}
		}
		seq = st.Seq
		report()
	}
}

// slowHive is a daemon whose /api/state takes slow, a control center
// following it, and the loop running an engine on a fake deck.
func slowHive(t *testing.T, slow time.Duration) (*daemon, *midi.Fake, func() *Engine) {
	t.Helper()
	st := waiting()
	d := &daemon{slow: slow, state: st}
	srv := httptest.NewServer(d)
	c := hive.Remote(srv.URL, "token")
	stop := make(chan struct{})
	go controlCenter(t, c, srv.URL, st.Pending(), stop)

	m, _ := ParseMapping(DefaultMapping())
	p, _ := surface.Load(m.Profile)
	e := New(m, p, c, false)
	port := midi.NewFake("DDJ-FLX4")
	quit := make(chan struct{})
	ended := make(chan error, 1)
	go func() { ended <- e.Run(Loop{Port: port, Stop: quit}) }()
	end := func() *Engine {
		close(quit)
		if err := <-ended; err != nil {
			t.Fatal(err)
		}
		close(stop)
		srv.CloseClientConnections()
		srv.Close()
		return e
	}
	return d, port, end
}

// A press goes to the Hive at once and the lights follow the control
// center within a few round trips, while each read of the Hive's state
// takes seconds, as /api/state does under load.
func TestPressesDontWaitOnASlowHive(t *testing.T) {
	const slow = 3 * time.Second
	d, port, end := slowHive(t, slow)
	// until wants the control center to have shown move n, and the deck a
	// few reads of the view more to have read it (a loaded machine is slow).
	until := func(n int64) {
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			d.mu.Lock()
			ok := d.view != nil && d.view.Applied >= n
			d.mu.Unlock()
			if ok {
				time.Sleep(3 * ViewEvery)
				return
			}
		}
		t.Fatalf("the control center never showed move %d", n)
	}
	until(0)
	var pressed []time.Time
	for i := 0; i < 4; i++ {
		msgs := jogDown
		if i%2 == 1 {
			msgs = jogUp
		}
		pressed = append(pressed, time.Now())
		port.Receive(msgs...)
		until(int64(i + 1))
	}
	e := end()

	d.mu.Lock()
	got := slices.Clone(d.got)
	d.mu.Unlock()
	if len(got) != len(pressed) {
		t.Fatalf("the Hive got %d moves for %d presses", len(got), len(pressed))
	}
	for i := range got {
		if lag := got[i].Sub(pressed[i]); lag > slow/3 {
			t.Errorf("move %d reached the Hive %s after its press, behind a read of /api/state", i+1, lag)
		}
	}
	lags := e.Lags()
	t.Log(LagLine(lags))
	if len(lags) != len(pressed) {
		t.Fatalf("%d moves timed, want %d: %+v", len(lags), len(pressed), lags)
	}
	for i, l := range lags {
		if !(l.Sent > 0 && l.Sent <= l.Shown && l.Shown <= l.Lit) {
			t.Errorf("move %d: hops out of order: %+v", i+1, l)
		}
		if l.Lit > slow/3 {
			t.Errorf("move %d took %s to the lights, behind a read of /api/state", i+1, l.Lit)
		}
	}
}

// Steps that come while a move is on its way go as one move.
func TestStepsPileUpBehindASlowSend(t *testing.T) {
	e, h := drive(t, false)
	e.async = true
	p := e.p
	for range 5 {
		press(t, e, p, t0, jogDown...)
	}
	press(t, e, p, t0, jogUp...)
	var sent []string
	for c := e.Next(); c != nil; c = e.Next() {
		if e.Next() != nil {
			t.Fatal("a second move went before the first was answered")
		}
		seq, err := h.UI(*c)
		e.Sent(seq, err, t0)
		sent = append(sent, c.Action+" "+strconv.Itoa(c.N))
	}
	if want := []string{"down 5", "up 1"}; !slices.Equal(sent, want) {
		t.Fatalf("sent %v, want %v", sent, want)
	}
	if e.caughtUp() {
		t.Fatal("caught up before the control center showed the moves")
	}
	shows(e, h, open("a3", 0, t0))
	if !e.caughtUp() {
		t.Fatal("not caught up once the control center showed both moves")
	}
}

// Each move's hops are timed from its press: the Hive taking it, the
// control center showing it, the lights showing that.
func TestLagIsTimedPerHop(t *testing.T) {
	e, h := drive(t, false)
	e.async = true
	press(t, e, e.p, t0, jogDown...)
	c := e.Next()
	seq, _ := h.UI(*c)
	e.Sent(seq, nil, t0.Add(40*time.Millisecond))
	if e.Driving() && e.caughtUp() {
		t.Fatal("caught up before the control center showed the move")
	}
	v := open("a2", seq, t0.Add(time.Second))
	e.ApplyUI(v, nil, t0.Add(150*time.Millisecond))
	e.Flush(midi.NewFake("x"), t0.Add(160*time.Millisecond))
	want := []Lag{{40 * time.Millisecond, 150 * time.Millisecond, 160 * time.Millisecond}}
	if got := e.Lags(); !slices.Equal(got, want) {
		t.Fatalf("lags %+v, want %+v", got, want)
	}
	if got := LagLine(e.Lags()); got != "press to: Hive 40 ms (40 ms) · control center 150 ms (150 ms) · lights 160 ms (160 ms), median (slowest) of 1" {
		t.Fatal(got)
	}
}

// A view pad lights its view the moment it is pressed, not when the Hive
// has taken the move.
func TestViewPadLightsAtOnce(t *testing.T) {
	e, _ := drive(t, false)
	e.async = true
	press(t, e, e.p, t0, agents...)
	if e.View() != "runs" {
		t.Fatalf("view %s before the move went, want runs", e.View())
	}
}

func bytesReader(b []byte) *bytes.Reader { return bytes.NewReader(b) }

// A daemon that restarted counts its moves from 1 again: a move timed
// before it is dropped, not waited on for good.
func TestLagAfterARestart(t *testing.T) {
	e, h := drive(t, false)
	e.async = true
	h.uiSeq = 40
	press(t, e, e.p, t0, jogDown...)
	c := e.Next()
	seq, _ := h.UI(*c)
	e.Sent(seq, nil, t0)
	h.uiSeq = 0 // the daemon restarted
	press(t, e, e.p, t0, jogUp...)
	c = e.Next()
	seq, _ = h.UI(*c)
	e.Sent(seq, nil, t0.Add(10*time.Millisecond))
	e.ApplyUI(open("a1", seq, t0.Add(time.Minute)), nil, t0.Add(50*time.Millisecond))
	e.Flush(midi.NewFake("x"), t0.Add(50*time.Millisecond))
	if len(e.lagging) != 0 || len(e.Lags()) != 1 || e.awaiting() {
		t.Fatalf("lagging %+v, lags %+v, awaiting %v", e.lagging, e.Lags(), e.awaiting())
	}
}

// A jog on the deck's own threads view while a read is out stays where it
// went when the read lands.
func TestJogDuringAReadStays(t *testing.T) {
	st := waiting()
	st.Chats = []hive.Chat{{ID: "c1", Title: "lead"}, {ID: "c2", Title: "deck"}, {ID: "c3", Title: "hive"}}
	e, _, _, _ := setup(t, "", st, false)
	e.async = true
	e.setView("threads")
	read := e.Reader()
	press(t, e, e.p, t0, jogDown...)
	if e.sel["threads"] != 1 || e.chat != "c2" {
		t.Fatalf("the jog went to %d %s, want 1 c2", e.sel["threads"], e.chat)
	}
	e.Apply(read())
	if e.sel["threads"] != 1 || e.chat != "c2" {
		t.Fatalf("the read took the jog back to %d %s", e.sel["threads"], e.chat)
	}
	e.Apply(e.Reader()())
	if e.sel["threads"] != 1 || e.chat != "c2" {
		t.Fatalf("a read since moved it to %d %s", e.sel["threads"], e.chat)
	}
}
