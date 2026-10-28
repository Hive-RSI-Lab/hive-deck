package hive

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestMain lets the test binary stand in for ssh (DECK_FAKE_SSH=1): it opens
// the -D SOCKS proxy, mapping the daemon ports "there" to real ones here, and
// prints the info file's daemon.json whenever it changes, as `hive
// remote-info --follow` does, until its input closes. It logs each dial's
// arguments, and drops the connection when the drop file appears.
func TestMain(m *testing.M) {
	if os.Getenv("DECK_FAKE_SSH") == "1" {
		os.Exit(fakeSSH(os.Args[1:]))
	}
	os.Exit(m.Run())
}

func fakeSSH(args []string) int {
	if log := os.Getenv("DECK_FAKE_SSH_LOG"); log != "" {
		f, _ := os.OpenFile(log, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		fmt.Fprintln(f, strings.Join(args, " "))
		f.Close()
	}
	if why := os.Getenv("DECK_FAKE_SSH_FAIL"); why != "" {
		fmt.Fprintln(os.Stderr, why)
		return 2
	}
	n := len(args)
	if n < 4 || args[0] != "-D" || args[n-3] != "--" || args[n-2] != "box" || args[n-1] != RemoteCommand("") {
		fmt.Fprintf(os.Stderr, "fake ssh: unexpected args %q\n", args)
		return 255
	}
	ports := map[string]string{} // there -> here
	for _, kv := range strings.Split(os.Getenv("DECK_FAKE_SSH_PORTS"), ",") {
		k, v, _ := strings.Cut(kv, "=")
		ports[k] = v
	}
	ln, err := net.Listen("tcp", args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 255
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go socks(c, ports)
		}
	}()
	closed := make(chan struct{})
	go func() { io.Copy(io.Discard, os.Stdin); close(closed) }()
	last := ""
	drop := os.Getenv("DECK_FAKE_SSH_DROP")
	for {
		if os.Remove(drop) == nil {
			fmt.Fprintln(os.Stderr, "client_loop: send disconnect: Connection reset")
			return 255
		}
		if raw, err := os.ReadFile(os.Getenv("DECK_FAKE_SSH_INFO")); err == nil && string(raw) != last {
			last = string(raw)
			fmt.Println(strings.TrimSpace(last))
		}
		select {
		case <-closed:
			return 0
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// socks serves one SOCKS5 CONNECT to 127.0.0.1, as ssh -D does.
func socks(c net.Conn, ports map[string]string) {
	defer c.Close()
	r := bufio.NewReader(c)
	head := make([]byte, 2)
	if _, err := io.ReadFull(r, head); err != nil {
		return
	}
	io.ReadFull(r, make([]byte, head[1]))
	c.Write([]byte{5, 0})
	req := make([]byte, 4)
	if _, err := io.ReadFull(r, req); err != nil || req[3] != 1 {
		return
	}
	addr := make([]byte, 6)
	io.ReadFull(r, addr)
	port := strconv.Itoa(int(binary.BigEndian.Uint16(addr[4:])))
	up, err := net.Dial("tcp", "127.0.0.1:"+ports[port])
	if err != nil {
		c.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0}) // connection refused
		return
	}
	defer up.Close()
	c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
	go io.Copy(up, r)
	io.Copy(c, up)
}

// remote is a Hive "there": its daemon.json, and the fake ssh set up to reach it.
type remote struct {
	dir, info, drop, log string
}

func newRemote(t *testing.T, ports map[int]int) *remote {
	dir := t.TempDir()
	r := &remote{dir: dir, info: filepath.Join(dir, "daemon.json"), drop: filepath.Join(dir, "drop"), log: filepath.Join(dir, "ssh.log")}
	var kv []string
	for there, here := range ports {
		kv = append(kv, fmt.Sprintf("%d=%d", there, here))
	}
	t.Setenv("DECK_FAKE_SSH", "1")
	t.Setenv("DECK_FAKE_SSH_INFO", r.info)
	t.Setenv("DECK_FAKE_SSH_DROP", r.drop)
	t.Setenv("DECK_FAKE_SSH_LOG", r.log)
	t.Setenv("DECK_FAKE_SSH_PORTS", strings.Join(kv, ","))
	return r
}

// restart writes daemon.json as a daemon starting on port with token does.
func (r *remote) restart(pid, port int, token string) {
	raw, _ := json.Marshal(map[string]any{"pid": pid, "port": port, "token": token, "version": "test"})
	os.WriteFile(r.info+".tmp", raw, 0o600)
	os.Rename(r.info+".tmp", r.info)
}

func (r *remote) dials() []string {
	raw, _ := os.ReadFile(r.log)
	return strings.Split(strings.TrimSpace(string(raw)), "\n")
}

func portOf(t *testing.T, srv *httptest.Server) int {
	_, p, _ := net.SplitHostPort(srv.Listener.Addr().String())
	n, _ := strconv.Atoi(p)
	return n
}

// eventually polls as the deck does until the Hive answers.
func eventually(t *testing.T, c *Client, what string) {
	t.Helper()
	var err error
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		if _, err = c.State(); err == nil {
			return
		}
	}
	t.Fatalf("%s: the Hive never answered: %v", what, err)
}

