package deck

import (
	"fmt"
	"strings"

	"github.com/Hive-RSI-Lab/hive-deck/hive"
)

// Screen is the deck's own terminal view: the Hive in a few lines, the
// current view with its selection, and what the deck said last. The jogs
// move and scroll it; the control center keeps its own.
func (e *Engine) Screen(width, height int) string {
	width, height = max(width, 40), max(height, 12)
	var b strings.Builder
	line := func(s string) {
		if r := []rune(s); len(r) > width {
			s = string(r[:width-1]) + "…"
		}
		b.WriteString(s + "\n")
	}
	conn := "connected"
	if !e.connected {
		conn = "not connected: " + e.lastErr
	}
	if e.dry {
		conn += " · DRY RUN: nothing is sent to the Hive"
	}
	line(fmt.Sprintf("hive deck · %s · %s", e.p.Name, conn))
	p := e.currentProject()
	main := p.Main()
	if main == "" {
		main = "not measured"
	}
	hold := ""
	if p.Hold != nil {
		hold = " · merges held: " + p.Hold.Why
	}
	line(fmt.Sprintf("project %s · main %s%s · waiting %d · checks %d running, %d queued · workers %d",
		orDash(e.project), main, hold, len(e.st.Pending()), len(e.st.Checks.Running), len(e.st.Checks.Waiting), e.st.Config.MaxWorkers))
	var tabs []string
	for _, v := range Views {
		if v == e.view {
			v = "[" + strings.ToUpper(v) + "]"
		}
		tabs = append(tabs, v)
	}
	line(strings.Join(tabs, "  "))
	line(strings.Repeat("─", width))
	body := height - 6 - len(e.log)
	body = max(body, 4)
	rows := e.rows(width)
	for i := 0; i < body; i++ {
		if i < len(rows) {
			line(rows[i])
		} else {
			line("")
		}
	}
	line(strings.Repeat("─", width))
	if e.armed != nil {
		line("» waiting for the confirm: press " + e.armed.control + " again to " + e.armed.desc)
	}
	for _, s := range e.log {
		line("· " + s)
	}
	return b.String()
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func (e *Engine) rows(width int) []string {
	var out []string
	mark := func(sel bool) string {
		if sel {
			return "▶ "
		}
		return "  "
	}
	switch e.view {
	case "waiting":
		asks := e.st.Pending()
		if len(asks) == 0 {
			return []string{"nothing is waiting on you"}
		}
		a, _ := e.selected()
		for _, x := range asks {
			out = append(out, fmt.Sprintf("%s%-8s %s %s", mark(x.ID == a.ID), x.Kind, clip(x.Title, width-30), optionsText(x)))
		}
		out = append(out, "")
		detail := strings.Split(strings.TrimSpace(a.Detail), "\n")
		out = append(out, scrolled(detail, e.scroll)...)
	case "feed":
		n := len(e.feed)
		for i := n - 1 - e.scroll; i >= 0 && len(out) < 200; i-- {
			ev := e.feed[i]
			out = append(out, fmt.Sprintf("%s %-22s %s", ev.At.Local().Format("15:04:05"), ev.Kind, summary(ev)))
		}
		if len(out) == 0 {
			out = append(out, "no events yet")
		}
	case "threads":
		for i, c := range e.st.Chats {
			st := ""
			if c.LeadStatus != "" {
				st = " (" + c.LeadStatus + ")"
			}
			unread := ""
			if c.Unread > 0 {
				unread = fmt.Sprintf(" · %d unread", c.Unread)
			}
			out = append(out, fmt.Sprintf("%s%s%s%s", mark(i == e.sel["threads"]), orDash(c.Title), st, unread))
			if len(out) >= 6 {
				break
			}
		}
		out = append(out, "")
		var lines []string
		for i := len(e.thread) - 1; i >= 0; i-- {
			m := e.thread[i]
			lines = append(lines, fmt.Sprintf("%s %s: %s", m.At.Local().Format("15:04"), m.From, clip(m.Text, 400)))
		}
		out = append(out, scrolled(lines, e.scroll)...)
	case "runs":
		for i, r := range e.liveRuns() {
			out = append(out, fmt.Sprintf("%s%-9s %-8s %-10s %s", mark(i == e.sel["runs"]), r.Role, r.Status, orDash(r.Project), r.Progress))
		}
		if len(out) == 0 {
			out = append(out, "no agents running")
		}
	}
	return out
}

func optionsText(a hive.Ask) string {
	yes, _ := Option(a, true)
	no, ok := Option(a, false)
	if !ok {
		return "[" + yes + "]"
	}
	return "[" + yes + " / " + no + "]"
}

func scrolled(lines []string, n int) []string {
	if n >= len(lines) {
		n = max(len(lines)-1, 0)
	}
	if len(lines) == 0 {
		return nil
	}
	return lines[n:]
}
