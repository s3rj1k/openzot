# AGENTS.md

Agent is an autonomous coding agent in a single Go binary. The engine runs
in-process and talks straight to any OpenAI-compatible model provider - no hosted
service. `cmd/cli` is the whole integration: it reads the flags and the config,
resolves them into a provider client and engine options, renders the prompt, opens
the session log, runs the viewer and prints the digest. Everything beneath it is a
feature package that does one job and imports as little as it can. `loop` is the
engine. Around it `outcome` is how a run is bounded and ended, `window` what fits
in the context window, `request` what a model call carries, and `cycle` and
`runaway` its repetition guards. `provider` is the model connection and `failure`
what a provider error means and how long to wait on it. `conversation` is the
messages, `plan`, `skills` and `tools` what the model can use, `session` the log
format, `order` the work order, `render` the terminal text, `tui` the viewer, and
`config` the settings and the rules for them. Only `cmd/cli` may import many of
them. The feature packages depend on each other only where they must (`loop` on
`conversation`, `failure`, `outcome`, `window`, `request`, `cycle` and `runaway`,
`cycle` on `runaway`, `window` and `request` on `conversation`, `provider` on
`failure`, `tui` on `loop` and `render`, `tools` on `plan` and `skills`), and
`config`, `order`, `plan`, `skills`, `conversation`, `failure`, `outcome` and
`runaway` import nothing else of the module. Nothing is importable - agent is a
binary, not a library - and it supports Linux only: no other platform is built,
tested or worked around.

## Working here

- **This is 0.x: break things freely.** Breaking changes are expected and welcome
  before 1.0. Backward compatibility is _not_ a goal - keeping a deprecated field,
  an alias, a legacy code path, or an old on-disk format "just in case" is an
  anti-pattern here: it adds surface, hides the real design, and there are no
  external users to protect yet. When you rename or replace something, rename or
  replace it everywhere and delete the old thing. Say so in the commit (a `feat!:`
  title) rather than carrying it.
- **Test everything you change.** `task test` must pass, and total coverage should
  not fall below 90% (`task cover` reports it per package and in total, leaving
  out `internal/testutils`). Write tests that
  assert _behaviour_, not constants; a test that restates a value it reads is
  worse than none. Tests live in the external `x_test` package and reach only
  what `x` exports. Export what a test needs instead of bridging it with an
  `export_test.go` or an alias. Only `cmd/cli` is tested in `package main`, since
  a `main` package cannot be imported. Helpers more than one package needs live
  in `internal/testutils`, not copied into each test package.
- **It vets clean, in modern Go.** `task vet` runs `go vet` and fails on anything
  `go fix` would rewrite (the modernizers: `any`, `range n`, `min`/`max`, `slices`,
  `maps`, `cmp.Or`, ...). Write the current idiom rather than the old one; the
  Modern Go Guidelines skill lists what the target version has. Run
  `govulncheck ./...` when dependencies change.
- **Match the surrounding code.** Same naming, comment density, and idioms.
- **Commit messages have one shape.** A conventional title (`feat!:`, `refactor:`,
  `style:`, `test:`, ...), a blank line, then one short paragraph saying what
  changed and why, wrapped at 72 columns. No bullet lists, no extra sections and
  no `Co-Authored-By` or other attribution trailers.
