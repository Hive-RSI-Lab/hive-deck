// Command score is the deck project's score (.hive/project.json): the share
// of the FLX4 profile's controls a real device was heard on, across the
// `deck check` reports committed in checks/. The last line is the number,
// 0 to 1, higher better.
//
//	go run ./cmd/score [--dir checks] [--profile ddj-flx4]
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/Hive-RSI-Lab/hive-deck/check"
	"github.com/Hive-RSI-Lab/hive-deck/surface"
)

func main() {
	dir := flag.String("dir", "checks", "the folder of committed check reports")
	profile := flag.String("profile", "ddj-flx4", "the profile: a built-in name or a JSON file")
	flag.Parse()
	p, err := surface.Load(*profile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "score:", err)
		os.Exit(1)
	}
	heard, reports, err := check.Score(p, *dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "score:", err)
		os.Exit(1)
	}
	got := map[string]bool{}
	for _, h := range heard {
		got[h] = true
	}
	var missing []string
	for _, c := range p.Controls {
		if !got[c.Name] {
			missing = append(missing, c.Name)
		}
	}
	n := len(p.Controls)
	fmt.Printf("%d reports from a real %s in %s; heard %d of %d controls\n", reports, p.Name, *dir, len(heard), n)
	if len(missing) > 0 && len(heard) > 0 {
		fmt.Printf("not heard yet: %s\n", strings.Join(missing, ", "))
	}
	share := 0.0
	if n > 0 {
		share = float64(len(heard)) / float64(n)
	}
	fmt.Printf("%.4f\n", share)
}
