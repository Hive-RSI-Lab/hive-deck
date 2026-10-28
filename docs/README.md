# deck: the DDJ-FLX4 drives the Hive

`deck` is a small program that runs on the laptop the controller is plugged into, next to the control center. It opens a Pioneer DDJ-FLX4 as a USB MIDI device (Linux, macOS, Windows) and talks to your Hive through the daemon's API with your own token. It drives the control center you have open: the jogs move its selection and scroll, pads switch its views, the browse knob's push opens what's selected and LOAD 1 goes back, and the view pads light for the view it shows. Pads answer the Waiting item selected there, hold or lift merges and retry work. A fader and two knobs set the worker limit and model routes. The other pad lights show the Hive's state.

It's an optional module and isn't installed by default.

![The default layout on the DDJ-FLX4](layout.svg)

*(Drawn from the default mapping by `deck layout --svg`. A test fails if the picture and the mapping drift apart.)*

## The default layout

| Control | Does | Light |
|---|---|---|
| Left pad 1 | **approve** the Waiting item selected in the control center: its first option (yes, merge, run, install…). Press twice. | on when there's something to approve; fast while waiting for the second press |
| Left pad 2 | **refuse** it: its second option (no, leave open, decline…). Press twice. | the same |
| Left pads 3 / 4 | previous / next Waiting item | blink while anything waits on you |
| Left pad 5 | **hold** the project's merges | on while held |
| Left pad 6 | **lift** the hold (press twice) | on while held |
| Left pad 7 | **re-run** the failed checks on the selected item's task, or the project's newest open PR (press twice) | |
| Left pad 8 | **retry** the selected item's task, or the project's newest blocked or stopped one (press twice) | |
| Right pads 1–4 | show the **Waiting / dashboard / threads / agents** view | on for the view the control center shows |
| Right pad 5 | jump to the feed | **main green**: on while the project's main passed its last measure |
| Right pad 6 | jump to the feed | **main red**: blinks while main failed its last measure |
| Right pad 7 | jump to runs | **gate**: on while a check runs; blinks while checks wait for a slot |
| Right pad 8 | jump to threads | **lead**: on while a lead is thinking |
| Left jog | move the selection, as ↑ and ↓ (in an open item, scroll it) | |
| Right jog | a page up or down | |
| Browse push | **open** the selection (Enter) | |
| LOAD 1 | **back** (Esc) | |
| LOAD 2 | read the Hive now | |
| IN / OUT, left | pick among the selected Waiting item's answers, as ← and → | |
| SYNC left | **answer** with the answer picked (press twice) | on when one is picked; fast while waiting for the second press |
| SYNC right | | **driving**: on while the deck drives a control center |
| Browse knob | pick the project for hold, lift, re-run, retry and the main lights | |
| CUE (either deck) | cancel a press that's waiting for its second press | blinks fast while one waits |
| PLAY left | | **connected**: on while the Hive answers |
| PLAY right | | **lead** thinking |
| Channel fader 1 | **worker limit**, 0 at the bottom to 8 at the top | |
| HI knob, channel 1 | the **worker** role's model route | |
| HI knob, channel 2 | the **lead** role's model route | |

Every pad does the same thing in any pad mode (HOT CUE, PAD FX, BEAT JUMP, SAMPLER…), because the profile lists each pad's note in every mode.

### The second press

Approve, refuse and answer arm on the first press. The pad blinks fast, and the deck's screen says what the second press will do. A second press of the **same pad** on the **same item** within 4 seconds (`confirm_seconds`) sends the answer. Any of these cancels the armed press:

- another button
- moving the selection, on the deck or in the control center
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
| Moves, views, open and back | `/api/ui`, which every open control center follows |
| Answers | `/api/answer`, the control center's Waiting call |
| Hold and lift | `/api/hold`. The daemon takes that only with your token, never a module's. |
| Retry | `/api/task/retry` |

The worker limit, routes and re-running checks should go **through the same ops the lead uses**, so they're journaled the same way. The deck calls them at `POST /api/op {"op": "workers" | "route" | "rerun_checks", "args": {...}}`. Today's daemon doesn't have that endpoint yet. A brief to add it is filed on the Hive. Until it lands:

- **Worker limit and routes** go through Settings' own route: `/api/config`, read, change, write back. Each change also puts a line on the journal (`module.reported`: "the person set max_workers from 3 to 5 from the deck").
- **Re-running checks** says it needs `/api/op`. Retry works today.

## Driving the control center

