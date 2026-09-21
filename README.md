# agent

Agent is an autonomous coding agent in a single Go binary. It takes a work
order - an objective, acceptance criteria and constraints - and runs until the
work is done, talking straight to any OpenAI-compatible model provider with your
own key.

## Build

```bash
make build
```

Needs Go 1.27 or newer and runs on Linux. `make` lists the other targets.

## Use

```bash
agent config   # declare the provider and its models
agent new      # write an order in your editor
agent .agent/orders/<name>.md
```

`agent --help` lists the commands and flags, and
[configs/agent.example.yaml](configs/agent.example.yaml) documents every config key.

## Safety

Agent has real file-write and shell access and `--dir` is not a sandbox. Point it
at a disposable checkout.

## Status

Agent is 0.x, so flags, config and behavior may change. See
[AGENTS.md](AGENTS.md) to work on it.
