# Work orders

zot takes **work orders, not prompts**. A work order is a small YAML file: the
durable objective, the acceptance criteria that define "done", and the
constraints the work must hold to. Write one, hand it over, and walk away:

```bash
zot new    # opens a blank order in your editor; write the objective there
zot
```

zot reads the repo, writes and edits the code, runs the build and the tests, and
fixes what it broke - on its own, from that one order to a recorded outcome. When
it stops you have a changed working tree and a full session log of every step it
took. No chat loop, no approving each edit: one order in, finished work out.

## Writing one

`zot new` creates a blank order in `.zot/orders/` and opens it in your editor
(`$VISUAL`, then `$EDITOR`), the same way `zot config` opens the config. It
takes no prose: the objective, the acceptance criteria and the constraints are
written in the file, where they can be reviewed, not squeezed onto a command
line.

The file is named for the moment it was made - a unix timestamp, like
`1758300000.yaml` - so there is nothing to invent and orders sort in the order
they were written. Close the editor without writing anything and no order is
kept. `--dir` says which project the order is for, and `--orders-dir` files it
somewhere else, such as a shared folder of briefs or a drop box a watcher is
pointed at.

An order may declare an optional `title:` - a short label for people, shown in
the viewer instead of the order text. The title never reaches the agent: the
objective is the contract.

## Orders compose

Orders are files, so they compose. A bare `zot` runs the project's whole book -
every order in `.zot/orders/`, each as its own run, in filename order, stopping
at the first that does not end in success. Naming order files runs exactly
those, from any path:

```bash
zot                                   # the project's orders
zot ~/briefs/add-rate-limiting.yaml   # exactly this one
zot ~/briefs/*.yaml                   # exactly these
```

## Every run starts from zero

Nothing remembers that an order ran. Running an order again is running it fresh:
a new conversation, a new session log, nothing carried over from the last time -
not a half-finished run, not a finished one. A bare `zot` runs the whole book
every time, so an order you have finished with should leave the folder.

## Orders persist

Orders are files, and files belong in repositories. Committing an order makes
it part of the project: reviewable in a pull request, versioned with the code
it asks for, and there for whoever - or whatever - runs it next.

A *standing order* is one committed file meant to run forever, each run
producing new work from the same words. What carries from run to run is
whatever the order tells the agent to read first - a catalogue, a log of
previous passes - which is how each run knows what the last one did.

The [factories](https://github.com/openzot) are standing orders in public:
[the arcade](https://github.com/openzot/arcade)'s `orders/new-game.yaml` makes
a new browser game every shift, [the
machinery](https://github.com/openzot/machinery)'s builds a new machine, and
[the whetstone](https://github.com/openzot/whetstone)'s hones the same game
version after version. Each is a small YAML file committed at the root of its
repository, handed to zot every 30 minutes by a cron - the whole factory is the
order, the conventions file beside it, and a gate script.

Persisted orders run together like any others: `zot orders/*.yaml` runs
exactly those, in filename order, and a bare `zot` runs the project's whole
book.

## Orders stream

`zot --watch` keeps zot up and runs every `*.yaml` order that lands in the
folder as it arrives - a drop-box factory:

```bash
zot --watch            # this project's own orders
zot --watch ~/inbox    # any folder, or a glob
```

See [watch mode](configuration.md#watch-mode).

## After the run

Every run is written to disk as it happens, so a run nobody watched is still
answerable afterwards. See [sessions](configuration.md#sessions).
