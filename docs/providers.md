# Providers: connecting zot to inference

A provider is a named inference connection: an endpoint and a credential. It may
reach a model service directly, a local server, or a gateway. **zot ships none.**
Every provider is declared in your config file, and a run needs one selected - by
`default_provider`, or per run with `--provider`.

For the agent itself - the run loop, tools - see [how-it-works.md](how-it-works.md); for safety, [safety.md](safety.md).

## Declaring a provider

Anything that speaks the OpenAI chat-completions API works. Name a provider, give
it a base URL and a key, then say which provider and model a run uses:

```yaml
# ~/.config/zot/config.yaml
default_provider: mygateway

agent:
  model: my-model

providers:
  mygateway:
    base_url: https://gateway.internal.example.com/v1
    api_key: '$GATEWAY_KEY'
```

`zot config` opens the file in `$EDITOR`, seeding it from a template that already
has this shape.

There is **no default provider and no default model**. A config that declares
neither fails before any request is made, saying what to add - a provider and a
model that cannot talk to each other otherwise fail as a provider error rather
than a configuration one, which is much harder to read.

| Key        | Meaning                                                                                  |
| ---------- | ---------------------------------------------------------------------------------------- |
| `base_url` | The endpoint root. Required. `https`, unless it is loopback.                             |
| `api_key`  | The credential. Required, unless `base_url` is loopback. Literal, or a `$ENV_VAR` reference. |
| `driver`   | The wire implementation. Optional: the only one is `openai`, and it is the default.      |
| `models`   | An optional custom model list - see [below](#custom-model-lists).                        |

The map key (`mygateway`) is only the name you select with `--provider` and
`default_provider`; it selects nothing else. A provider called `openai` gets no
endpoint and no key on account of its name.

## The one driver

`driver: openai` means the OpenAI-compatible chat-completions API - `POST
{base_url}/chat/completions`, streamed. It is not restricted to OpenAI: it is
the wire format that OpenAI, Groq, Mistral, DeepSeek, OpenRouter, Together, a
local Ollama or llama.cpp, and most gateways speak. Any other `driver` value is
rejected when the config loads.

## Credentials

The key is whatever the provider says it is - nothing is read from a conventional
variable such as `OPENAI_API_KEY` on your behalf. A credential is scoped to the
host it was issued for, and guessing which one belongs to a URL somebody typed
is how a key ends up in someone else's logs. To keep the secret out of the file,
reference a variable you export:

```yaml
providers:
  mygateway:
    base_url: https://gateway.internal.example.com/v1
    api_key: '$GATEWAY_KEY'
```

```bash
export GATEWAY_KEY="…"
zot
```

An unset variable resolves to nothing, so a missing key is reported as a missing
key rather than sent to the endpoint as the literal text `$GATEWAY_KEY`.

A key can also be set per model, where one gateway fronts several upstreams that
each want their own - see below.

## Several providers

Declare as many as you like and pick one per run:

```yaml
default_provider: work

providers:
  work:
    base_url: https://gateway.internal.example.com/v1
    api_key: '$GATEWAY_KEY'
  local:
    base_url: http://localhost:11434/v1     # loopback: plaintext, and no key needed
```

```bash
zot --provider local --model llama-4 "…"
```

The endpoint must be `https` unless it is loopback (`localhost`, `127.0.0.0/8`,
`::1`); only a loopback endpoint may go without a key.

## Custom model lists

The `models` block on a provider is optional. Without it, zot accepts the model
named by `agent.model` or `--model`; catalogued models contribute their known
context and capabilities, while a newly released unknown model still runs with
conservative defaults.

Define `models` when a connection should expose a deliberate list. Its map keys
become the allowed names for that provider, and each entry can alias the real
model ID or override model-specific settings:

```yaml
default_provider: corporate
agent:
  model: fast

providers:
  corporate:
    base_url: https://models.example.com/v1
    api_key: $CORPORATE_MODEL_KEY
    models:
      fast:
        model: gpt-5.4-mini
        max_iterations: 50
      deep:
        model: gpt-5.4
        api_key: $DEEP_MODEL_KEY      # this model's own credential
```

When a list exists, selecting any other model is a configuration error. This
makes the list useful as an intentional connection boundary. Omit it to accept
any model name.

### Overriding what a model can do

A model entry can also correct what zot believes about the model. `vision` is a
tri-state: unset defers to the catalogue, `true` and `false` decide.

```yaml
providers:
  corporate:
    base_url: https://models.example.com/v1
    api_key: $CORPORATE_MODEL_KEY
    models:
      stealth/ox-alpha:
        vision: true       # zot has never heard of it, but it can be shown images
        context: 200000
      gpt-5.4:
        vision: false      # this deployment strips image parts
```

| Key | Overrides |
| --- | --- |
| `vision` | whether the model can be shown images, which decides whether it is offered the `view` tool |
| `context` | its total context window, in tokens |
| `content_array` | send every message's content as an array of parts, for a self-hosted llama.cpp whose chat template rejects a bare string |

**`vision` is the one worth knowing about.** The catalogue's default for an
unrecognised model is *blind*, deliberately: a model wrongly assumed to take
tools fails on its first turn, loudly and cheaply, while a model wrongly assumed
to see is sent an attachment its endpoint rejects in the middle of a long
unattended run - or silently drops, leaving it to describe a picture it never
received. So an uncatalogued model is never offered `view`, and never told
images exist, until you say otherwise here.

## Gateways and prefixed models

A model gateway - one endpoint fronting many providers - addresses models by a
provider-qualified name like `openai/gpt-5.4` or `anthropic/claude-5-sonnet`.
zot sends the model name exactly as you give it: it never adds or rewrites a
prefix, so give the gateway the name it routes by. zot still resolves the real
context window behind the prefix.

```yaml
default_provider: router
agent:
  model: z-ai/glm-5.2

providers:
  router:
    base_url: https://openrouter.ai/api/v1
    api_key: '$ROUTER_KEY'
```

Cloudflare AI Gateway is configured the same way; its endpoint carries your
account and gateway ids, so its `base_url` is your gateway's compat URL:
`https://gateway.ai.cloudflare.com/v1/<account>/<gateway>/compat`.

zot sends no app-attribution headers to any endpoint - it names itself to
nobody.
