# Releasing

The root module and the nested modules (`sqlite`, `postgres`, `kurrentdb`,
`telemetry`) are released together: `v0.5.0` with `sqlite/v0.5.0` and so
on, each nested module requiring the matching root version.

Between releases a nested module may use root API that is not released
yet. It builds inside the workspace (`go.work`) but not alone, so a
release takes two PRs: one for the root, then one that points the nested
modules at it. `scripts/release.sh` runs each step. It needs `git`, `go`
and `gh`, and a clean working tree.

1. **Release PR.** Write the changelog's `[Unreleased]` section as it should
   read, then run

   ```bash
   scripts/release.sh prepare v0.5.0
   ```

   It opens a PR that sets `InstrumentationVersion` in `version.go` and
   adds the dated `## [0.5.0]` heading under `[Unreleased]`. Merge it.

2. **Root tag.**

   ```bash
   scripts/release.sh tag-root v0.5.0
   ```

   Tags the merged release PR as `v0.5.0` and pushes the tag. It refuses
   unless `version.go` on `origin/master` says `v0.5.0`.

3. **Bump PR.**

   ```bash
   scripts/release.sh bump v0.5.0
   ```

   Opens a PR in which every nested module, `examples/todolist` included,
   requires `v0.5.0`, after checking that each builds, vets and tests with
   `GOWORK=off`. Merge it.

4. **Module tags.**

   ```bash
   scripts/release.sh tag-modules v0.5.0
   ```

   Checks that every nested module on `origin/master` requires `v0.5.0`
   and builds alone, then tags `sqlite/v0.5.0`, `postgres/v0.5.0`,
   `kurrentdb/v0.5.0` and `telemetry/v0.5.0` there and pushes the tags.
   The Release Check workflow runs the same check on those tags.

`scripts/release.sh check` runs the standalone check on the working tree at
any time; the Release Check workflow can also be started by hand. Between
releases it is expected to fail for any module that uses unreleased API.
