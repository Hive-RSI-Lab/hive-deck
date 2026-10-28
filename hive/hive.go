// Package hive is the deck's view of the person's Hive: the slice of the
// daemon's HTTP API the deck reads and calls, with the person's own token.
// Locally it reads the daemon's address and token from the workspace, as the
// control center does, and follows a restart; on another machine it follows
// the daemon over one ssh session (DialSSH), or takes an address and a token.
package hive

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// State is the part of GET /api/state the deck shows and lights.
type State struct {
	Version  string    `json:"version"`
	Seq      int64     `json:"seq"`
	Projects []Project `json:"projects"`
	Runs     []Run     `json:"runs"`
	Asks     []Ask     `json:"asks"`
	Alerts   []Alert   `json:"alerts"`
	Checks   struct {
		Running []Slot `json:"running"`
		Waiting []Slot `json:"waiting"`
	} `json:"checks"`
	Chats  []Chat `json:"chats"`
	Config Config `json:"config"`
}

// Project is a project with its tasks, scores and merge hold.
type Project struct {
	Name   string        `json:"name"`
	Tasks  []Task        `json:"tasks"`
	Scores []Measurement `json:"scores"`
	Hold   *Hold         `json:"hold,omitempty"`
}

// Main is the project's main branch as its last measure found it: "green",
// "red", or "" when it has none yet.
func (p Project) Main() string {
	var last *Measurement
	for i := range p.Scores {
		m := &p.Scores[i]
		if m.NoResult {
			continue
		}
		if last == nil || !m.At.Before(last.At) {
			last = m
		}
	}
	switch {
	case last == nil:
		return ""
	case last.TestsOK:
		return "green"
	}
	return "red"
}

// Task is a piece of work.
type Task struct {
	ID      string               `json:"id"`
	Project string               `json:"project"`
	Title   string               `json:"title"`
	Stage   string               `json:"stage"`
	Reason  string               `json:"reason,omitempty"`
	SHA     string               `json:"sha,omitempty"`
	PR      struct{ Number int } `json:"pr"`
	Created time.Time            `json:"created"`
}

// Measurement is one measure of main.
type Measurement struct {
	SHA      string    `json:"sha"`
	TestsOK  bool      `json:"tests_ok"`
	NoResult bool      `json:"no_result,omitempty"`
	At       time.Time `json:"at"`
}

// Hold is the person's hold on a project's merges.
type Hold struct {
	Why string `json:"why"`
	By  string `json:"by"`
}

// Run is an agent.
type Run struct {
	ID       string `json:"id"`
	Role     string `json:"role"`
	Project  string `json:"project"`
	Status   string `json:"status"`
	Progress string `json:"progress,omitempty"`
}

// Ask is one Waiting item.
type Ask struct {
	ID      string    `json:"id"`
	Kind    string    `json:"kind"`
	Title   string    `json:"title"`
	Detail  string    `json:"detail"`
	Options []string  `json:"options"`
	Ref     string    `json:"ref,omitempty"`
	From    string    `json:"from,omitempty"`
	Project string    `json:"project,omitempty"`
	Answer  string    `json:"answer,omitempty"`
	Asked   time.Time `json:"asked"`
}

// Alert is a ledger alert.
type Alert struct {
	Kind string `json:"kind"`
	Text string `json:"text"`
}

// Slot is a check holding or waiting for a slot.
type Slot struct {
	Project string `json:"project"`
	SHA     string `json:"sha"`
	Kind    string `json:"kind"`
}

// Chat is one chat in the list.
type Chat struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	Unread     int    `json:"unread"`
	LeadStatus string `json:"lead_status,omitempty"`
}

// Message is one message in a chat.
type Message struct {
	ID     string    `json:"id"`
	From   string    `json:"from"`
	Text   string    `json:"text"`
	Thread string    `json:"thread,omitempty"`
	At     time.Time `json:"at"`
}

