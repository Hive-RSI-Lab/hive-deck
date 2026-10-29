package deck

import (
	"time"

	"github.com/Hive-RSI-Lab/hive-deck/hive"
	"github.com/Hive-RSI-Lab/hive-deck/midi"
	"github.com/Hive-RSI-Lab/hive-deck/surface"
)

// Loop is what Run needs besides the engine: the device, and what the
// command around it adds.
type Loop struct {
	Port midi.Port
	Keys <-chan surface.Event // typed controls, for a dry run without the device; closed ends the run
	Stop <-chan struct{}      // the run ends, its lights dark
	Draw func()               // the screen, a few times a second; nil for none
	Warn func(error)          // the lights couldn't be sent
}

// How often the loop reads the Hive: its state and journal each ReadEvery,
// and the control center's view each ViewEvery, or as soon as the last read
// came back while a move is sent and not yet shown.
var (
	ReadEvery = time.Second
	ViewEvery = 250 * time.Millisecond
	viewGap   = 15 * time.Millisecond // the least between two reads of the view
	tickEvery = 60 * time.Millisecond
	drawEvery = 150 * time.Millisecond
)

type uiRead struct {
	v   *hive.UIView
	err error
}

type sent struct {
	seq int64
	err error
}

// Run runs the deck until the device goes away, Keys closes or Stop. Each
// control is handled the moment it comes; the Hive is read and steered from
// goroutines of their own, one request of each kind at a time, so neither a
// slow /api/state nor a slow ssh link holds a press or a light up. Moves go
// out in order, and those that pile up behind a slow one go as one.
func (e *Engine) Run(l Loop) error {
	e.async = true
	defer func() { e.async = false }()
	var (
		reads              = make(chan Read, 1)
		views              = make(chan uiRead, 1)
		sends              = make(chan sent, 1)
		reading, viewing   bool
		sending            bool
		lastRead, lastView time.Time
		lastDraw           time.Time
	)
	tick := time.NewTicker(tickEvery)
	defer tick.Stop()
	pump := func(now time.Time) {
		if !reading && now.Sub(lastRead) >= ReadEvery {
			reading, lastRead = true, now
			read := e.Reader()
			go func() { reads <- read() }()
		}
		if !sending {
			if c := e.Next(); c != nil {
				sending = true
				h, c := e.h, *c
				go func() {
					seq, err := h.UI(c)
					sends <- sent{seq, err}
				}()
			}
		}
		if !viewing && (e.awaiting() || now.Sub(lastView) >= ViewEvery) {
			if read := e.UIReader(); read != nil {
				viewing = true
				wait := lastView.Add(viewGap).Sub(now)
				go func() {
					time.Sleep(wait)
					v, err := read()
					views <- uiRead{v, err}
				}()
			}
		}
		if err := e.Flush(l.Port, now); err != nil && l.Warn != nil {
			l.Warn(err)
		}
	}
	pump(time.Now())
	for {
		select {
		case <-l.Stop:
			e.Dark(l.Port)
			return nil
		case ev, ok := <-l.Keys:
			if !ok {
				return nil
			}
			e.Handle(ev, time.Now())
		case msg, ok := <-l.Port.In():
			if !ok {
				return errGone
			}
			if ev, known, _ := e.p.Decode(msg); known {
				e.Handle(ev, time.Now())
			}
		case r := <-reads:
			reading = false
			e.Apply(r)
		case r := <-views:
			viewing, lastView = false, time.Now()
			e.ApplyUI(r.v, r.err, lastView)
		case s := <-sends:
			sending = false
			e.Sent(s.seq, s.err, time.Now())
		case now := <-tick.C:
			e.Tick(now)
			if l.Draw != nil && now.Sub(lastDraw) >= drawEvery {
				l.Draw()
				lastDraw = now
			}
		}
		pump(time.Now())
	}
}

// awaiting is whether a move the Hive took isn't shown yet, so the loop
// reads the control center's view again as soon as the last read is back.
func (e *Engine) awaiting() bool {
	return e.Driving() && e.flight == nil && len(e.outbox) == 0 && !e.caughtUp()
}

type gone struct{}

func (gone) Error() string { return "the deck went away (unplugged?)" }

var errGone error = gone{}
