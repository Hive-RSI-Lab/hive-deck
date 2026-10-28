package hive

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// SSH reaches a Hive on another machine as the Hive's own `hive remote` does:
// one ssh session runs `hive remote-info --follow` there, which prints the
// daemon's daemon.json and again whenever a restart changes it, and opens a
// SOCKS proxy (-D) on this machine's loopback through which the deck reaches
// the daemon's 127.0.0.1 port there. A restart on a new port and token needs
// no new tunnel; if ssh itself drops, the deck dials it again.
type SSH struct {
	Dest   string        // what ssh connects to: host, user@host or a Host from ~/.ssh/config
	Hive   string        // the hive command there; "hive" by default, found in ~/.local/bin too
	Cmd    []string      // the ssh command; "ssh" by default (ssh.exe on Windows)
	Redial time.Duration // the first wait before dialing again after ssh drops; 1s by default

	mu     sync.Mutex
	info   Info   // the daemon.json the Hive there last printed
	proxy  string // ssh's SOCKS proxy, 127.0.0.1:port
	live   bool   // a session is up and its proxy listens
	err    error  // why the last session ended, while there is none
	stdin  io.Closer
	cmd    *exec.Cmd
	ended  <-chan struct{} // closed when the current ssh exits
	closed chan struct{}
}

// RemoteCommand is what ssh runs there: the installer puts hive in
// ~/.local/bin, which a non-interactive SSH session often leaves off PATH.
func RemoteCommand(hive string) string {
	if hive == "" {
		hive = "hive"
	}
	return `PATH="$HOME/.local/bin:$PATH" exec ` + shellQuote(hive) + ` remote-info --follow`
}

func shellQuote(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_./") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// DialSSH starts the session and returns a client once the Hive there has
// answered with its daemon.json. ssh asks for a password or passphrase on
// the terminal as usual, and its messages show on stderr until then. Later
// sessions run with BatchMode, so they don't ask on a screen the deck draws:
// a key or an agent lets the deck redial on its own.
func DialSSH(s *SSH) (*Client, error) {
	s.closed = make(chan struct{})
	ended, err := s.session(true)
	if err != nil {
		return nil, err
	}
	go s.redial(ended)
	return &Client{ssh: s, http: &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{Proxy: s.proxyURL}}}, nil
}

// session runs ssh once. It returns when the Hive there has printed its
// daemon.json and ssh's proxy listens, with a channel closed when ssh exits.
func (s *SSH) session(first bool) (<-chan struct{}, error) {
	port, err := freePort()
	if err != nil {
		return nil, err
	}
	argv := s.Cmd
	if len(argv) == 0 {
		argv = []string{"ssh"}
	}
	argv = append([]string{}, argv...)
	argv = append(argv, "-D", "127.0.0.1:"+strconv.Itoa(port), "-o", "ExitOnForwardFailure=yes",
		"-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=3")
	if !first {
		argv = append(argv, "-o", "BatchMode=yes", "-o", "ConnectTimeout=20")
	}
	argv = append(argv, "--", s.Dest, RemoteCommand(s.Hive))
	cmd := exec.Command(argv[0], argv[1:]...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr tail
	if first {
		stderr.live = os.Stderr
	}
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("can't run ssh (OpenSSH's client; on Windows, the OpenSSH Client optional feature): %w", err)
	}
	got, ended := make(chan Info, 1), make(chan struct{})
	s.mu.Lock()
	s.cmd, s.stdin, s.ended = cmd, stdin, ended
	select {
	case <-s.closed: // Close came first: this session isn't wanted
		cmd.Process.Kill()
	default:
	}
	s.mu.Unlock()
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			var info Info
			if json.Unmarshal(sc.Bytes(), &info) != nil || info.Port == 0 {
				continue
			}
			s.mu.Lock()
			s.info = info
			s.mu.Unlock()
			select {
			case got <- info:
			default:
			}
		}
		werr := cmd.Wait()
		s.mu.Lock()
		s.live, s.err = false, s.why(werr, &stderr)
		s.mu.Unlock()
		close(ended)
	}()
	select {
	case <-got:
	case <-ended:
		s.mu.Lock()
		defer s.mu.Unlock()
		return nil, s.err
	}
	stderr.quiet() // the deck owns the screen from here
	proxy := "127.0.0.1:" + strconv.Itoa(port)
	// Wait for the proxy: ssh may print daemon.json before it listens.
	for end := time.Now().Add(10 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		if c, err := net.Dial("tcp", proxy); err == nil {
			c.Close()
			break
		} else if time.Now().After(end) {
			s.stop(cmd, stdin, ended)
			return nil, fmt.Errorf("ssh's proxy on %s didn't open: %w", proxy, err)
		}
	}
	s.mu.Lock()
	s.proxy, s.live, s.err = proxy, true, nil
	s.mu.Unlock()
	return ended, nil
}

