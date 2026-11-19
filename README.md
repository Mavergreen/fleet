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
- `mavergreen-fleet timing [repo|run]`: where Actions time goes, as terminal Gantt bars of queue wait and
  run time. `--since` sets the window (default 7 days).
- `mavergreen-fleet graph`: the family's ingredient graph, annotated with how long changes take to flow.
  `--dot` and `--mermaid` print it in those formats, and `--from-dir DIR` reads a directory of family checkouts instead of the org.

Every command accepts `--json`. Text output uses colour only on a terminal.

## Building

```sh
sh build/build-fleet.sh /usr/local/mavergreen/bin/go darwin amd64 dist/x86_64 dev
dist/x86_64/mavergreen-fleet --version
```
