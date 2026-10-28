# Check reports from the real deck

Each `deck check` run on a real device writes `deck-check.md`. When the person pastes one into the deck's chat, commit it here unchanged, as `YYYY-MM-DD-<machine>.md`.

`go run ./cmd/score` (the project's score) reads every report here and prints the share of the profile's controls a real device was heard on: a control heard in any report counts. A report for another profile or from a fake device doesn't count, and nor does this file.
