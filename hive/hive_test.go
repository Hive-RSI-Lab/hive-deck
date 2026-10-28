package hive

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// daemon is a stand-in for the Hive's daemon: the person's token, and a log of calls.
type daemon struct {
	token string
	calls []string
	op    bool // has /api/op
	ui    bool // has /api/ui
	cfg   map[string]any
}

func (d *daemon) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+d.token {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	var body map[string]any
	json.NewDecoder(r.Body).Decode(&body)
	raw, _ := json.Marshal(body)
	d.calls = append(d.calls, r.Method+" "+r.URL.RequestURI()+" "+string(raw))
	switch r.URL.Path {
	case "/api/state":
		fmt.Fprint(w, `{"seq":5,"asks":[{"id":"a1","kind":"merge","options":["merge","leave open"]},{"id":"a2","answer":"yes"}],"projects":[{"name":"deck","scores":[{"tests_ok":true,"at":"2026-10-24T10:00:00Z"},{"tests_ok":false,"no_result":true,"at":"2026-10-24T11:00:00Z"}]}],"config":{"max_workers":3}}`)
	case "/api/op":
		if !d.op {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"routed":"worker"}`)
	case "/api/ui":
		if !d.ui {
			http.NotFound(w, r)
			return
		}
		if r.Method == http.MethodPost {
			fmt.Fprint(w, `{"seq":13,"action":"down"}`)
			return
		}
		fmt.Fprint(w, `{"seq":12,"commands":[],"view":{"by":"tower pid 4242","view":"waiting","focus":"main","sel":1,"rows":3,"item":"a1","title":"Merge?","options":["merge","leave open"],"option":1,"applied":12}}`)
	case "/api/config":
		if r.Method == http.MethodPost {
			d.cfg = body
		}
		fmt.Fprint(w, `{"max_workers":3,"routes":{"lead":["claude:opus"]},"sandbox":"auto"}`)
	case "/api/answer":
		if body["answer"] == "nope" {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":"\"nope\" isn't an answer to this ask"}`)
			return
		}
		fmt.Fprint(w, `{}`)
	default:
		fmt.Fprint(w, `"ok"`)
	}
}

func TestClientCallsThePersonsAPI(t *testing.T) {
	d := &daemon{token: "secret"}
	srv := httptest.NewServer(d)
	defer srv.Close()
	c := Remote(srv.URL+"/", "secret\n")
	st, err := c.State()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Pending()) != 1 || st.Projects[0].Main() != "green" || st.Config.MaxWorkers != 3 {
		t.Fatalf("state: %+v", st)
	}
	if err := c.Answer("a1", "merge", "from the deck"); err != nil {
		t.Fatal(err)
	}
	if err := c.Answer("a1", "nope", ""); err == nil || !strings.Contains(err.Error(), "isn't an answer") {
		t.Fatalf("the daemon's error should come through: %v", err)
	}
	c.Hold("deck", "held from the deck")
	c.Lift("deck", "lifted")
	c.Retry("t1")
	if _, err := c.Op("route", map[string]any{"role": "worker"}); !errors.Is(err, ErrNoOp) {
		t.Fatalf("a daemon without /api/op: %v", err)
	}
	d.op = true
	if out, err := c.Op("route", map[string]any{"role": "worker"}); err != nil || !strings.Contains(string(out), "routed") {
		t.Fatalf("op: %s %v", out, err)
	}
	if err := c.SetConfig(func(m map[string]any) { m["max_workers"] = 5 }); err != nil {
		t.Fatal(err)
	}
	if d.cfg["max_workers"] != float64(5) || d.cfg["sandbox"] != "auto" || d.cfg["routes"] == nil {
		t.Fatalf("the config written back should keep what the deck didn't touch: %v", d.cfg)
	}
	c.Note("the person set max_workers from 3 to 5 from the deck")
	want := []string{
		`POST /api/answer {"answer":"merge","id":"a1","note":"from the deck"}`,
		`POST /api/hold {"project":"deck","why":"held from the deck"}`,
		`POST /api/hold {"lift":true,"project":"deck","why":"lifted"}`,
		`POST /api/task/retry {"id":"t1"}`,
		`POST /api/module/report {"from":"deck","text":"the person set max_workers from 3 to 5 from the deck"}`,
	}
	all := strings.Join(d.calls, "\n")
	for _, w := range want {
		if !strings.Contains(all, w) {
			t.Errorf("no call %s in\n%s", w, all)
		}
	}
	if bad := Remote(srv.URL, "wrong"); bad.Answer("a1", "merge", "") == nil {
		t.Fatal("a wrong token got through")
	}
}

func TestClientDrivesTheControlCenter(t *testing.T) {
	d := &daemon{token: "secret"}
	srv := httptest.NewServer(d)
	defer srv.Close()
	c := Remote(srv.URL, "secret")
	if _, err := c.UIView(); !errors.Is(err, ErrNoUI) {
		t.Fatalf("a daemon without /api/ui: %v", err)
	}
	if _, err := c.UI(UICommand{Action: "down"}); !errors.Is(err, ErrNoUI) {
		t.Fatalf("a daemon without /api/ui: %v", err)
	}
	d.ui = true
	v, err := c.UIView()
	if err != nil || v == nil || v.View != "waiting" || v.Item != "a1" || v.Option != 1 || len(v.Options) != 2 {
		t.Fatalf("view: %+v %v", v, err)
	}
	if seq, err := c.UI(UICommand{Action: "down", N: 3, Client: "deck", Control: "jog.l"}); err != nil || seq != 13 {
		t.Fatal(seq, err)
	}
	if _, err := c.UI(UICommand{Action: "view", View: "agents", Client: "deck", Control: "pad.r.4"}); err != nil {
		t.Fatal(err)
	}
	all := strings.Join(d.calls, "\n")
	for _, w := range []string{
		`GET /api/ui null`,
		`POST /api/ui {"action":"down","client":"deck","control":"jog.l","n":3}`,
		`POST /api/ui {"action":"view","client":"deck","control":"pad.r.4","view":"agents"}`,
	} {
		if !strings.Contains(all, w) {
			t.Errorf("no call %s in\n%s", w, all)
		}
	}
}

func TestLocalFollowsARestart(t *testing.T) {
	d := &daemon{token: "old"}
	srv := httptest.NewServer(d)
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	ws := t.TempDir()
	priv := filepath.Join(ws, ".hive", "private")
	os.MkdirAll(priv, 0o700)
	write := func(tok string) {
		os.WriteFile(filepath.Join(priv, "daemon.json"), []byte(fmt.Sprintf(`{"pid":1,"port":%s,"token":%q}`, u.Port(), tok)), 0o600)
	}
	write("old")
	c, err := Local(ws)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.State(); err != nil {
		t.Fatal(err)
	}
	d.token = "new" // the daemon restarted with a new token
	write("new")
	if _, err := c.State(); err != nil {
		t.Fatalf("should re-read daemon.json: %v", err)
	}
	if _, err := Local(t.TempDir()); err == nil || !strings.Contains(err.Error(), "isn't running") {
		t.Fatalf("no daemon.json: %v", err)
	}
}

func TestWorkspace(t *testing.T) {
	t.Setenv("HIVE_HOME", "/somewhere/hive")
	if Workspace() != "/somewhere/hive" {
		t.Fatal(Workspace())
	}
}
