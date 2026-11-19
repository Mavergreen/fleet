# fleet

**This README has not been read or edited by a human yet.** Until it has, this project cannot cut
its first release.

`mavergreen-fleet` watches over the Mavergreen GitHub org: every non-archived repo, listed at run time.
It reads with your `gh` login (`GH_TOKEN` if set, else `gh auth token`) and never stores or prints the token.

It is an unofficial community tool for the Mavergreen family. It runs on Mac OS X 10.9, modern macOS and
x86_64 Linux, as you, and never needs root.

## Commands

- `mavergreen-fleet` (or `mavergreen-fleet attention`): everything family-wide that needs a human, such as
  stuck pull requests and repos it could not read. An empty list says what it checked.
- `mavergreen-fleet remediate [--dry-run]`: for a Renovate PR whose failure matches a known transient
  pattern, ticks its rebase/retry checkbox, at most twice per update. Other failures are reported as
  needing attention and not re-run, until re-runs are verified. `--dry-run` says what it would do and
  changes nothing. It never merges, cancels or deletes anything.
- `mavergreen-fleet timing [repo [run-id]]`: where Actions time goes. With no repo, a table per repo and
  runner kind; with a repo, that repo's latest `release.yml` run (or the run named) as Gantt bars of
  queue wait and run time. `--since` sets the window (default 7 days). Reading jobs costs a request per
  run not yet cached, from the GitHub quota every agent shares, so a fleet-wide run that would leave
  fewer than 500 requests this hour stops and says so; `--force` runs it anyway.
- `mavergreen-fleet graph`: the family's ingredient graph: its edges, diamonds and cycles, plus what
  it could not read. How long changes take to flow along it is to come. `--dot` and `--mermaid` print
  it in those formats, and `--from-dir DIR` reads a directory of family checkouts instead of the org.

Every command accepts `--json`. Text output uses colour only on a terminal, and is cut to the
terminal's width (or `COLUMNS`) only there: piped, nothing is truncated.

## Installing

On a Mac, install the `.pkg`: it puts `mavergreen-fleet` in `/usr/local/mavergreen/fleet/bin` and links
it into `/usr/local/mavergreen/bin`. On x86_64 Linux, the tarball holds one file,
`opt/mavergreen/fleet/bin/mavergreen-fleet`, so `sudo tar -xzf fleet-VERSION-linux-x86_64.tar.gz -C /`
installs it to `/opt/mavergreen/fleet/bin`.

## JSON output and exit codes

With `--json`, every command prints one JSON object on stdout, never truncated or coloured. It
starts with the same three fields:

| Field | Value |
|---|---|
| `schema` | `"fleet/v1"` |
| `command` | `"attention"`, `"remediate"`, `"timing"` or `"graph"` |
| `generated` | when it ran, RFC 3339 in UTC (`"2026-11-19T12:00:00Z"`) |

The command's own fields follow. Lists are `[]` when empty, never `null`.

**`attention`**

- `checked`: `{"repos", "prs", "issues", "runs"}`, counts of what was read, so an empty list is not silent.
- `findings`: a list of `{"repo", "item", "url", "kind", "since", "why", "fix"}`, most urgent first.
  `kind` is `cannot-read`, `stuck-pr`, `red-workflow`, `needs-reply` or `retry-in-progress`; `since` is
  RFC 3339; `item` is `repo#123` for a PR or issue and `repo:workflow` for a workflow.

Something fleet could not read is a finding of kind `cannot-read`: `item` names what (an API path, or
"checks for repo#123"), `why` carries the error.

**`remediate`**

- `actions`: a list of `{"kind", "repo", "pr", "run_id", "run_url", "pattern", "attempt", "note", "done",
  "dry_run", "error"}`. `kind` is `tick-box`, `rerun` or `report` (an action declined at the last moment
  comes back as a `report`). `pr`, `run_id`, `run_url`, `pattern`, `attempt` and `error` are left out
  when they do not apply. `done` is true only when the write happened.

Something remediate could not read is a `report` whose `error` carries the read error.

**`timing`**, with no repo:

- `window_start`: the first day counted, `YYYY-MM-DD`.
- `repos`: one row per repo and runner kind, most total hours first: `{"repo", "kind"` (`macos` or
  `linux`), `"jobs", "wait_h", "median_wait_m", "p90_wait_m", "run_h", "never_started",
  "never_started_wait_h", "reruns", "manual_kicks", "wasted_h", "total_h"}`. Jobs not yet finished
  are not counted.
- `unreadable_runs`: `{"<repo>": <runs whose jobs could not be read>}`; those runs are left out of `repos`.
- `cannot_read`: one line per thing that could not be read (a repo's run list, a run's jobs).

**`timing <repo> [run-id]`**:

- `repo`, `run_id`, and `note` when there is something to say (no `release.yml` run, so another
  workflow's; or no runs at all, when `run_id` is absent).
- `jobs`: `{"name", "attempt", "labels", "created", "started", "completed", "wait_s", "run_s",
  "never_started", "conclusion"}`, every attempt's jobs. `[]` when there are no runs.
- `critical_path`: job names, earliest first.

A repo or run that cannot be read is an error (exit 1), not a field.

**`graph`**

- `edges`: `{"consumer", "ingredient"}`; `diamonds` and `cycles`: the script's `DIAMOND` and `CYCLE` lines.
- `incomplete`: the repos it skipped and names it could not map (`SKIP …`, `UNMAPPED …`).
- `timings`: says propagation timings are not yet available.

**Exit codes**

| Code | When |
|---|---|
| 0 | the command printed its report, text or JSON. **Something unread does not change this**: a script must look at `cannot-read` findings, remediate's `error`, timing's `cannot_read` and `unreadable_runs`, or graph's `incomplete`. |
| 1 | an error: no token; the org's repos or the token's user could not be read; output could not be written. `remediate`: a write failed, or a read it needed just before writing failed (a cannot-read found while planning is a report, exit 0). `timing`: the repo or run asked for cannot be read; the quota check stopped it; or, as text only, it read no runs and something could not be read. `graph`: `family-graph.sh` failed (anything but its exit 0, or 3 for a partial graph), or the shipyard scripts are missing. |
| 2 | a usage error: an unknown command or argument, a bad `--since` or run id. |

## Building

```sh
sh build/build-fleet.sh /usr/local/mavergreen/bin/go darwin amd64 dist/x86_64 dev
dist/x86_64/mavergreen-fleet --version
```
