# AGENTS.md

zot is an autonomous coding agent in a single Go binary. The engine runs
in-process and talks straight to any OpenAI-compatible model provider - no hosted
service. `cmd/zot` is the CLI; `internal/run` is one run of an order, from a
configuration to a finished log. Beneath it: `loop` is the engine, `provider` the
model connection, `conversation` the messages and their forgetting, `plan`,
`skills` and `tools` what the model can use, `session` the log, `order` the work
order, `tui` the viewer, and `config` the settings and the rules for them, which
imports nothing else of the module. Nothing is importable - zot is a binary, not
a library - and it supports Linux only: no other platform is built, tested or
worked around.

## Working here

- **This is 0.x: break things freely.** Breaking changes are expected and welcome
  before 1.0. Backward compatibility is _not_ a goal - keeping a deprecated field,
  an alias, a legacy code path, or an old on-disk format "just in case" is an
  anti-pattern here: it adds surface, hides the real design, and there are no
  external users to protect yet. When you rename or replace something, rename or
  replace it everywhere and delete the old thing. Say so in the commit (a `feat!:`
  title) rather than carrying it.
- **Test everything you change.** `make test` must pass, and total coverage should
  not fall below 90% (`make cover` reports it per package). Write tests that
  assert _behaviour_, not constants; a test that restates a value it reads is
  worse than none.
- **It vets clean, in modern Go.** `make vet` runs `go vet` and fails on anything
  `go fix` would rewrite (the modernizers: `any`, `range n`, `min`/`max`, `slices`,
  `maps`, `cmp.Or`, ...). Write the current idiom rather than the old one; the
  Modern Go Guidelines skill lists what the target version has. Run
  `govulncheck ./...` when dependencies change.
- **Match the surrounding code.** Same naming, comment density, and idioms.
- **Commit messages have one shape.** A conventional title (`feat!:`, `refactor:`,
  `style:`, `test:`, ...), a blank line, then one short paragraph saying what
  changed and why, wrapped at 72 columns. No bullet lists, no extra sections and
  no `Co-Authored-By` or other attribution trailers.
