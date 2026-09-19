# Configuration, flags, sessions & project context

How to configure a run, the flags and keys, where the session logs live, and how
zot picks up `AGENTS.md` and skills. For choosing and configuring a provider, see
[providers.md](providers.md); for work orders, [orders.md](orders.md); for the
agent itself, [how-it-works.md](how-it-works.md).

## The config file

Configuration is layered: built-in defaults < config file < CLI flags.

```bash
zot config        # opens the config in $EDITOR, creating it from a template
zot config path   # print the config file location
```

Providers are not built in: each is declared under `providers:` with a `base_url` and an
`api_key`, and a run needs `default_provider` and `agent.model` set. A key is
written in the config or references a variable you export (`api_key:
'$GATEWAY_KEY'`); no conventional variable is read on a provider's behalf. Every field is documented in
[configs/zot.example.yaml](../configs/zot.example.yaml).

## Flags

`zot --help` lists them all. The ones worth knowing:

| Flag                                          | Effect                                                             |
| --------------------------------------------- | ------------------------------------------------------------------ |
| `--provider` / `--model`                       | which provider and model to run against                           |
| `--dir`                                       | the directory the agent reads, writes and runs commands in; also accepted by `zot new`, which creates the order under `<dir>/.zot/orders` |
| `--max-iterations`                            | cap the agentic rounds; the default is deliberately large          |
| `--session-dir` / `--no-session`              | see [Sessions](#sessions)                                          |
| `--watch`                                     | keep running and watch a folder or glob for new orders - see [Watch mode](#watch-mode) |
| `--orders-dir`                                | where this project's orders live: what a bare `zot` runs, what a bare `--watch` watches, and where `zot new` files one. Defaults to `<dir>/.zot/orders` |
| `--diff`                                      | show a syntax-highlighted diff under each write                    |
| `--plain`                                     | stream unstyled output; auto-enabled when stdout is not a terminal |
| `--config`                                    | use a specific config file                                         |

## Controls

Because the agent is autonomous, the only keys are for viewing:

| Key           | Action             |
| ------------- | ------------------ |
| `↑` / `↓`     | scroll the log     |
| `PgUp`/`PgDn` | page the log       |
| `g` / `G`     | jump to top/bottom |
| `q`           | quit               |

## The header stats

The bar under the title shows how the run is going. Each field is a segment,
shown only when it fits whole - a narrow terminal drops the tail rather than
clipping a number mid-digit, so **order matters**: what you list first is what
survives.

| Stat                  | Shows                                                          |
| --------------------- | -------------------------------------------------------------- |
| `provider` / `model`  | what the run is talking to                                     |
| `task`                | plan steps done, from the agent's own `plan`/`progress` calls   |
| `order`               | position in a batch - `2/5` when running several orders         |
| `iter`                | agentic rounds, against the cap when one is set                 |
| `edits`               | files written or edited                                        |
| `elapsed`             | wall time, against `max_time` when set                          |
| `tps`                 | output tokens per second - is the provider keeping up           |
| `pace`                | average wall time per iteration - what predicts the finish      |
| `tokens`              | provider-reported usage, in and out                             |
| `dir`                 | the working directory, shortened from the left (`…/repos/zot/tool`) |
| `tools`               | cumulative tool calls (off by default)                          |

Set your own list and order with `ui.stats`:

```yaml
ui:
  stats: [task, order, elapsed, tps, model]
```

The defaults are everything above except `tools`, which is a cumulative count
that climbs on every run and says nothing about whether this one is going well.
`dir` is last because it is the longest field and never changes - a static path
is not worth the live stats it would push off the end. A stat with nothing to
report yet (`tps` before the first tokens, `task` before the agent has planned,
`order` outside a batch) shows `-` rather than a confident zero.

## The book

A bare `zot`, run in a project, runs that project's orders:

```bash
zot new    # write an order in your editor
zot
```

That is the whole loop. Everything below is what it means.

A project's work orders live under one directory at its root:

```
<project>/.zot/
    orders/<timestamp>.yaml       written by zot new
```

One dotted directory, the way every other tool that keeps state in a repository
does it. A top-level `orders/` folder would claim a generic name in the root of
somebody else's project, which is not zot's to take.

Orders are read from anywhere. An order is advisory input - what to do - and may
live wherever it is useful: in the repository being worked on, in a shared
folder of briefs, in a file some other process wrote. `zot <any
path>/order.yaml` runs exactly that, no book required. `.zot/orders/` is where
zot looks when you name nothing, and where `zot new` files one when you have not
said otherwise; `--orders-dir` moves both (`zot new --orders-dir ~/briefs`),
while `--dir` still says which project the order is *for*.

```bash
# file the brief in a shared folder of briefs
zot new --orders-dir ~/briefs

# run it against a project
zot --dir ~/work/api ~/briefs/1758300000.yaml
```

An order may declare an optional `title:` - a short label for people, shown in
the viewer instead of the order text. Without one the file name is used, its
dashes read as spaces (`fix-the-flaky-test.yaml` becomes "Fix the flaky test"),
so an order named for its moment shows its timestamp until you give it a title.
The title never reaches the agent: the objective is the contract, and a title is
only how you recognise it on screen.

Nothing is remembered between runs. Every order runs from zero each time it is
run, so a bare `zot` runs the whole book every time - an order you have
finished with should leave the folder.

## Watch mode

`--watch` turns a one-shot invocation into a standing one: instead of running
the batch and exiting, zot stays up and runs every `*.yaml` work order that
shows up in the watched target - including orders already sitting there when it
starts - as it arrives. This is how zot becomes a drop-box factory: write an
order, and the running watcher picks it up without a restart.

```bash
# watch this project's own orders - the usual case
zot --watch

# a drop box that is not the book at all - orders are read from anywhere
zot --watch ~/inbox

# a glob, if only some of a folder's files are orders
zot --watch "~/inbox/*.yaml"

# watch one project's orders while working from anywhere else; like an order
# path, a named target resolves against the directory you invoke from
zot --dir ~/work/api --watch
```

Each order runs exactly as it would in a batch (`zot .zot/orders/*.yaml`): a
fresh run with its own session log - one at a time, in filename order. Nothing
remembers that an order ran, so one already in the folder runs again when the
watch starts.

A failed order is reported and the watch goes on - one bad afternoon must not
end the factory - but nothing retries it behind your back: fix or edit the
order and its new content is picked up on the next sweep. The target is swept
every second, so a folder that does not exist yet can be created after the
watch starts. Ctrl-C (or SIGTERM) stops watching and exits.

## Sessions

Every run is written to `~/.local/state/zot/sessions/` as it happens - one JSON
object per line, holding the brief, the model, every message, every tool call
and how it ended.

That matters because an autonomous run is unattended by definition: nobody
watched it, and by the time you look the terminal is gone. The log is what turns
"it failed overnight" into something you can answer.

```bash
# what has run, newest first
zot sessions

# read one
cat ~/.local/state/zot/sessions/20260805-155859.jsonl | jq .

```

A log is a record, not a save point: nothing reads it back into a run. Running
the same order again starts from zero and writes a log of its own.

### Exporting sessions

The log is zot's own shape. `zot sessions export` renders sessions as
**trajectories** - the conversation in the chat convention everything else
reads (`system`, `user`, `assistant` with `tool_calls`, `tool`), with the run's
task, model, outcome and timings beside it - which is what you want for
analysis, evaluation or building a training dataset.

```bash
# the last session, as one JSON line on stdout
zot sessions export

# named sessions into a directory: <id>.jsonl each, screenshots under images/
zot sessions export 20260805-155859 20260805-171012 --out ./trajectories

# every session in the session directory
zot sessions export --all --out ./trajectories
```

`messages` is the whole conversation - it is only ever appended to,
so the end state holds every turn that happened. Each message keeps zot's own `type` beside its `role`, and an assistant turn
carries the model's `reasoning` when the provider surfaced it. Images the model
was shown are copied next to the export and referenced by relative path; on
stdout the turn keeps only its text. The system prompt is not in the log and so
not in the export - `task` is the brief the run was given.

The log is appended and flushed line by line, so it is readable while the run is
still going and a killed run still leaves everything up to the kill. Use
`--no-session` to record nothing, or `--session-dir` (or `ZOT_SESSION_DIR`) to
put the logs somewhere else.

## Project context (`AGENTS.md` & skills)

On startup zot folds in context from two places - the **config directory**
(`~/.config/zot/`, global) and the **working directory** (`--dir`, per-project):

- **`AGENTS.md`** - at the **root** of either directory; its contents are
  appended to the agent's instructions (config first, then project). Use it for
  conventions the agent should always follow.
- **skills** - each `<name>/SKILL.md` (with `name` / `description` YAML front
  matter) is described to the agent in its instructions, and the agent reads a
  skill's full file on demand when it's relevant. Both
  **`.skills/`** (typical at a project root) and **`skills/`** are searched.

```
~/.config/zot/          ./ (your project, --dir)
├── AGENTS.md           ├── AGENTS.md
└── skills/             └── .skills/
    └── greet/              └── deploy/
        └── SKILL.md            └── SKILL.md
```

Everything here is optional - missing files and directories are ignored.
