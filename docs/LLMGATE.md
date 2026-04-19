# llmgate

> A provider-agnostic LLM gateway for Go — the "LiteLLM for Go" that
> the engine uses internally and that stands alone as a separate OSS
> library.

## Purpose

Give any Go program a single interface for talking to multiple LLM
providers, with production features bolted in: router, fallback, cost
tracking, retries, prompt caching, capability flags.

Vectorless uses it internally. Its existence as a separate library is
deliberate — other Go projects will want the same thing, and having it
standalone (a) generates community interest, (b) forces cleaner
boundaries, (c) keeps the engine repo focused on retrieval.

## Repo

`llmgate` (no `vectorless-` prefix — it stands alone).

Lives in-tree as `pkg/llm/` inside `vectorless-engine` today. Extracts
to its own repo after the `internal -> pkg` refactor proves the
boundary.

## What it does

- Provide a small interface that abstracts over providers:
  Anthropic, OpenAI, Gemini, Bedrock, Ollama, etc.
- Route requests across models based on policy: "prefer Sonnet,
  fall back to GPT-4o on 5xx, fall back to Haiku on budget breach."
- Track cost per call with a unified `Usage` struct.
- Surface capability flags so callers branch on features, not vendor
  names.
- Wrap providers in middleware: retries with jitter, rate limits,
  prompt caching, PII redaction.

## What it does not do

- **Model inference.** It's a client library, not a model runner.
- **Streaming UI.** It passes streams through; rendering them is the
  caller's job.
- **Prompt templating.** No Jinja, no Handlebars. Go strings are fine.
- **Agent orchestration.** Tool-calling loops, ReAct agents, planner
  chains — not in scope. That's what langchaingo or framework-level
  code is for.
- **Vector operations.** We're vectorless. The gateway does not do
  embeddings unless a genuine use-case appears later, and even then it
  would be a separate sub-package.

## Foundation: langchaingo

The tedious part of any LLM gateway is maintaining 15+ HTTP clients
for each vendor's quirky API. Rewriting that is pure drudgery.

**llmgate depends on `github.com/tmc/langchaingo/llms`** for the
provider-adapter layer. langchaingo already ships clean `Model`
implementations for OpenAI, Anthropic, Bedrock, Google, Cohere,
Mistral, Ollama, HuggingFace, Cloudflare Workers AI, Ernie,
Llamafile, Maritaca, Watsonx, and a fake model for testing.

What langchaingo does well:

- Small interface: `GenerateContent(ctx, []MessageContent, ...CallOption)`.
- Prompt caching, token counting, reasoning-model support are in
  separate files at the package root.
- Actively maintained, MIT, Go 1.24+.

What langchaingo does not do (and where llmgate earns its keep):

- **No router.** Each `Model` is standalone.
- **No fallback logic.**
- **No cost tracking.** Usage is returned from each provider in its
  own shape.
- **No unified capability discovery.** You have to know "model X
  supports JSON mode" by reading the code.

llmgate composes langchaingo providers behind its own interface and
adds those missing pieces as middleware.

## Rejected alternatives

- **Bifrost** (`maximhq/bifrost`) — a 7k-line framework, multi-module
  repo, forces a specific worldview (`Account` interface, fasthttp,
  sonic, plugin pipelines). An app, not a library. Excellent as a
  standalone sidecar gateway; wrong shape as a Go import.
- **go-litellm** — a client for the Python LiteLLM proxy. Wrong
  layer — it requires running the Python gateway.
- **litellm-go** — a 45-line round-robin weekend project. Too small.
- **Rolling our own provider HTTP clients** — rewriting the boring
  80% with no upside.

## The interface

The shape that should stay stable for a long time:

```go
// Client is the one interface everything composes over.
type Client interface {
    Complete(ctx context.Context, req Request) (*Response, error)
    Stream(ctx context.Context, req Request) (<-chan Event, error)
    CountTokens(ctx context.Context, text string) (int, error)
    Capabilities() Capabilities
}

type Request struct {
    Model       string
    Messages    []Message
    MaxTokens   int
    Temperature float64
    JSONMode    bool
    JSONSchema  []byte
    Tools       []Tool          // for function calling
}

type Response struct {
    Content      string
    Usage        Usage
    Model        string
    FinishReason string
    ToolCalls    []ToolCall
}

type Usage struct {
    InputTokens     int
    OutputTokens    int
    CacheReadTokens int
    CacheWriteTokens int
    CostUSD         float64
}

type Capabilities struct {
    MaxContext        int
    SupportsJSONMode  bool
    SupportsStreaming bool
    SupportsToolUse   bool
    SupportsCaching   bool
}
```