The control center (`hive`, or `hive remote <host>`) is the screen; the deck is its keys. Every move goes to the daemon as `POST /api/ui` with your token, and each open control center acts on it as the keys it stands for, so the deck does there what your keys would: `↓` at the end of a chat's threads lands in the reply box, Enter opens a task or a run, Esc goes back. The control center reports where it is within about 1.5 seconds, and the deck reads that (`GET /api/ui`) a few times a second to light its view pads and to know what is selected. The control center's hint line shows `⌁ deck pad.r.4: view agents` for each.

- **Answers act on what you see.** Approve, refuse and answer take the Waiting item the control center has selected, by its id, and send it through `/api/answer`. While the control center hasn't shown the deck's last move yet, they do nothing and say so, so a press right after a jog never answers the item you were on before. A confirm armed on an item is dropped when the control center moves off it, by the deck or by your keys.
- **Re-run and retry** take the task selected in the control center (in the agents view or the dashboard), or the Waiting item's task, else the project's newest that fits.
- **A view pad may name any of the control center's views**: `chats`, `threads`, `thread`, `dashboard`, `agents`, `waiting`, `feedback`, `knowledge`, `modules`, `settings`. `feed` and `runs` are the dashboard and agents. Its light is `view.<name>`.
- **No control center open?** The deck moves its own view, as before. The daemon keeps the last view a control center reported, so the deck only knows one is gone when a move isn't shown within 5 seconds; it then says *no control center is following* and moves its own view until one moves again. A Hive from before `/api/ui` says so, and its `hive update` fixes it.

Steering is the person's: the daemon takes `/api/ui` only with your token, never a module's.

## The screen

`deck` draws its own view in the terminal. The header shows the project, main, Waiting, checks and the worker limit. Below that are tabs for waiting, feed, threads and runs, a line saying whether the deck is driving a control center and what's selected there, the selection, and what the deck last did. While a control center is open this view follows it; with none, the jogs move this one. Use `--plain` to print one line per action instead.

## Running it

