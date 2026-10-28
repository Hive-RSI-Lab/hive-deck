package check

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"

	"github.com/Hive-RSI-Lab/hive-deck/surface"
)

// Heard reads a report Report wrote (as pasted into the deck's chat and
// committed): the profile it was for, the device it ran on, and the controls
// it heard. ok is false when the text isn't a deck check report.
func Heard(report string) (profile, device string, heard []string, ok bool) {
	sc := bufio.NewScanner(strings.NewReader(report))
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		switch {
		case strings.HasPrefix(l, "## deck check: "):
			profile, ok = strings.TrimSpace(strings.TrimPrefix(l, "## deck check: ")), true
		case strings.HasPrefix(l, "- device: "):
			device = strings.TrimSpace(strings.TrimPrefix(l, "- device: "))
		case strings.HasPrefix(l, "|"):
			// | control | type | in the mapping | heard | by |
			f := strings.Split(strings.Trim(l, "|"), "|")
			if len(f) == 5 && strings.TrimSpace(f[3]) == "heard" && strings.TrimSpace(f[0]) != "control" {
				heard = append(heard, strings.TrimSpace(f[0]))
			}
		}
	}
	return profile, device, heard, ok
}

// Score is the share of p's controls that a real device was heard on, across
// every report in dir: a control heard in any of them counts. Reports for
// another profile, from no device or from a fake one, and controls the
// profile no longer has are left out.
func Score(p *surface.Profile, dir string) (heard []string, reports int, err error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.md"))
	if err != nil {
		return nil, 0, err
	}
	got := map[string]bool{}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			return nil, 0, err
		}
		profile, device, hs, ok := Heard(string(raw))
		if !ok || profile != p.Name || device == "" || strings.Contains(device, "(fake)") {
			continue
		}
		reports++
		for _, h := range hs {
			if _, ok := p.Control(h); ok {
				got[h] = true
			}
		}
	}
	for _, c := range p.Controls {
		if got[c.Name] {
			heard = append(heard, c.Name)
		}
	}
	return heard, reports, nil
}