// Event is one journal entry.
type Event struct {
	Seq  int64           `json:"seq"`
	Kind string          `json:"kind"`
	At   time.Time       `json:"at"`
	Data json.RawMessage `json:"data"`
}

// Config is the settings the deck turns. The whole config.json is kept as
// read, so writing it back changes only what the deck set.
type Config struct {
	MaxWorkers   int                 `json:"max_workers"`
	Routes       map[string][]string `json:"routes,omitempty"`
	ModelChoices []string            `json:"model_choices,omitempty"`
	Models       map[string]any      `json:"models,omitempty"`
}

// Pending is what still waits for an answer, in the order the Hive lists it.
func (s State) Pending() []Ask {
	var out []Ask
	for _, a := range s.Asks {
		if a.Answer == "" {
			out = append(out, a)
		}
	}
	return out
}

// Hive is what the deck does with the Hive; Client is the real one.
type Hive interface {
	State() (State, error)
	Events(after int64) ([]Event, error)
	Messages(chat string) ([]Message, error)
	Answer(id, answer, note string) error
	Hold(project, why string) error
	Lift(project, why string) error
	Retry(task string) error
	// Op runs one of the lead's ops as the person, journaled as the lead's
	// are (POST /api/op). ErrNoOp means this Hive doesn't have that seam.
	Op(name string, args map[string]any) (json.RawMessage, error)
	// SetConfig changes config.json as the control center's Settings does:
	// read, change, write back.
	SetConfig(change func(map[string]any)) error
	// Note puts a line on the Hive's journal (module.reported) saying what
	// the deck did, for changes that have no op of their own yet.
	Note(text string) error
	// UI sends a command to the open control center (POST /api/ui); ErrNoUI
	// means this Hive doesn't have that seam.
	// It returns the command's seq, which the view's Applied reaches once
	// the control center has acted on it.
	UI(cmd UICommand) (int64, error)
	// UIView is where the control center is, nil if none has said.
	UIView() (*UIView, error)
}

// ErrNoOp is returned when the daemon has no /api/op.
var ErrNoOp = errors.New("this Hive has no /api/op yet")

// Info is the daemon's address and token.
type Info struct {
	Port  int    `json:"port"`
	Token string `json:"token"`
	URL   string `json:"-"`
}

// Client talks to the daemon.
type Client struct {
	mu       sync.Mutex
	info     Info
	infoFile string // local: re-read when the daemon restarts with a new token
	ssh      *SSH   // over ssh: the daemon.json the Hive there last printed
	http     *http.Client
}

// Workspace finds the Hive's folder as the hive command does: $HIVE_HOME,
// the pointer file the installer writes, or ~/hive.
func Workspace() string {
	if v := os.Getenv("HIVE_HOME"); v != "" {
		return v
	}
	if dir, err := os.UserConfigDir(); err == nil {
		if raw, err := os.ReadFile(filepath.Join(dir, "hive", "workspace")); err == nil {
			if root := strings.TrimSpace(string(raw)); root != "" {
				return root
			}
		}
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "hive")
}

// Local is the daemon running on this machine, from <workspace>/.hive/private/daemon.json.
func Local(workspace string) (*Client, error) {
	c := &Client{infoFile: filepath.Join(workspace, ".hive", "private", "daemon.json"), http: &http.Client{Timeout: 30 * time.Second}}
	info, err := c.readInfo()
	if err != nil {
		return nil, err
	}
	c.info = info
	return c, nil
}

// Remote is a daemon at url (http://host:port) with the person's token.
func Remote(url, token string) *Client {
	return &Client{info: Info{URL: strings.TrimRight(url, "/"), Token: strings.TrimSpace(token)}, http: &http.Client{Timeout: 30 * time.Second}}
}