**Windows and Linux:** download the binary from the [latest release](https://github.com/Hive-RSI-Lab/hive-deck/releases/latest). There's nothing to install. On Windows it's `deck.exe`. Put it in a folder and run it from a terminal there (`.\deck.exe check`). On Linux it's `deck-linux-amd64`. Rename it to `deck` and `chmod +x` it.

**Or build it yourself**, which is the only way on macOS. You need Go 1.22 or newer. On macOS you also need the Xcode command line tools (`xcode-select --install`), because CoreMIDI needs cgo. Linux and Windows need nothing else.

```sh
go install github.com/Hive-RSI-Lab/hive-deck/cmd/deck@latest   # or: git clone … && go build ./cmd/deck
```

Then, with the FLX4 plugged in, and rekordbox or any other DJ software closed so it isn't holding the device:

```sh
deck check              # the first step on a new laptop: see below
deck list               # the MIDI devices this machine sees, if check can't find the deck
deck learn              # move every control: each line is the message and the control it reads as
deck --dry-run          # what each control does, then what each press would do; sends nothing to the Hive
deck                    # the real thing
```

### `deck check`: the first step

`deck check` needs no Hive. It opens the deck and does two things:

1. **The lights.** It lights each light in the profile on its own, in turn (pads, PLAY, CUE…). Watch the deck. Then press Enter if every one lit, or type the ones that didn't (`pad.l.3 play.r`, or just `no`).
2. **The controls.** Every light that hasn't been heard yet comes back on. Press every pad and button, turn both jogs and every knob, and move every fader. Each lit control goes dark once the deck has heard it, and the screen says how many are left. It ends when every control is heard, when you press Enter, or after 5 minutes (`--wait`).

Then it prints a table with every control in the profile: whether the mapping uses it, whether it was heard, and the message it was heard by (or the one the profile expected). A second table lists the messages the deck sent that the profile doesn't know, with the control heard just before each one. A wrong note number usually shows up there, right after the control it belongs to. SHIFT with a pad sends notes like these too, and they don't fail the check.

The report is also written to `deck-check.md` (`--out`). Paste it into the deck's chat. `deck check` exits non-zero if any control in the profile was never heard, or if you said a light didn't light.

On Linux the deck opens `/dev/snd/midiC*D*`, so your user needs to be in the `audio` group.

**A Hive on this machine:** the deck finds the workspace as the `hive` command does (`$HIVE_HOME`, the installer's pointer, or `~/hive`). It reads the daemon's port and your token from `.hive/private/daemon.json`, and follows a restart to its new token.

**A Hive elsewhere:** name its machine, as you would to `ssh` (a host, `you@host`, or a `Host` from `~/.ssh/config`):

```sh
deck --ssh you@hive-host
```

One ssh login does it all, as the Hive's own `hive remote` does: ssh runs `hive remote-info --follow` there, which prints that Hive's `daemon.json` and again whenever a restart changes its port or token, and opens a SOCKS proxy on your machine's loopback that the deck reaches the daemon through. So when the Hive restarts, the deck picks up the new port and token on its own; while it's down the screen says *can't reach the Hive* and the deck carries on when it's back; if ssh itself drops (a sleep, a network change), the deck dials it again, waiting a little longer after each failed try, up to 30 seconds.

- It runs the `ssh` on your PATH: OpenSSH on Linux and macOS, and on Windows the built-in `ssh.exe` (Settings → Optional features → OpenSSH Client).
- The first login can ask for a password or passphrase as usual. Redials run with `BatchMode`, so they can't ask on the deck's screen: use a key with `ssh-agent` (on Windows, the *OpenSSH Authentication Agent* service) for the deck to reconnect on its own.
- If `hive` isn't on that machine's PATH or in `~/.local/bin`, name it: `--hive /path/to/hive`. A Hive too old for `hive remote-info` says so; run its `hive update`.

Or tunnel by hand and give the deck an address and that Hive's `daemon.json` (or the token alone); this one doesn't follow a restart:

```sh
ssh -N -L 7777:127.0.0.1:<port> you@hive-host &          # <port> is in the remote daemon.json
scp you@hive-host:hive/.hive/private/daemon.json ~/.hive-remote.json
deck --url http://127.0.0.1:7777 --token-file ~/.hive-remote.json
```

The token is yours: anyone with it can do what you can in the control center. Keep the copy readable only by you (`chmod 600`).

## Changing the mapping

```sh
deck mapping > ~/deck.json      # the default, to edit
deck check --map ~/deck.json    # names, actions, views, signals and control types checked against the profile (no device)
deck --map ~/deck.json
deck layout --map ~/deck.json   # what each control does now, in words (--svg for the picture)
```

A binding is `{"do": "<action>", ...}`:

| Action | Takes |
|---|---|
| `approve`, `refuse`, `answer` | `confirm` (only honored for questions) |
| `select`, `page` | `step` for a pad, `per` (ticks per step) for a jog |
| `scroll` | `per` for a jog (in the control center, a page) |
| `pick` | `step` (-1 left, 1 right) for a pad, `per` for a jog |
| `open`, `back` | |
| `view` | `view`: waiting, feed, threads, runs, or any of the control center's views |
| `project` | |
| `hold`, `lift`, `rerun`, `retry` | `confirm` |
| `workers` | `min`, `max` |
| `route` | `role`, `models` |
| `cancel`, `refresh`, `nothing` | |

`lights` maps a lit control to a signal: `main.green`, `main.red`, `asks`, `gate`, `lead`, `connected`, `held`, `approve`, `refuse`, `answer`, `armed`, `steering`, or `view.<name>`.

## The surface, kept apart from the actions

There are two layers, so a future custom command-center product needs a profile, not new code:

- **The MIDI map** is a *profile*: `surface/profiles/ddj-flx4.json`. For each control it lists the messages it sends, how its value reads (button, absolute or relative), the messages that light it, and where it sits in the picture. Nothing else in the deck knows a MIDI byte.
- **The action layer** is in `deck/`. The mapping names controls (`pad.l.1`, `jog.r`, `fader.l`), never messages.

A different controller, or the custom hardware, needs only a new profile (`--profile my-surface.json`), and every action, light and test carries over. If `deck learn` shows a control sending something other than what the profile says, fix that line in the profile.

The FLX4 profile comes from Pioneer's MIDI message list, the layout the FLX4 shares with the DDJ-400. Hasn't been checked on a real deck yet: `deck check` is the check, and `deck learn` shows any message in detail.

## Tests

`go test ./...`. Nothing needs a device or a Hive:

- a fake MIDI device feeds raw FLX4 messages through the profile into the engine, against a fake Hive
- that covers the mapping and its checks, the confirm rule (second press, lapse, cancel, never one-touch for what's always yours), the lights (first flush, only changes after, blink phases), soft takeover and settling, and dry runs
- the HTTP client runs against a stand-in daemon: token, errors, the missing `/api/op` and `/api/ui`, config write-back, following a restart's new token
- driving the control center: what each control sends, the lights following its reported view (and lit at once on a view pad), answers taking its selection and waiting until it has shown the last move, a confirm dropped when it moves off the item, a control center that stops following let go, and the deck's own view when none is open
- a test fails if the layout picture has drifted from the mapping
- `deck check` runs against the fake device: every control heard, one missing, a message the profile doesn't know, lights named as dark, and the deck unplugged
