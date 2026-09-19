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

`make` on its own prints the targets. `make test`, `make race`, `make cover`,
`make vet` and `make cross GOOS=… GOARCH=…` are also available.

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
| `tui/`                 | public Bubble Tea read-only viewer (themeable; embeddable over any `agent` run) |
| `configs/`             | example configuration                                                          |

Contributor conventions live in [AGENTS.md](../AGENTS.md) and
[.agents/skills/](../.agents/skills/). Changes are noted in
[CHANGELOG.md](../CHANGELOG.md).
