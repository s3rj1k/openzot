# Building & developing zot

Building from source, the two build variants and why they differ, and a map of
the codebase. For running zot, see the [README](../README.md).

## Build from source

Requires Go 1.27+.

```bash
git clone https://github.com/openzot/openzot
cd openzot
make                     # lists the targets
make build               # or: go build -o zot ./cmd/zot
```

`make` on its own prints the targets rather than assuming one, because `build`
and `dev` produce binaries that differ in what they will read from disk.
`make test`, `make race`, `make cover`, `make vet` and
`make cross GOOS=… GOARCH=…` are also available.

## Release vs developer builds

`make build` produces a release binary. `make dev` produces a developer one, and
the difference is a security boundary rather than a convenience:

|                           | release (`make build`) | developer (`make dev`) |
| ------------------------- | ---------------------- | ---------------------- |
| reads `.env` from `--dir` | no                     | yes                    |

zot runs unattended with a provider key and a shell tool, so a released binary
must not take credentials from whatever directory it was pointed at - running it
against a repository you cloned to review would otherwise be enough to load a
stray committed `.env` into the process that is about to run commands. Released
builds read credentials from the config file and the real environment, both of
which you chose deliberately.

The switch is a build tag (`-tags dev`) and defaults to off, so a build that
forgets it loses a convenience rather than a boundary.

## Architecture

| Path                   | Responsibility                                                                 |
| ---------------------- | ------------------------------------------------------------------------------ |
| `cmd/zot/`             | the binary: flag parsing, sessions, working dir, then `zot.Run`                |
| `zot.go`               | embeddable core: builds the model client + agent options and runs it           |
| `agent/`               | the public harness: `ExecuteWithTools`, tools, skills, events                  |
| `internal/loop/`       | the agentic loop: budgets, guards, settle mode, message hygiene                |
| `internal/thread/`     | context-window assembly and the four loop-detection heuristics                 |
| `internal/llm/`        | the model connection: fantasy over chat-completions, plus error classification |
| `internal/catalogue/`  | what each model's context window and capabilities are                          |
| `internal/tokenizer/`  | BPE token counting with embedded vocabularies                                  |
| `internal/session/`    | JSONL run logs: write, read, list, resume                                      |
| `internal/config/`     | layered config (defaults < file < env), XDG paths, env overrides               |
| `internal/buildinfo/`  | release vs developer build, and what that changes                              |
| `tui/`                 | public Bubble Tea read-only viewer (themeable; embeddable over any `agent` run) |
| `configs/`             | example configuration                                                          |

Contributor conventions live in [AGENTS.md](../AGENTS.md) and
[.agents/skills/](../.agents/skills/). Changes are noted in
[CHANGELOG.md](../CHANGELOG.md).
