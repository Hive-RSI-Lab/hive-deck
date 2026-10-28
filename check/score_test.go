package check

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// report is a check's report as if it ran on the real deck, hearing every
// control but skip.
func report(t *testing.T, skip string) string {
	r, _ := check(t, everything(t, skip), "")
	return strings.Replace(r.Report(), "DDJ-FLX4 (fake)", "DDJ-FLX4", 1)
}

func TestHeardReadsTheReport(t *testing.T) {
	profile, device, heard, ok := Heard(report(t, "jog.l"))
	if !ok || profile != "Pioneer DDJ-FLX4" || device != "DDJ-FLX4" {
		t.Fatalf("got %v %q %q", ok, profile, device)
	}
	if want := len(flx4(t).Controls) - 1; len(heard) != want {
		t.Fatalf("heard %d, want %d", len(heard), want)
	}
	for _, h := range heard {
		if h == "jog.l" {
			t.Fatal("jog.l was not heard")
		}
	}
	if _, _, _, ok := Heard("# notes\n| a | b | c | heard | d |\n"); ok {
		t.Fatal("a file that isn't a report read as one")
	}
}

func TestScoreJoinsRealReports(t *testing.T) {
	p := flx4(t)
	dir := t.TempDir()
	write := func(name, s string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if heard, n, err := Score(p, dir); err != nil || n != 0 || len(heard) != 0 {
		t.Fatalf("empty: %v %d %v", heard, n, err)
	}
	write("README.md", "# check reports\n")
	fake, _ := check(t, everything(t, ""), "")
	write("fake.md", fake.Report())
	write("other.md", strings.Replace(report(t, ""), "## deck check: Pioneer DDJ-FLX4", "## deck check: Something Else", 1))
	if heard, n, _ := Score(p, dir); n != 0 || len(heard) != 0 {
		t.Fatalf("a fake device, another profile or a readme counted: %d reports, %d heard", n, len(heard))
	}
	write("a.md", report(t, "jog.l"))
	heard, n, _ := Score(p, dir)
	if n != 1 || len(heard) != len(p.Controls)-1 {
		t.Fatalf("one report: %d reports, %d heard", n, len(heard))
	}
	write("b.md", report(t, "jog.r"))
	heard, n, _ = Score(p, dir)
	if n != 2 || len(heard) != len(p.Controls) {
		t.Fatalf("two reports, each missing a different jog: %d reports, %d heard of %d", n, len(heard), len(p.Controls))
	}
}