## Components

### Providers

Each provider is a `Client` implementation that wraps the corresponding
`langchaingo/llms` `Model`. The adapter translates between llmgate's
request/response types and langchaingo's, plus fills in cost and
capabilities from a static table.

```
llmgate/
  providers/
    anthropic/    - wraps llms.anthropic
    openai/       - wraps llms.openai
    gemini/       - wraps llms.googleai
    bedrock/      - wraps llms.bedrock
    ollama/       - wraps llms.ollama
    mock/         - canned responses for tests
```

### Router

```go
type Router struct {
    Primary   Client
    Fallbacks []Fallback
}

type Fallback struct {
    Client    Client
    TriggerOn func(err error, usage Usage) bool  // 5xx, 429, over-budget, etc.
}
```

The router tries `Primary`; on an error matching any `Fallback.TriggerOn`
predicate, it retries with that fallback. Preserves the original error
if all fallbacks fail.

### Cost tracker

A standalone middleware that wraps any `Client`, observes the `Usage`
on each response, and emits metrics. Zero dependencies; just accepts a
callback.

```go
func WithCostTracking(c Client, onUsage func(Usage)) Client
```

The pricing table is shipped as a Go map keyed by `(provider, model)`,
updated as vendor prices change. Not fetched at runtime — we want
deterministic behaviour and no external dependency for billing
calculations.

### Retry middleware

Exponential backoff with jitter on 429 and 5xx. Respects
`Retry-After` headers when present. Cap at `MaxRetries`, default 3.

### Cache middleware

Content-addressed result cache keyed by the hash of `(model, messages,
max_tokens, temperature, json_mode)`. In-memory LRU by default;
pluggable Redis for multi-replica deploys. Short TTL (minutes) —
LLM outputs are not cache-friendly for long, but a hot-path dashboard
can benefit.

Distinct from **provider-native prompt caching** (Anthropic's
`cache_control`), which is a flag on the provider, not something
llmgate implements itself.

### Capability flags

A map of `(provider, model) -> Capabilities` shipped as data. Callers
can ask "does this model support JSON mode?" without knowing the vendor.
Updated as vendors add features.

## Usage from the engine

The engine never constructs providers directly. It takes a
`llmgate.Client` in its config and calls `Complete` / `CountTokens`:

```go
pipeline := ingest.NewPipeline(ingest.Pipeline{
    LLM: llmgate.NewRouter(
        llmgate.Anthropic(anthConfig),
        llmgate.WithFallback(
            llmgate.OpenAI(oaConfig),
            llmgate.OnStatus(429, 500, 502, 503, 504),
        ),
    ),
    ...
})
```

The engine doesn't know or care which provider actually handled the
call.

## Testing

`llmgate/providers/mock` returns canned responses based on predicates.
The engine's retrieval tests use this today (see
`internal/retrieval/retrieval_test.go`) and will continue to after the
extraction.

Integration tests against real providers live behind an
`LLMGATE_INTEGRATION_TESTS=1` env flag so CI doesn't spend money by
default.

## Licensing

Apache-2.0. Permissive, explicit patent grant, enterprise-friendly.

## Open questions

- **Embeddings sub-package.** If llmgate ever gains an `Embed`
  interface, it should live behind a separate build tag so consumers
  who never need embeddings don't pull in the dep graph.
- **Tool use / function calling.** Anthropic, OpenAI, and Gemini all
  support it, each with a different shape. A unified `Tool` type is
  straightforward; the hard part is making the response stream work
  consistently across them.
- **Observability.** Currently the only observability is the
  `onUsage` callback. A proper OTel instrumentation package should
  land once the interface settles.
- **Budget guardrails.** Per-request and per-day dollar caps as
  middleware, so a misconfigured router can't burn $10k overnight.

## Related docs

- [ENGINE.md](./ENGINE.md) — the primary consumer.
- [SDKS.md](./SDKS.md) — unrelated to llmgate; the SDKs talk to the
  vectorless server, which internally uses llmgate.
