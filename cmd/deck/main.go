// Command deck runs a USB MIDI controller (first: the Pioneer DDJ-FLX4) as a
// command surface for the Hive, on the machine it is plugged into.
//
//	deck                    run: open the deck and the Hive, act on each control
//	deck --dry-run          say what each control would do; send nothing to the Hive
//	deck list               the MIDI devices this machine sees
//	deck learn              print every message the deck sends, and its control's name
//	deck mapping            print the default mapping, to copy and edit
//	deck check              the first step on a new laptop: light every light, hear every
//	                        control, and report both against the profile (no Hive needed)
//	deck check --map FILE   check a mapping against the profile only (no device needed)
//	deck layout [--svg]     the mapping on the deck, as text or as a picture
//
// It talks to the Hive with the person's own token: on this machine it reads
// the daemon's address and token from the workspace as the control center
// does; for a Hive elsewhere pass --ssh host, and the deck follows it over
// one ssh session across its restarts (or --url and --token-file, for an
// address and token of your own).
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"

	"github.com/Hive-RSI-Lab/hive-deck/check"
	"github.com/Hive-RSI-Lab/hive-deck/deck"
	"github.com/Hive-RSI-Lab/hive-deck/hive"
	"github.com/Hive-RSI-Lab/hive-deck/midi"
	"github.com/Hive-RSI-Lab/hive-deck/surface"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "deck:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	cmd := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("deck", flag.ContinueOnError)
	mapPath := fs.String("map", "", "a mapping file (default: the built-in one; `deck mapping` prints it)")
	profile := fs.String("profile", "", "a surface profile: a built-in name or a JSON file (default: the mapping's)")
	device := fs.String("device", "", "the MIDI device's name, or part of it (default: the profile's)")
	dry := fs.Bool("dry-run", false, "say what each control would do and send nothing to the Hive")
	sshDest := fs.String("ssh", "", "a Hive elsewhere, over ssh: host, user@host or a Host from ~/.ssh/config")
	hiveCmd := fs.String("hive", "", "with --ssh: the hive command there (default: hive, on PATH or in ~/.local/bin)")
	url := fs.String("url", "", "a Hive elsewhere: its daemon's address, http://host:port")
	tokenFile := fs.String("token-file", "", "with --url: the person's token, or that Hive's daemon.json")
	workspace := fs.String("workspace", "", "this machine's Hive folder (default: as the hive command finds it)")
	plain := fs.Bool("plain", false, "print a line per action instead of drawing the screen")
	svg := fs.Bool("svg", false, "layout: draw the deck as SVG")
	out := fs.String("out", "deck-check.md", "check: where to write the report")
	wait := fs.Duration("wait", 5*time.Minute, "check: the longest to wait for the controls to be touched")
	step := fs.Duration("step", 300*time.Millisecond, "check: how long each light stays on in the walk")
	if err := fs.Parse(args); err != nil {
		return err
	}
	m, err := deck.LoadMapping(*mapPath)
	if err != nil {
		return err
	}
	if *profile != "" {
		m.Profile = *profile
	}
	p, err := surface.Load(m.Profile)
	if err != nil {
		return err
	}
	switch cmd {
	case "mapping":
		os.Stdout.Write(deck.DefaultMapping())
		return nil
	case "list":
		names, err := midi.List()
		if err != nil {
			return err
		}
		if len(names) == 0 {
			fmt.Println("no MIDI devices")
		}
		for _, n := range names {
			fmt.Println(n)
		}
		return nil
	}
	if err := m.Check(p); err != nil {
		return err
	}
	switch cmd {
	case "check":
		fmt.Printf("the mapping is good: %d controls and %d lights on the %s\n", len(m.Controls), len(m.Lights), p.Name)
		if *mapPath != "" {
			return nil
		}
		return checkDevice(p, m, pick(*device, p.Match), *out, *step, *wait)
	case "layout":
		if *svg {
			fmt.Print(deck.SVG(p, m))
			return nil
		}
		for _, l := range deck.New(m, p, nil, true).Describe() {
			fmt.Println(l)
		}
		return nil
	case "learn":
		return learn(p, pick(*device, p.Match))
	case "":
	default:
		return fmt.Errorf("no command %q: run, list, learn, mapping, check or layout", cmd)
	}

	if *sshDest != "" && *url != "" {
		return errors.New("--ssh or --url, not both")
	}
	h, done, herr := connect(*sshDest, *hiveCmd, *url, *tokenFile, *workspace)
	if herr != nil && !*dry {
		return herr
	}
	defer done()
	if herr != nil {
		fmt.Println("dry run without the Hive:", herr)
		h = offline{}
	}
	port, err := midi.Open(pick(*device, p.Match))
	if err != nil && !*dry {
		return err
	}
	var keys chan string
	if err != nil {
		fmt.Printf("dry run without the deck (%v): type a control's name, such as pad.l.1, \"jog.l +6\" or \"fader.l 0.5\"; empty line quits\n", err)
		port, keys = midi.NewFake("keyboard"), readKeys()
	}
	defer port.Close()

	e := deck.New(m, p, h, *dry)
	screen := !*plain && !*dry && isTerminal()
	if !screen {
		e.Out = func(s string) { fmt.Println(time.Now().Format("15:04:05"), s) }
	}
	if *dry {
		fmt.Println("what each control does:")
		for _, l := range e.Describe() {
			fmt.Println("  " + l)
		}
	}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt)
	quit := make(chan struct{})
	go func() { <-stop; close(quit) }()
	events := make(chan surface.Event)
	if keys != nil {
		go func() {
			defer close(events)
			for line := range keys {
				for _, ev := range typed(p, line) {
					events <- ev
				}
			}
		}()
	}
	l := deck.Loop{Port: port, Keys: events, Stop: quit,
		Warn: func(err error) { fmt.Fprintln(os.Stderr, "deck: lights:", err) }}
	if screen {
		l.Draw = func() {
			w, h := size()
			fmt.Print("\x1b[H\x1b[2J" + e.Screen(w, h))
		}
	}
	err = e.Run(l)
	if line := deck.LagLine(e.Lags()); line != "" {
		fmt.Println(line)
	}
	return err
}

