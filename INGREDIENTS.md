# Build ingredients

Everything baked into what fleet ships, and how a change to it reaches a release.

fleet is **its own upstream**: it is versioned `YYYYMMDD.N` and nothing external releases it. Its Go
source has no module dependencies beyond the standard library.

| Ingredient | Pinned in | Renovate | On a bump |
|---|---|---|---|
| golang-127 cross toolchain (builds the darwin binaries) | `components/golang/version` | ✅ `github-releases` on `Mavergreen/golang-127`, `-mavericks.N` versioning | releases: the pin is declared state |
| upstream Go (builds the Linux binary and the darwin/arm64 slice) | `components/go-upstream/version` | ✅ `golang-version`, capped to the 1.27 line | releases: the pin is declared state |
| MacOSX10.9 SDK, CMake modules, compat guard, packaging and signing scripts | `Mavergreen/shipyard@v1` | ✅ github-actions manager tracks the tag | `@v1` moves without the pin changing, so nothing repackages by itself |

No upstream release notes: fleet is its own upstream.

## Declared state

- upstream: UPSTREAM_VERSION
- golang: components/golang/version
- go-upstream: components/go-upstream/version

## Conformance deviations

- scheme: fleet is its own upstream (no upstream release to repackage), so it versions itself by date as YYYYMMDD.N with no -mavericks.N axis
- arm64:usr/local/mavergreen/fleet/bin/*: maintainer tool on Apple Silicon development hosts; slice floor is Go 1.27's 13.0 until the family's Go emits 11.0 (golang-126/127 QUEUE, 2026-11-19)
- macos-runner:release.yml:build: golang-127's cross toolchain runs only on arm64 macOS 13+; exit: a Linux-hosted golang-127
- macos-runner:release.yml:modern-smoke: check 37 requires it
