<p align="center">
  <img width="96" height="96" alt="zot" src="https://github.com/user-attachments/assets/2577f5e5-ce2e-4c96-b7b5-9c48ac05e4ca" />
</p>

<h1 align="center">Zot</h1>

<p align="center">
  <strong>Stop prompting. Start shipping.</strong>
</p>

<p align="center">
  <img width="3260" height="2160" alt="zot running a work order" src="https://github.com/user-attachments/assets/a3b74447-85a8-4a0e-be3d-6c88aeed8649" />
</p>

zot is an automated software factory in a single static binary. It takes a
work order - an objective, the acceptance criteria that define done, the
constraints to hold - and plans, edits, builds, tests and fixes until the work
is done. No chat loop, no approving each edit, no hosted engine, no telemetry.
It talks straight to any OpenAI-compatible model provider with your own key.

## Install

zot is built from source. With Go installed:

```bash
git clone https://github.com/openzot/openzot
cd openzot
make build
```

It needs Go 1.27 or newer. `make` on its own lists the other targets: `make test`,
`make race`, `make vet` and `make cross`.

## Use

zot ships no providers and no default model: declare a provider and a model in
the config first (`zot config` opens it from a template), then write an order
and hand it over:

```yaml
# ~/.config/zot/config.yaml
default_provider: mygateway
agent:
  model: my-model
providers:
  mygateway:
    base_url: https://gateway.example.com/v1
    api_key: '$GATEWAY_KEY'
    models:
      my-model:
        context: 128000   # the model's context window, in tokens - required
```

```bash
export GATEWAY_KEY="..."
zot new    # opens a new order in your editor
zot .zot/orders/<name>.md
```

`zot new` creates an order under `.zot/orders/` and opens it in your editor. An
order is one file: a front matter block that says what to do, then the whole
system prompt as a Go template that reads it.

```markdown
---
objective: |
  Add rate limiting to the API.
acceptance:
  - requests beyond the limit receive a 429
  - the test suite passes
constraints:
  - do not change handler signatures
---
You are zot, a fully autonomous software engineering agent ...
{{ range .Tools }}- "{{ .Name }}": {{ .Description }}
{{ end }}
## Your task

{{ .Objective }}
```

Write the objective and leave the prompt alone, or rewrite the prompt to change
how the agent works: the template can use the order's fields, the tool list, the
working directory, the date, the model, the project's `AGENTS.md` (`{{ .Project }}`)
and the functions `file "path"`, `env "NAME"` and `inc N`. Whatever the prompt
says, zot adds its non-interactive contract back if the rendered text lacks it: a
run has no way to ask anyone anything. `zot .zot/orders/<name>.md` runs an order
from zero - one order per invocation. Any OpenAI-compatible endpoint works - a hosted service, a
gateway, a local server - declare it under `providers:` and name it with
`default_provider`.

Skills - folders of `SKILL.md` instructions - live in the directory named by
`skills_dir` in the config. They are read into memory at startup and offered to
the model through a `skills` tool: it lists them with short descriptions and reads
one in full when it decides it applies.

Every run is logged beside its order as `.zot/orders/<name>.jsonl`: one JSON
line per step, including the model's reasoning, appended as the run goes. Running
an order again adds to the same file. Read it with `jq`.

## Why Zot

- **Orders, not prompts.** Files you can edit, commit and re-run.
- **Nothing in the way.** Your key, your provider, your machine.
- **Built to run unattended.** Context trimming, loop detection, a full log of every run.
- **Orchestration is content.** No sub-agent framework; skills decide.

Watch it work: [the factories](#factories) run in public, each shipping from
one standing order with no human in the loop.

## ⚠️ Safety

zot has real file-write and shell access from `--dir`, and `--dir` is not a
sandbox. Point it at a disposable checkout.

## Reference

- `zot --help` lists the commands and flags.
- [configs/zot.example.yaml](configs/zot.example.yaml) documents every config key.
- [AGENTS.md](AGENTS.md) is the guide for working on zot itself.

## Factories

Live zot factories: the same standing order every 30 minutes, the catalogue as
the only memory, nobody in the loop. The site is the working tree, and every
session ships to a public dataset.

| Factory | Makes | Repo | Sessions |
| --- | --- | --- | --- |
| [Arcade](https://openzot.github.io/arcade/) | One brand-new browser game per shift, playtested and published | [openzot/arcade](https://github.com/openzot/arcade) | [openzot/arcade](https://huggingface.co/datasets/openzot/arcade) |
| [Machinery](https://openzot.github.io/machinery/) | One working control panel per shift - a live simulation, faults, and its operating manual | [openzot/machinery](https://github.com/openzot/machinery) | [openzot/machinery](https://huggingface.co/datasets/openzot/machinery) |
| [Whetstone](https://openzot.github.io/whetstone/) | One game honed toward perfection - each shift improves one facet and ships the next playable version | [openzot/whetstone](https://github.com/openzot/whetstone) | [openzot/whetstone](https://huggingface.co/datasets/openzot/whetstone) |

## Ecosystem

| Project                                       | Role                                                                      |
| --------------------------------------------- | ------------------------------------------------------------------------- |
| [Rook](https://github.com/pdparchitect/rook)  | A fully automated offensive security harness                              |
| [Pion](https://github.com/pdparchitect/pion)  | A defensive AI security harness for automatic monitoring and incident prevention |
| [Pantalk](https://github.com/pantalk/pantalk) | Connect coding agents to the chat platforms people already use            |
| [MCPShim](https://github.com/mcpshim/mcpshim) | Turn MCP servers and HTTP APIs into standard CLI commands                 |
| [crmkit](https://github.com/crmkit/crmkit)    | Give agents a shared CRM and system of record over HTTP or MCP            |

## Status

zot is **0.x** and in active use. Flags, config and behavior may change before
1.0 - pin a commit before upgrading.
Small, focused pull requests are welcome; anything large is worth an issue first.
