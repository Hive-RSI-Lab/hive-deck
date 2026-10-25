//go:build linux

package midi

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// On Linux a USB MIDI device is an ALSA raw MIDI node, /dev/snd/midiC<card>D<dev>,
// which reads and writes plain MIDI bytes: no library and no cgo.

type rawPort struct {
	name string
	f    *os.File
	in   chan Msg
	mu   sync.Mutex
}

type node struct{ path, name string }

func nodes() []node {
	paths, _ := filepath.Glob("/dev/snd/midiC*D*")
	sort.Strings(paths)
	var out []node
	for _, p := range paths {
		var card, dev int
		if _, err := fmt.Sscanf(filepath.Base(p), "midiC%dD%d", &card, &dev); err != nil {
			continue
		}
		id := readLine(fmt.Sprintf("/proc/asound/card%d/id", card))
		name := readLine(fmt.Sprintf("/proc/asound/card%d/midi%d", card, dev))
		out = append(out, node{p, strings.TrimSpace(name + " (" + id + ", " + filepath.Base(p) + ")")})
	}
	return out
}

func readLine(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	line, _, _ := strings.Cut(string(raw), "\n")
	return strings.TrimSpace(line)
}

func list() ([]string, error) {
	var out []string
	for _, n := range nodes() {
		out = append(out, n.name)
	}
	return out, nil
}

func open(want string) (Port, error) {
	for _, n := range nodes() {
		if !Match(n.name, want) {
			continue
		}
		f, err := os.OpenFile(n.path, os.O_RDWR, 0)
		if err != nil {
			return nil, fmt.Errorf("%s: %w (is another program, such as rekordbox or Mixxx, holding it? Is your user in the audio group?)", n.path, err)
		}
		p := &rawPort{name: n.name, f: f, in: make(chan Msg, 256)}
		go p.read()
		return p, nil
	}
	return nil, fmt.Errorf("%w matching %q under /dev/snd (is it plugged in? `deck list` shows what is there)", ErrNoDevice, want)
}

func (p *rawPort) read() {
	defer close(p.in)
	var parse Parser
	buf := make([]byte, 256)
	for {
		n, err := p.f.Read(buf)
		for _, m := range parse.Feed(buf[:n]) {
			p.in <- m
		}
		if err != nil {
			return
		}
	}
}

func (p *rawPort) Name() string   { return p.name }
func (p *rawPort) In() <-chan Msg { return p.in }
func (p *rawPort) Close() error   { return p.f.Close() }

func (p *rawPort) Send(m Msg) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, err := p.f.Write(m[:1+dataLen(m[0])])
	return err
}
