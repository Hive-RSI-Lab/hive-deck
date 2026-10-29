package deck

import (
	"fmt"
	"slices"
	"time"
)

// Lag is how long one move took from the press: until the Hive took it
// (POST /api/ui answered), until the deck read that the control center
// showed it (GET /api/ui), and until the deck's lights showed that.
type Lag struct {
	Sent, Shown, Lit time.Duration
}

// lagging is a move the Hive took that the deck hasn't seen shown and lit.
type lagging struct {
	seq         int64
	at          time.Time
	sent, shown time.Duration
}

// keepLags is how many moves' times the deck keeps.
const keepLags = 50

// shown marks the moves the control center has now shown.
func (e *Engine) shown(now time.Time) {
	if e.ui == nil {
		return
	}
	for i := range e.lagging {
		if l := &e.lagging[i]; l.shown == 0 && e.ui.Applied >= l.seq {
			l.shown = max(now.Sub(l.at), time.Nanosecond)
		}
	}
}

// litLags takes the moves the lights now show.
func (e *Engine) litLags(now time.Time) {
	var left []lagging
	for _, l := range e.lagging {
		if l.shown == 0 {
			left = append(left, l)
			continue
		}
		e.lags = append(e.lags, Lag{Sent: l.sent, Shown: l.shown, Lit: max(now.Sub(l.at), l.shown)})
	}
	e.lagging = left
	if len(e.lags) > keepLags {
		e.lags = e.lags[len(e.lags)-keepLags:]
	}
}

// Lags are the last moves' times, oldest first.
func (e *Engine) Lags() []Lag { return slices.Clone(e.lags) }

// LagLine says what the last moves took at each hop: the median, and the
// slowest in brackets. "" before the first.
func LagLine(lags []Lag) string {
	if len(lags) == 0 {
		return ""
	}
	hop := func(f func(Lag) time.Duration) string {
		ds := make([]time.Duration, len(lags))
		for i, l := range lags {
			ds[i] = f(l)
		}
		slices.Sort(ds)
		return fmt.Sprintf("%s (%s)", ms(ds[len(ds)/2]), ms(ds[len(ds)-1]))
	}
	return fmt.Sprintf("press to: Hive %s · control center %s · lights %s, median (slowest) of %d",
		hop(func(l Lag) time.Duration { return l.Sent }), hop(func(l Lag) time.Duration { return l.Shown }),
		hop(func(l Lag) time.Duration { return l.Lit }), len(lags))
}

func ms(d time.Duration) string {
	return fmt.Sprintf("%d ms", d.Round(time.Millisecond).Milliseconds())
}
