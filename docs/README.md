# deck: the DDJ-FLX4 drives the Hive

`deck` is a small program that runs on the laptop the controller is plugged into, next to the control center. It opens a Pioneer DDJ-FLX4 as a USB MIDI device (Linux, macOS, Windows) and talks to your Hive through the daemon's API with your own token. Pads answer what's waiting, hold or lift merges, retry work and jump between views. The jogs move the selection and scroll. A fader and two knobs set the worker limit and model routes. The pad lights show the Hive's state.

It's an optional module and isn't installed by default.

![The default layout on the DDJ-FLX4](layout.svg)

*(Drawn from the default mapping by `deck layout --svg`. A test fails if the picture and the mapping drift apart.)*

## The default layout

| Control | Does | Light |
|---|---|---|
| Left pad 1 | **approve** the selected Waiting item: its first option (yes, merge, run, install…). Press twice. | on when there's something to approve; fast while waiting for the second press |
| Left pad 2 | **refuse** it: its second option (no, leave open, decline…). Press twice. | the same |
| Left pads 3 / 4 | previous / next Waiting item | blink while anything waits on you |
| Left pad 5 | **hold** the project's merges | on while held |
| Left pad 6 | **lift** the hold (press twice) | on while held |
| Left pad 7 | **re-run** the failed checks on the selected item's task, or the project's newest open PR (press twice) | |
| Left pad 8 | **retry** the selected item's task, or the project's newest blocked or stopped one (press twice) | |
| Right pads 1–4 | jump to the **waiting / feed / threads / runs** view | on for the current view |
| Right pad 5 | jump to the feed | **main green**: on while the project's main passed its last measure |
| Right pad 6 | jump to the feed | **main red**: blinks while main failed its last measure |
| Right pad 7 | jump to runs | **gate**: on while a check runs; blinks while checks wait for a slot |
| Right pad 8 | jump to threads | **lead**: on while a lead is thinking |
| Left jog | move the selection (Waiting items, threads, runs; scrolls the feed) | |
| Right jog | scroll the selected item's detail, the thread, or the feed | |
| Browse knob | pick the project for hold, lift, re-run, retry and the main lights | |
| Browse push | read the Hive now | |
| CUE (either deck) | cancel a press that's waiting for its second press | blinks fast while one waits |
| PLAY left | | **connected**: on while the Hive answers |
| PLAY right | | **lead** thinking |
| Channel fader 1 | **worker limit**, 0 at the bottom to 8 at the top | |
| HI knob, channel 1 | the **worker** role's model route | |
| HI knob, channel 2 | the **lead** role's model route | |

Every pad does the same thing in any pad mode (HOT CUE, PAD FX, BEAT JUMP, SAMPLER…), because the profile lists each pad's note in every mode.

### The second press

Approve and refuse arm on the first press. The pad blinks fast, and the deck's screen says what the second press will do. A second press of the **same pad** on the **same item** within 4 seconds (`confirm_seconds`) sends the answer. Any of these cancels the armed press:

- another button
- moving the selection
- CUE
- the item being answered elsewhere
- the time running out

Anything that's always yours (a merge, a step past an agent's reach, a release, a request, a login, a route, an install, and any kind the deck doesn't know yet) **always** takes the second press, whatever the mapping says. The one exception is a plain `question`: setting `"confirm": false` in the mapping makes that a one-touch answer. Lift, re-run and retry take the second press in the default mapping too (`"confirm": true`).

### Faders and knobs

- **No jumps.** A fader or knob does nothing until it reaches the value the Hive holds now (soft takeover, as DJ software does it). A fader left at the top can't jump the worker limit from 3 to 8 when the deck starts. The screen says where to move it.
- **One change per move.** The value is sent once the control has rested for 600 ms (`settle_ms`), so a sweep sends one change and not forty.
- **Route stops.** A route knob's first stop is "no route", which uses the model settings. The rest are your Hive's named models and `model_choices`, or the list you give the binding in `models`.

### How changes reach the Hive

| Control | Endpoint |
|---|---|
| Answers | `/api/answer`, the control center's Waiting call |
| Hold and lift | `/api/hold`. The daemon takes that only with your token, never a module's. |
| Retry | `/api/task/retry` |

The worker limit, routes and re-running checks should go **through the same ops the lead uses**, so they're journaled the same way. The deck calls them at `POST /api/op {"op": "workers" | "route" | "rerun_checks", "args": {...}}`. Today's daemon doesn't have that endpoint yet. A brief to add it is filed on the Hive. Until it lands:

