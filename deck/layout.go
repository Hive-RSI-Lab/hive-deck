package deck

import (
	"fmt"
	"html"
	"strings"

	"github.com/Hive-RSI-Lab/hive-deck/surface"
)

// Short is a binding's name on the picture.
func Short(b Binding) string {
	switch b.Do {
	case "approve":
		return "APPROVE ×2"
	case "refuse":
		return "REFUSE ×2"
	case "select":
		switch {
		case b.Step < 0:
			return "▲ PREV"
		case b.Step > 0:
			return "▼ NEXT"
		}
		return "SELECT"
	case "view":
		return strings.ToUpper(b.View)
	case "pick":
		return dir(b.Step, "◀ ANSWER", "ANSWER ▶")
	case "page":
		return dir(b.Step, "▲ PAGE", "▼ PAGE")
	case "answer":
		return "ANSWER ×2"
	case "workers":
		return "WORKERS"
	case "route":
		return strings.ToUpper(b.Role) + " MODEL"
	case "nothing":
		return ""
	}
	s := strings.ToUpper(b.Do)
	if need(b, false) {
		s += " ×2"
	}
	return s
}

// colours by what an action is about: answering, merges, moving around, settings.
func colour(do string) string {
	switch do {
	case "approve", "answer":
		return "#2e9e5b"
	case "refuse":
		return "#c8463d"
	case "hold", "lift", "rerun", "retry":
		return "#d08a1e"
	case "select", "scroll", "page", "pick", "open", "back", "project", "view", "refresh", "cancel":
		return "#3a74c4"
	case "workers", "route":
		return "#8a55c2"
	}
	return "#555"
}

// SVG draws the surface with what each control does and what each light
// shows: the picture in docs/README.md, made from the profile and mapping so
// it never drifts from them.
func SVG(p *surface.Profile, m *Mapping) string {
	w, h := p.Size[0], p.Size[1]+150
	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 %d %d" font-family="Helvetica, Arial, sans-serif">`+"\n", w, h, w, h)
	fmt.Fprintf(&b, `<rect x="0" y="0" width="%d" height="%d" fill="#f4f4f2"/>`+"\n", w, h)
	fmt.Fprintf(&b, `<rect x="0" y="0" width="%d" height="%d" rx="18" fill="#1c1c1f"/>`+"\n", w, p.Size[1])
	fmt.Fprintf(&b, `<text x="%d" y="22" fill="#ddd" font-size="14" text-anchor="middle">%s · hive deck default mapping</text>`+"\n", w/2, html.EscapeString(p.Name))
	for _, c := range p.Controls {
		bd, mapped := m.Controls[c.Name]
		fill, stroke := "#2c2c31", "#444"
		if mapped && bd.Do != "nothing" {
			fill = colour(bd.Do)
		}
		if _, ok := m.Lights[c.Name]; ok {
			stroke = "#ffe066"
		}
		x, y, cw, ch := c.At[0], c.At[1], c.Size[0], c.Size[1]
		if c.Shape == "circle" {
			fmt.Fprintf(&b, `<circle cx="%d" cy="%d" r="%d" fill="%s" stroke="%s" stroke-width="2"/>`+"\n", x, y, cw, fill, stroke)
		} else {
			fmt.Fprintf(&b, `<rect x="%d" y="%d" width="%d" height="%d" rx="5" fill="%s" stroke="%s" stroke-width="2"/>`+"\n", x, y, cw, ch, fill, stroke)
		}
		cx, cy := x+cw/2, y+ch/2
		if c.Shape == "circle" {
			cx, cy = x, y
		}
		label := c.Label
		if mapped {
			label = Short(bd)
		}
		if label == "" {
			continue
		}
		size, inside := 9, true
		switch {
		case c.Shape == "circle" && cw < 12: // a push in the middle of a knob: the key says what it does
			continue
		case c.Shape == "circle" && cw > 50: // a jog: above its touch plate
			size, cy = 13, y-cw*2/3
		case c.Shape == "circle" && c.Type != surface.Button: // a knob: its name under it
			size, inside, cy = 8, false, y+cw+9
		case c.Shape != "circle" && cw < 30: // a fader: its name over it
			inside, cy = false, y-6
		}
		lines := []string{label}
		if inside && c.Shape != "circle" && len(label) > 9 {
			lines = strings.Split(label, " ")
		}
		for i, l := range lines {
			fmt.Fprintf(&b, `<text x="%d" y="%d" fill="#fff" font-size="%d" text-anchor="middle">%s</text>`+"\n", cx, cy+3+(i*2-len(lines)+1)*size/2+i, size, html.EscapeString(l))
		}
		sig, lit := m.Lights[c.Name]
		switch {
		case lit && c.Shape == "circle":
			fmt.Fprintf(&b, `<text x="%d" y="%d" fill="#ffe066" font-size="7" text-anchor="middle">◉ %s</text>`+"\n", cx, cy+14, html.EscapeString(sig))
		case lit && ch >= 40:
			fmt.Fprintf(&b, `<text x="%d" y="%d" fill="#ffe066" font-size="7" text-anchor="middle">◉ %s</text>`+"\n", cx, y+ch-5, html.EscapeString(sig))
		}
	}
	// the key
	y := p.Size[1] + 24
	keys := [][2]string{{"approve", "answer: approve"}, {"refuse", "answer: refuse"}, {"hold", "merges and checks"}, {"view", "views and selection"}, {"workers", "settings (faders, knobs)"}}
	for i, k := range keys {
		x := 20 + i*195
		fmt.Fprintf(&b, `<rect x="%d" y="%d" width="14" height="14" rx="3" fill="%s"/><text x="%d" y="%d" fill="#222" font-size="12">%s</text>`+"\n", x, y, colour(k[0]), x+20, y+12, k[1])
	}
	y += 30
	fmt.Fprintf(&b, `<rect x="20" y="%d" width="14" height="14" rx="3" fill="none" stroke="#e0b000" stroke-width="2"/><text x="40" y="%d" fill="#222" font-size="12">lit: ◉ shows a signal (main.green, main.red, asks, gate, lead, held, connected, armed, the current view)</text>`+"\n", y, y+12)
	y += 26
	fmt.Fprintf(&b, `<text x="20" y="%d" fill="#222" font-size="12">×2: press twice within the confirm window; answers to anything always the person's always take the second press.</text>`+"\n", y+12)
	y += 22
	fmt.Fprintf(&b, `<text x="20" y="%d" fill="#222" font-size="12">Left jog: move the selection · right jog: scroll · browse: pick the project, push to refresh · CUE: cancel a press waiting for its confirm.</text>`+"\n", y+12)
	b.WriteString("</svg>\n")
	return b.String()
}