func (c *Client) readInfo() (Info, error) {
	var info Info
	raw, err := os.ReadFile(c.infoFile)
	if err != nil {
		return info, fmt.Errorf("the Hive isn't running here (no %s); start it, or pass --ssh host for one elsewhere", c.infoFile)
	}
	if err := json.Unmarshal(raw, &info); err != nil {
		return info, fmt.Errorf("%s: %v", c.infoFile, err)
	}
	return info, nil
}

func (i Info) base() string {
	if i.URL != "" {
		return i.URL
	}
	return fmt.Sprintf("http://127.0.0.1:%d", i.Port)
}

type statusErr struct {
	code int
	msg  string
}

func (e *statusErr) Error() string { return e.msg }

func (c *Client) call(method, path string, body, out any) error {
	info, err := c.current()
	if err != nil {
		return err // the ssh session dropped: say so, not a refused request
	}
	err = c.do(info, method, path, body, out)
	var se *statusErr
	if errors.As(err, &se) && se.code == http.StatusUnauthorized || dialFailed(err) {
		// A restarted daemon has a new port or token; daemon.json names it.
		// A refused or unreachable request did nothing, so sending it again is safe.
		if next, e := c.next(); e == nil && next != info {
			err = c.do(next, method, path, body, out)
		}
	}
	if c.ssh != nil && dialFailed(err) {
		if _, e := c.ssh.Info(); e != nil {
			return e
		}
		var op *net.OpError
		if errors.As(err, &op) && op.Op == "proxyconnect" { // ssh's proxy is gone before ssh is seen to end
			return fmt.Errorf("the ssh session to %s dropped; the deck dials it again", c.ssh.Dest)
		}
		return fmt.Errorf("the Hive on %s isn't answering (restarting, or stopped); the deck carries on when it's back", c.ssh.Dest)
	}
	return err
}

