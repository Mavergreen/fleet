# Build ingredients

Everything baked into what fleet ships, and how a change to it reaches a release.

fleet is **its own upstream**: it is versioned `YYYYMMDD.N` and nothing external releases it. Its Go
source has no module dependencies beyond the standard library.

| Ingredient | Pinned in | Renovate | On a bump |
|---|---|---|---|
| golang-127 cross toolchain (builds the darwin binaries) | `components/golang/version` | ✅ `github-releases` on `Mavergreen/golang-127`, `-mavericks.N` versioning | releases: the pin is declared state |
| upstream Go (builds the Linux binary and the darwin/arm64 slice) | `components/go-upstream/version` | ✅ `golang-version`, capped to the 1.27 line | releases: the pin is declared state |
| clang22's `llvm-lipo`, `llvm-otool`, `llvm-nm`, `llvm-strings` (merge the darwin slices; read Mach-O for the compat guard and conformance). CI only: not in the binary, so not declared state | `components/clang22/version`, fetched by shipyard's `fetch_clang22.sh` | ✅ `github-releases` on `Mavergreen/clang-22`, `-mavericks.N` versioning | nothing releases: the tools change no shipped byte |
| MacOSX10.9 SDK, CMake modules, compat guard, packaging and signing scripts | `Mavergreen/shipyard@v1` | ✅ github-actions manager tracks the tag | `@v1` moves without the pin changing, so nothing repackages by itself |

No upstream release notes: fleet is its own upstream.

## Release shape

`release.yml` publishes when `release-needed.sh` finds no release carrying the declared state below, so a bump
of either pin, which is declared state, releases by itself on its push to main. fleet therefore has no
`repackage-on-ingredient-bump.yml` caller: it would dispatch a second release for the same push. fleet ships
no Sparkle updater and no feed, and signs nothing, so there is no `scan-for-key.yml` job either.

## Declared state

- upstream: UPSTREAM_VERSION
- golang: components/golang/version
- go-upstream: components/go-upstream/version

## Conformance deviations

- scheme: fleet is its own upstream (no upstream release to repackage), so it versions itself by date as YYYYMMDD.N with no -mavericks.N axis
- arm64:usr/local/mavergreen/fleet/bin/*: maintainer tool on Apple Silicon development hosts; slice floor is Go 1.27's 13.0 until the family's Go emits 11.0 (golang-126/127 QUEUE, 2026-11-19)
- sdk-pin:usr/local/mavergreen/fleet/bin/*: Go's linker stamps the arm64 slice with its own minos 13.0 and a build SDK, not the pinned 11.3 SDK at 11.0; this also excuses the x86_64 slice at conformance, which the compat guard run on the thin x86_64 binary in release.yml still holds to the pin; exit: the family's Go emitting darwin/arm64 at an 11.0 floor (golang-126/127 QUEUE, 2026-11-19)
- macos-runner:release.yml:modern-smoke: check 37 requires it
