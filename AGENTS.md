# AGENTS.md

zot is an autonomous coding agent in a single Go binary. The engine runs
in-process and talks straight to any OpenAI-compatible model provider - no hosted
service. `cmd/zot` is the CLI and the run orchestration; `internal/` holds the
engine (agent harness, loop, thread assembly, the model client, session logs) and
the `tui` viewer. Nothing is importable - zot is a binary, not a library.

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
- **It vets clean.** `make vet` runs `go vet`. Run `govulncheck ./...` when
  dependencies change.
- **Match the surrounding code.** Same naming, comment density, and idioms.