// current is the daemon to send to.
func (c *Client) current() (Info, error) {
	if c.ssh != nil {
		return c.ssh.Info()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.info, nil
}

// next is the daemon as daemon.json names it now.
func (c *Client) next() (Info, error) {
	switch {
	case c.ssh != nil:
		return c.ssh.Info()
	case c.infoFile != "":
		info, err := c.readInfo()
		if err == nil {
			c.mu.Lock()
			c.info = info
			c.mu.Unlock()
		}
		return info, err
	}
	return Info{}, errors.New("no daemon.json to follow")
}

// dialFailed: the request never reached the daemon (nothing listens on its
// port, ssh's proxy couldn't connect to it, or the proxy is gone), so it can
// be sent again. A
// request that went out and lost its answer is not: it may have been acted on.
func dialFailed(err error) bool {
	var op *net.OpError
	return errors.As(err, &op) && (op.Op == "dial" || op.Op == "socks connect" || op.Op == "proxyconnect")
}

func (c *Client) do(info Info, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, info.base()+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+info.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		var e struct{ Error string }
		msg := fmt.Sprintf("%s %s: %s", method, path, resp.Status)
		if json.Unmarshal(raw, &e) == nil && e.Error != "" {
			msg = e.Error
		}
		if resp.StatusCode == http.StatusUnauthorized {
			msg = "the Hive refused the token (unauthorized): is it the person's token from .hive/private/daemon.json?"
		}
		return &statusErr{resp.StatusCode, msg}
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

func (c *Client) State() (State, error) {
	var s State
	return s, c.call("GET", "/api/state", nil, &s)
}

func (c *Client) Events(after int64) ([]Event, error) {
	var evs []Event
	return evs, c.call("GET", fmt.Sprintf("/api/events?after=%d", after), nil, &evs)
}

func (c *Client) Messages(chat string) ([]Message, error) {
	var ms []Message
	return ms, c.call("GET", "/api/chat?id="+chat, nil, &ms)
}

func (c *Client) Answer(id, answer, note string) error {
	return c.call("POST", "/api/answer", map[string]string{"id": id, "answer": answer, "note": note}, nil)
}

func (c *Client) Hold(project, why string) error {
	return c.call("POST", "/api/hold", map[string]any{"project": project, "why": why}, nil)
}

func (c *Client) Lift(project, why string) error {
	return c.call("POST", "/api/hold", map[string]any{"project": project, "why": why, "lift": true}, nil)
}

func (c *Client) Retry(task string) error {
	return c.call("POST", "/api/task/retry", map[string]string{"id": task}, nil)
}

func (c *Client) Op(name string, args map[string]any) (json.RawMessage, error) {
	var out json.RawMessage
	err := c.call("POST", "/api/op", map[string]any{"op": name, "args": args}, &out)
	var se *statusErr
	if errors.As(err, &se) && se.code == http.StatusNotFound {
		return nil, ErrNoOp
	}
	return out, err
}

func (c *Client) SetConfig(change func(map[string]any)) error {
	var cfg map[string]any
	if err := c.call("GET", "/api/config", nil, &cfg); err != nil {
		return err
	}
	if e, _ := cfg["load_error"].(string); e != "" {
		return fmt.Errorf("config.json has to be fixed first: %s", e)
	}
	delete(cfg, "load_error")
	change(cfg)
	return c.call("POST", "/api/config", cfg, nil)
}

func (c *Client) Note(text string) error {
	return c.call("POST", "/api/module/report", map[string]string{"from": "deck", "text": text}, nil)
}

// UICommand is one command to the control center (POST /api/ui): the keys
// it stands for, as the person's own would be.
type UICommand struct {
	Seq     int64  `json:"seq,omitempty"`
	Action  string `json:"action"`            // up, down, left, right, pageup, pagedown, view, open, back, answer
	N       int    `json:"n,omitempty"`       // up, down, pageup, pagedown: how many steps
	View    string `json:"view,omitempty"`    // view: which one
	Answer  string `json:"answer,omitempty"`  // answer: an option of the selected Waiting item
	Client  string `json:"client,omitempty"`  // "deck", for the control center's hint line
	Control string `json:"control,omitempty"` // the control that sent it
}

// UIView is where the control center is, as it last reported (GET /api/ui).
type UIView struct {
	By      string    `json:"by,omitempty"`
	At      time.Time `json:"at"`
	View    string    `json:"view"`             // chats, threads, thread, dashboard, agents, waiting, feedback, knowledge, modules, settings
	Focus   string    `json:"focus"`            // sidebar, main or thread
	Chat    string    `json:"chat,omitempty"`   // the open chat
	Detail  string    `json:"detail,omitempty"` // the kind of an item opened on top (task, run, diff...)
	Sel     int       `json:"sel"`              // the selected row, -1 for none; in an open item, its scroll
	Rows    int       `json:"rows"`
	Item    string    `json:"item,omitempty"` // the selection's id
	Title   string    `json:"title,omitempty"`
	Options []string  `json:"options,omitempty"` // a Waiting item's answers
	Option  int       `json:"option"`            // the one picked
	Applied int64     `json:"applied"`
}

// ErrNoUI is returned when the daemon has no /api/ui (a Hive from before it).
var ErrNoUI = errors.New("this Hive has no /api/ui yet: run its hive update for the deck to drive the control center")

// UI sends one command to every open control center, and returns its seq.
func (c *Client) UI(cmd UICommand) (int64, error) {
	var out UICommand
	err := c.call("POST", "/api/ui", cmd, &out)
	var se *statusErr
	if errors.As(err, &se) && se.code == http.StatusNotFound {
		return 0, ErrNoUI
	}
	return out.Seq, err
}

// UIView is the control center's view as last reported; nil when none has
// reported since the daemon started.
func (c *Client) UIView() (*UIView, error) {
	var out struct {
		Seq  int64   `json:"seq"`
		View *UIView `json:"view"`
	}
	err := c.call("GET", "/api/ui", nil, &out)
	var se *statusErr
	if errors.As(err, &se) && se.code == http.StatusNotFound {
		return nil, ErrNoUI
	}
	return out.View, err
}