- **Worker limit and routes** go through Settings' own route: `/api/config`, read, change, write back. Each change also puts a line on the journal (`module.reported`: "the person set max_workers from 3 to 5 from the deck").
- **Re-running checks** says it needs `/api/op`. Retry works today.

## The screen

`deck` draws its own view in the terminal. The header shows the project, main, Waiting, checks and the worker limit. Below that are tabs for waiting, feed, threads and runs, the selection, and what the deck last did. The jogs move this view. The control center keeps its own, and a seam for the deck to drive the control center's views directly would be a later change to the Hive. Use `--plain` to print one line per action instead.

## Running it

You need Go 1.22 or newer. On macOS you also need the Xcode command line tools (`xcode-select --install`), because CoreMIDI needs cgo. Linux and Windows need nothing else.

```sh
go install github.com/Hive-RSI-Lab/hive-deck/cmd/deck@latest   # or: git clone … && go build ./cmd/deck
```

Then, with the FLX4 plugged in, and rekordbox or any other DJ software closed so it isn't holding the device:

```sh
deck list               # the MIDI devices this machine sees: the DDJ-FLX4 should be there
deck learn              # move every control: each line is the message and the control it reads as
deck --dry-run          # what each control does, then what each press would do; sends nothing to the Hive
deck                    # the real thing
```

On Linux the deck opens `/dev/snd/midiC*D*`, so your user needs to be in the `audio` group.

**A Hive on this machine:** the deck finds the workspace as the `hive` command does (`$HIVE_HOME`, the installer's pointer, or `~/hive`). It reads the daemon's port and your token from `.hive/private/daemon.json`, and follows a restart to its new token.

**A Hive elsewhere:** tunnel to its daemon and give the deck that Hive's `daemon.json`, or the token alone:

```sh
ssh -N -L 7777:127.0.0.1:<port> you@hive-host &          # <port> is in the remote daemon.json
scp you@hive-host:hive/.hive/private/daemon.json ~/.hive-remote.json
deck --url http://127.0.0.1:7777 --token-file ~/.hive-remote.json
```

The token is yours: anyone with it can do what you can in the control center. Keep the copy readable only by you (`chmod 600`).

## Changing the mapping

```sh
deck mapping > ~/deck.json      # the default, to edit
deck check --map ~/deck.json    # names, actions, views, signals and control types checked against the profile
deck --map ~/deck.json
deck layout --map ~/deck.json   # what each control does now, in words (--svg for the picture)
```

A binding is `{"do": "<action>", ...}`:

| Action | Takes |
|---|---|
| `approve`, `refuse` | `confirm` (only honored for questions) |
| `select` | `step` for a pad, `per` (ticks per step) for a jog |
| `scroll` | `per` for a jog |
| `view` | `view`: waiting, feed, threads or runs |
| `project` | |
| `hold`, `lift`, `rerun`, `retry` | `confirm` |
| `workers` | `min`, `max` |
| `route` | `role`, `models` |
| `cancel`, `refresh`, `nothing` | |

`lights` maps a lit control to a signal: `main.green`, `main.red`, `asks`, `gate`, `lead`, `connected`, `held`, `approve`, `refuse`, `armed`, or `view.<name>`.

## The surface, kept apart from the actions

There are two layers, so a future custom command-center product needs a profile, not new code:

- **The MIDI map** is a *profile*: `surface/profiles/ddj-flx4.json`. For each control it lists the messages it sends, how its value reads (button, absolute or relative), the messages that light it, and where it sits in the picture. Nothing else in the deck knows a MIDI byte.
- **The action layer** is in `deck/`. The mapping names controls (`pad.l.1`, `jog.r`, `fader.l`), never messages.

A different controller, or the custom hardware, needs only a new profile (`--profile my-surface.json`), and every action, light and test carries over. If `deck learn` shows a control sending something other than what the profile says, fix that line in the profile.

The FLX4 profile comes from Pioneer's MIDI message list, the layout the FLX4 shares with the DDJ-400. Hasn't been checked on a real deck yet: `deck learn` is the check.

## Tests

`go test ./...`. Nothing needs a device or a Hive:

- a fake MIDI device feeds raw FLX4 messages through the profile into the engine, against a fake Hive
- that covers the mapping and its checks, the confirm rule (second press, lapse, cancel, never one-touch for what's always yours), the lights (first flush, only changes after, blink phases), soft takeover and settling, and dry runs
- the HTTP client runs against a stand-in daemon: token, errors, the missing `/api/op`, config write-back, following a restart's new token
- a test fails if the layout picture has drifted from the mapping