func pick(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// connect reaches the Hive, and returns what ends the connection.
func connect(sshDest, hiveCmd, url, tokenFile, workspace string) (hive.Hive, func(), error) {
	switch {
	case sshDest != "":
		fmt.Fprintf(os.Stderr, "Connecting to the Hive on %s over ssh…\n", sshDest)
		s := &hive.SSH{Dest: sshDest, Hive: hiveCmd}
		c, err := hive.DialSSH(s)
		if err != nil {
			return nil, func() {}, err
		}
		return c, s.Close, nil
	case url != "":
		h, err := remote(url, tokenFile)
		return h, func() {}, err
	}
	h, err := hive.Local(pick(workspace, hive.Workspace()))
	return h, func() {}, err
}

func remote(url, tokenFile string) (hive.Hive, error) {
	tok := os.Getenv("HIVE_TOKEN")
	if tokenFile != "" {
		raw, err := os.ReadFile(tokenFile)
		if err != nil {
			return nil, err
		}
		var info hive.Info
		if json.Unmarshal(raw, &info) == nil && info.Token != "" {
			tok = info.Token // a copy of that Hive's daemon.json
		} else {
			tok = strings.TrimSpace(string(raw))
		}
	}
	if tok == "" {
		return nil, errors.New("--url needs the person's token: --token-file (that Hive's .hive/private/daemon.json, or the token alone) or $HIVE_TOKEN")
	}
	return hive.Remote(url, tok), nil
}

// learn prints each message with the control the profile reads it as, so a
// message the profile has wrong shows at once.
func learn(p *surface.Profile, device string) error {
	port, err := midi.Open(device)
	if err != nil {
		return err
	}
	defer port.Close()
	fmt.Printf("listening to %s; move each control (Ctrl-C ends)\n", port.Name())
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt)
	for {
		select {
		case <-stop:
			return nil
		case m, ok := <-port.In():
			if !ok {
				return nil
			}
			ev, known, ignored := p.Decode(m)
			switch {
			case known:
				fmt.Printf("%s  %s\n", m, ev)
			case ignored:
			default:
				fmt.Printf("%s  (not in the %s profile)\n", m, p.Name)
			}
		}
	}
}