// TestSSHFollowsARestart: the deck reaches the Hive there through ssh's
// proxy at the port it has there, says so while it is down, and carries on
// when a restart brings it back on a new port and token, with no new ssh.
func TestSSHFollowsARestart(t *testing.T) {
	one := httptest.NewServer(&daemon{token: "one"})
	defer one.Close()
	two := &daemon{token: "two"}
	srv2 := httptest.NewServer(two)
	defer srv2.Close()
	r := newRemote(t, map[int]int{41001: portOf(t, one), 41002: portOf(t, srv2)})
	r.restart(1, 41001, "one")
	s := &SSH{Dest: "box", Cmd: []string{os.Args[0]}}
	c, err := DialSSH(s)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := c.State(); err != nil {
		t.Fatal("first daemon:", err)
	}

	one.Close() // the Hive goes down; daemon.json still names it
	if _, err := c.State(); err == nil || !strings.Contains(err.Error(), "isn't answering") {
		t.Fatalf("while the Hive is down: %v", err)
	}
	r.restart(2, 41002, "two")
	eventually(t, c, "after the restart")
	if err := c.Answer("a1", "merge", ""); err != nil || len(two.calls) < 2 {
		t.Fatalf("an answer after the restart: %v, calls %q", err, two.calls)
	}

	// A new token on the same port: the refused request goes again, once.
	r.restart(3, 41002, "three")
	two.token = "three"
	time.Sleep(200 * time.Millisecond)
	if _, err := c.State(); err != nil {
		t.Fatal("new token on the old port:", err)
	}
	if n := len(r.dials()); n != 1 {
		t.Fatalf("ssh was dialed %d times; one session should do", n)
	}
}

// TestSSHRedials: when ssh itself drops, the deck says so and dials again,
// without asking on the screen (BatchMode), and Close stops it.
func TestSSHRedials(t *testing.T) {
	d := httptest.NewServer(&daemon{token: "one"})
	defer d.Close()
	r := newRemote(t, map[int]int{41001: portOf(t, d)})
	r.restart(1, 41001, "one")
	s := &SSH{Dest: "box", Cmd: []string{os.Args[0]}, Redial: 300 * time.Millisecond}
	c, err := DialSSH(s)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(r.dials()[0], "BatchMode") {
		t.Fatalf("the first dial may ask for a password: %s", r.dials()[0])
	}

	os.WriteFile(r.drop, nil, 0o600)
	var seen error
	for end := time.Now().Add(2 * time.Second); time.Now().Before(end) && seen == nil; time.Sleep(10 * time.Millisecond) {
		if _, err := c.State(); err != nil {
			seen = err
		}
	}
	if seen == nil || !strings.Contains(seen.Error(), "dropped") {
		t.Fatalf("while ssh is down: %v", seen)
	}
	eventually(t, c, "after the redial")
	dials := r.dials()
	if len(dials) != 2 || !strings.Contains(dials[1], "BatchMode=yes") {
		t.Fatalf("dials: %q", dials)
	}

	s.Close()
	if _, err := c.State(); err == nil {
		t.Fatal("a closed session still reaches the Hive")
	}
	time.Sleep(700 * time.Millisecond)
	if n := len(r.dials()); n != 2 {
		t.Fatalf("ssh was dialed again after Close: %d dials", n)
	}
}

// TestSSHSaysWhy: a Hive there too old for remote-info, or no hive on its
// PATH, is named, not left as ssh's exit status.
func TestSSHSaysWhy(t *testing.T) {
	newRemote(t, nil)
	for why, want := range map[string]string{
		`unknown command "remote-info"`:         "older than `hive remote`",
		`bash: line 1: hive: command not found`: "--hive <path>",
		`ssh: Could not resolve hostname box`:   "Could not resolve",
	} {
		t.Setenv("DECK_FAKE_SSH_FAIL", why)
		_, err := DialSSH(&SSH{Dest: "box", Cmd: []string{os.Args[0]}})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", why, err)
		}
	}
}

func TestRemoteCommand(t *testing.T) {
	if got := RemoteCommand("/opt/hive bin/hive"); got != `PATH="$HOME/.local/bin:$PATH" exec '/opt/hive bin/hive' remote-info --follow` {
		t.Fatal(got)
	}
}
