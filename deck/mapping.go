package deck

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"

	"github.com/Hive-RSI-Lab/hive-deck/surface"
)

//go:embed default.json
var defaultMapping []byte

// DefaultMapping is the mapping the deck ships with, as JSON: copy it
// (`deck mapping > deck.json`), edit it, and run `deck --map deck.json`.
func DefaultMapping() []byte { return defaultMapping }

// Mapping ties a surface's named controls to Hive actions, and its lights to
// what they show. It names controls, never MIDI messages: the profile owns those.
type Mapping struct {
	Profile        string             `json:"profile"`         // a built-in profile name or a path
	ConfirmSeconds float64            `json:"confirm_seconds"` // how long a first press waits for its second
	SettleMillis   int                `json:"settle_ms"`       // how long a fader or knob rests before its value is sent
	Controls       map[string]Binding `json:"controls"`
	Lights         map[string]string  `json:"lights"` // control -> signal
}

// Binding is what one control does.
type Binding struct {
	Do      string   `json:"do"`
	Step    int      `json:"step,omitempty"`    // select: how far a press moves (default 1)
	View    string   `json:"view,omitempty"`    // view: which one
	Per     int      `json:"per,omitempty"`     // jog: ticks per step (default 4)
	Min     int      `json:"min,omitempty"`     // workers: the fader's bottom
	Max     int      `json:"max,omitempty"`     // workers: the fader's top (default 8)
	Role    string   `json:"role,omitempty"`    // route: which role
	Models  []string `json:"models,omitempty"`  // route: the choices along the knob; default the Hive's named models and model choices
	Confirm *bool    `json:"confirm,omitempty"` // a second press to confirm; answers to anything always the person's confirm regardless
}

// Actions, and the views and light signals the mapping may name.
var (
	Actions = map[string]string{
		"approve": "answer the selected Waiting item with its first option (yes, merge, run, install...)",
		"refuse":  "answer the selected Waiting item with its second option (no, leave open, decline...)",
		"select":  "move the selection in the current view (a press moves by step; a jog by its turn)",
		"scroll":  "scroll the feed, the selected thread, or the selected item's detail",
		"view":    "jump to a view: waiting, feed, threads, runs",
		"project": "pick the project that hold, lift, rerun, retry and the main light are about",
		"hold":    "hold the project's merges",
		"lift":    "lift the project's merge hold",
		"rerun":   "re-run the forge's failed checks on the selected task",
		"retry":   "retry the selected task (blocked or stopped)",
		"workers": "set the worker limit (max_workers) from a fader",
		"route":   "set a role's model route from a knob",
		"cancel":  "cancel a press waiting for its confirm",
		"refresh": "read the Hive's state now",
		"nothing": "do nothing (keeps a control quiet in dry runs)",
	}
	Views   = []string{"waiting", "feed", "threads", "runs"}
	Signals = map[string]string{
		"main.green":   "on while the project's main passed its last measure",
		"main.red":     "blinks while the project's main failed its last measure",
		"asks":         "blinks while anything waits on the person",
		"gate":         "on while a check runs; blinks while checks only wait for a slot",
		"lead":         "on while a lead is thinking (a turn in progress)",
		"connected":    "on while the Hive answers",
		"held":         "on while the project's merges are held",
		"approve":      "on when the selected item can be approved; fast while it waits for the confirm",
		"refuse":       "on when the selected item can be refused; fast while it waits for the confirm",
		"armed":        "fast while any press waits for its confirm",
		"view.waiting": "on in the waiting view", "view.feed": "on in the feed view",
		"view.threads": "on in the threads view", "view.runs": "on in the runs view",
	}
)

// LoadMapping reads a mapping file, or the default with path "".
func LoadMapping(path string) (*Mapping, error) {
	raw := defaultMapping
	if path != "" {
		var err error
		if raw, err = os.ReadFile(path); err != nil {
			return nil, err
		}
	}
	return ParseMapping(raw)
}

// ParseMapping reads a mapping and fills its defaults.
func ParseMapping(raw []byte) (*Mapping, error) {
	var m Mapping
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("mapping: %v", err)
	}
	if m.Profile == "" {
		m.Profile = "ddj-flx4"
	}
	if m.ConfirmSeconds <= 0 {
		m.ConfirmSeconds = 4
	}
	if m.SettleMillis <= 0 {
		m.SettleMillis = 600
	}
	return &m, nil
}

// Check refuses a mapping that names a control the profile lacks, an action
// or view or signal that doesn't exist, or a control type an action can't use.
func (m *Mapping) Check(p *surface.Profile) error {
	var errs []string
	for _, name := range sortedKeys(m.Controls) {
		b := m.Controls[name]
		c, ok := p.Control(name)
		if !ok {
			errs = append(errs, fmt.Sprintf("%s: %s has no control by that name", name, p.Name))
			continue
		}
		if _, ok := Actions[b.Do]; !ok {
			errs = append(errs, fmt.Sprintf("%s: no action %q (actions: %s)", name, b.Do, strings.Join(sortedKeys(Actions), ", ")))
			continue
		}
		want := map[string][]string{
			"workers": {surface.Absolute}, "route": {surface.Absolute},
			"project": {surface.Relative, surface.Button}, "scroll": {surface.Relative, surface.Button},
			"select": {surface.Relative, surface.Button},
		}[b.Do]
		if want == nil {
			want = []string{surface.Button}
		}
		if b.Do == "nothing" {
			want = []string{surface.Button, surface.Absolute, surface.Relative}
		}
		if !slices.Contains(want, c.Type) {
			errs = append(errs, fmt.Sprintf("%s: %s needs a %s control, and this is a %s", name, b.Do, strings.Join(want, " or "), c.Type))
		}
		if b.Do == "view" && !slices.Contains(Views, b.View) {
			errs = append(errs, fmt.Sprintf("%s: no view %q (views: %s)", name, b.View, strings.Join(Views, ", ")))
		}
		if b.Do == "route" && b.Role == "" {
			errs = append(errs, fmt.Sprintf("%s: route needs a role (lead, worker, reviewer, sweep, ...)", name))
		}
		if b.Do == "workers" && b.Max != 0 && b.Max <= b.Min {
			errs = append(errs, fmt.Sprintf("%s: workers max %d is not above min %d", name, b.Max, b.Min))
		}
	}
	for _, name := range sortedKeys(m.Lights) {
		if !p.Lit(name) {
			errs = append(errs, fmt.Sprintf("lights: %s has no light on %s", name, p.Name))
		}
		if _, ok := Signals[m.Lights[name]]; !ok {
			errs = append(errs, fmt.Sprintf("lights: %s: no signal %q (signals: %s)", name, m.Lights[name], strings.Join(sortedKeys(Signals), ", ")))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("mapping:\n  %s", strings.Join(errs, "\n  "))
	}
	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