// checkDevice is `deck check` on the device: it writes the report to out
// and fails if any control in the profile went unheard or a light stayed dark.
func checkDevice(p *surface.Profile, m *deck.Mapping, device, out string, step, wait time.Duration) error {
	port, err := midi.Open(device)
	if err != nil {
		return err
	}
	defer port.Close()
	used := map[string]bool{}
	for name := range m.Controls {
		used[name] = true
	}
	for name := range m.Lights {
		used[name] = true
	}
	lines := make(chan string)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(os.Stdin)
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()
	r := check.Run(check.Config{Profile: p, Port: port, Lines: lines, Out: os.Stdout, Used: used, Step: step, Wait: wait})
	rep := r.Report()
	fmt.Println()
	fmt.Print(rep)
	if err := os.WriteFile(out, []byte(rep), 0o644); err != nil {
		return err
	}
	fmt.Printf("\nthe report is in %s: paste it into the deck's chat\n", out)
	if missing := r.Missing(); len(missing) > 0 {
		return fmt.Errorf("%d of %d controls in the profile were never heard: %s", len(missing), len(p.Controls), strings.Join(missing, ", "))
	}
	if r.LightsBad {
		return errors.New("some lights didn't light")
	}
	return nil
}

func readKeys() chan string {
	ch := make(chan string)
	go func() {
		defer close(ch)
		sc := bufio.NewScanner(os.Stdin)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" {
				return
			}
			ch <- line
		}
	}()
	return ch
}

// typed reads "pad.l.1", "jog.l +6" or "fader.l 0.5" as the events the deck would send.
func typed(p *surface.Profile, line string) []surface.Event {
	f := strings.Fields(line)
	c, ok := p.Control(f[0])
	if !ok {
		fmt.Printf("no control %q on the %s\n", f[0], p.Name)
		return nil
	}
	arg := ""
	if len(f) > 1 {
		arg = f[1]
	}
	switch c.Type {
	case surface.Button:
		return []surface.Event{{Control: c.Name, Type: c.Type, Pressed: true}, {Control: c.Name, Type: c.Type}}
	case surface.Relative:
		n, _ := strconv.Atoi(arg)
		return []surface.Event{{Control: c.Name, Type: c.Type, Delta: n}}
	}
	v, _ := strconv.ParseFloat(arg, 64)
	return []surface.Event{{Control: c.Name, Type: c.Type, Value: v}}
}

func isTerminal() bool {
	fi, err := os.Stdout.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func size() (int, int) {
	w, _ := strconv.Atoi(os.Getenv("COLUMNS"))
	h, _ := strconv.Atoi(os.Getenv("LINES"))
	if w <= 0 {
		w = 100
	}
	if h <= 0 {
		h = 30
	}
	return w, h
}

// offline is a Hive that isn't there, for a dry run.
type offline struct{}

var errOffline = errors.New("not connected")

func (offline) State() (hive.State, error)                         { return hive.State{}, errOffline }
func (offline) Events(int64) ([]hive.Event, error)                 { return nil, errOffline }
func (offline) Messages(string) ([]hive.Message, error)            { return nil, errOffline }
func (offline) Answer(string, string, string) error                { return errOffline }
func (offline) Hold(string, string) error                          { return errOffline }
func (offline) Lift(string, string) error                          { return errOffline }
func (offline) Retry(string) error                                 { return errOffline }
func (offline) Op(string, map[string]any) (json.RawMessage, error) { return nil, errOffline }
func (offline) SetConfig(func(map[string]any)) error               { return errOffline }
func (offline) Note(string) error                                  { return errOffline }
func (offline) UI(hive.UICommand) (int64, error)                   { return 0, errOffline }
func (offline) UIView() (*hive.UIView, error)                      { return nil, errOffline }