// redial dials ssh again each time it drops, waiting longer between tries
// that fail, until Close.
func (s *SSH) redial(ended <-chan struct{}) {
	first := s.Redial
	if first <= 0 {
		first = time.Second
	}
	wait := first
	for {
		select {
		case <-s.closed:
			return
		case <-ended:
		}
		for {
			select {
			case <-s.closed:
				return
			case <-time.After(wait):
			}
			next, err := s.session(false)
			if err == nil {
				ended, wait = next, first
				break
			}
			s.mu.Lock()
			s.err = err
			s.mu.Unlock()
			if wait *= 2; wait > 30*time.Second {
				wait = 30 * time.Second
			}
		}
	}
}

// why says why a session closed, in ssh's or the Hive's last words.
func (s *SSH) why(err error, stderr *tail) error {
	all, why := stderr.all()
	switch {
	case strings.Contains(all, `unknown command "remote-info"`):
		return fmt.Errorf("the Hive on %s is older than `hive remote`; run its `hive update` first", s.Dest)
	case strings.Contains(all, "not found"):
		for _, l := range strings.Split(all, "\n") {
			if strings.Contains(l, "not found") {
				why = strings.TrimSpace(l)
			}
		}
		return fmt.Errorf("no hive command on %s (%s); name it with --hive <path>", s.Dest, why)
	case why != "":
		return fmt.Errorf("ssh %s: %s", s.Dest, why)
	case err != nil:
		return fmt.Errorf("ssh %s: %w", s.Dest, err)
	}
	return fmt.Errorf("ssh %s ended", s.Dest)
}

// Info is the daemon.json the Hive there last printed, or, while ssh is
// down, why it dropped.
func (s *SSH) Info() (Info, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.live {
		err := errors.New("no session")
		if s.err != nil {
			err = s.err
		}
		return s.info, fmt.Errorf("the ssh session to %s dropped (%v); the deck dials it again", s.Dest, err)
	}
	return s.info, nil
}

func (s *SSH) proxyURL(*http.Request) (*url.URL, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return &url.URL{Scheme: "socks5", Host: s.proxy}, nil
}

// Close ends the session and stops redialing: the Hive there stops
// following when its input closes.
func (s *SSH) Close() {
	s.mu.Lock()
	select {
	case <-s.closed:
		s.mu.Unlock()
		return
	default:
		close(s.closed)
	}
	cmd, stdin, ended := s.cmd, s.stdin, s.ended
	s.mu.Unlock()
	if cmd != nil {
		s.stop(cmd, stdin, ended)
	}
}

func (s *SSH) stop(cmd *exec.Cmd, stdin io.Closer, ended <-chan struct{}) {
	stdin.Close()
	select {
	case <-ended:
	case <-time.After(3 * time.Second):
		if cmd.Process != nil {
			cmd.Process.Kill()
		}
	}
}

func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

// tail passes ssh's stderr through until the deck takes the screen, and
// keeps its last lines for the error when the session ends.
type tail struct {
	mu   sync.Mutex
	live io.Writer
	buf  bytes.Buffer
}

func (w *tail) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.live != nil {
		w.live.Write(p)
	}
	w.buf.Write(p)
	if w.buf.Len() > 4096 {
		b := w.buf.Bytes()[w.buf.Len()-2048:]
		w.buf.Reset()
		w.buf.Write(b)
	}
	return len(p), nil
}

func (w *tail) quiet() {
	w.mu.Lock()
	w.live = nil
	w.mu.Unlock()
}

// all is what was kept, and its last non-empty line.
func (w *tail) all() (string, string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	s := strings.TrimSpace(w.buf.String())
	lines := strings.Split(s, "\n")
	return s, strings.TrimSpace(lines[len(lines)-1])
}
