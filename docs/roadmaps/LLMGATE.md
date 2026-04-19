# llmgate roadmap

> Design doc: [../LLMGATE.md](../LLMGATE.md)

"LiteLLM for Go." Starts as `pkg/llm/` in the engine repo, extracts to
its own repo once the interface stabilises.

## Phase 0 — in-repo foundation *(current)*

One-line: a working provider-agnostic interface used by the engine.

- [x] `llm.Client` interface with `Complete` + `CountTokens`
- [x] Request / Response / Message / Usage types
- [x] Anthropic live client (direct HTTP, retries, count_tokens
      endpoint)
- [x] ErrNotImplemented stubs for OpenAI + Gemini
- [x] Engine consumes the interface (single-pass + chunked-tree)
- [x] Mock client for unit tests

---

## Phase 1 — swap foundation to langchaingo

One-line: delete the handwritten HTTP client; adopt langchaingo as the
provider-adapter layer.

- [ ] Add `github.com/tmc/langchaingo/llms` dependency
- [ ] Build a thin adapter `type llmgateAdapter struct { M llms.Model }`
      that implements our `Client`
- [ ] Swap Anthropic impl to wrap `llms.anthropic.New()`
- [ ] Add wrappers for OpenAI (`llms.openai`), Gemini (`llms.googleai`),
      Bedrock, Ollama
- [ ] Retire the custom HTTP client in `anthropic.go` (keep the retry
      + count_tokens logic in a shared middleware layer)
- [ ] Verify retrieval tests still pass against the mock
- [ ] Verify live Anthropic integration test still passes

---

## Phase 2 — the value-add layer

One-line: add the features langchaingo deliberately doesn't ship —
router, fallback, cost, capabilities.

- [ ] **Router**
  - [ ] `Router` struct with `Primary` + `[]Fallback`
  - [ ] `Fallback` struct with `Client` + `TriggerOn(err, usage) bool`
  - [ ] Helpers: `OnStatus(...)`, `OnRateLimit()`, `OnError(err)`,
        `OnBudgetExceeded()`
  - [ ] Preserves original error when all fallbacks fail

- [ ] **Cost tracking**
  - [ ] Static price table keyed by `(provider, model)`
  - [ ] `Usage.CostUSD` populated on every response
  - [ ] `WithCostTracking(Client, onUsage func(Usage)) Client`
        middleware
  - [ ] Tests verify cost math for Anthropic + OpenAI

- [ ] **Capability flags**
  - [ ] `Capabilities{MaxContext, SupportsJSONMode,
        SupportsStreaming, SupportsToolUse, SupportsCaching}`
  - [ ] Static table keyed by `(provider, model)`
  - [ ] `Client.Capabilities()` method on every impl
  - [ ] Engine strategies branch on capabilities, not vendor names

- [ ] **Middleware: retries**
  - [ ] Exponential backoff + jitter
  - [ ] Respects `Retry-After` headers
  - [ ] Configurable `MaxRetries`
  - [ ] `WithRetries(Client, ...Option) Client`

- [ ] **Middleware: in-memory cache**
  - [ ] Content-addressed cache key: hash of
        `(model, messages, max_tokens, temperature, json_mode)`
  - [ ] LRU, configurable size + TTL
  - [ ] Hit/miss metrics
  - [ ] `WithCache(Client, CacheConfig) Client`

- [ ] **Middleware: budget guardrails**
  - [ ] Per-request dollar cap
  - [ ] Per-hour / per-day dollar cap
  - [ ] Reject with `ErrBudgetExceeded` when over

---

## Phase 3 — streaming + tool use

One-line: the two features that separate "toy wrapper" from
"production gateway."

- [ ] **Streaming**
  - [ ] `Client.Stream(ctx, Request) (<-chan Event, error)`
  - [ ] `Event` union type: `Delta`, `ToolCallDelta`, `Done`
  - [ ] Anthropic, OpenAI, Gemini streaming impls
  - [ ] Router + cache middleware pass streams through correctly

- [ ] **Tool use / function calling**
  - [ ] Unified `Tool` + `ToolCall` types across providers
  - [ ] Anthropic + OpenAI + Gemini translations
  - [ ] Tool-use examples in docs

---

## Phase 4 — extract to its own repo

One-line: stop being "that folder in the engine repo."

- [ ] Create `llmgate` repo (no `vectorless-` prefix — stands alone)
- [ ] Move `pkg/llm/` content out
- [ ] Engine updates go.mod to depend on `llmgate` externally
- [ ] llmgate has its own README, CHANGELOG, release cycle
- [ ] First tagged release `v0.1.0`
- [ ] Announce on `r/golang` and HN when the feature set is real

---

## Phase 5 — ecosystem polish

- [ ] OpenTelemetry instrumentation package
      (`llmgate/instrumentation/otel`)
- [ ] Prometheus metrics package
- [ ] (opt) Redis-backed distributed cache
- [ ] (opt) Embeddings sub-package (`llmgate/embed`) behind a build
      tag
- [ ] (opt) Go 1.25 iterators for streaming responses
- [ ] Example apps: chatbot, RAG, structured extraction

---

## Cross-cutting

- [ ] Price table update process (monthly or on vendor announcement)
- [ ] Capability table update process
- [ ] Integration test suite against real providers, gated by
      `LLMGATE_INTEGRATION_TESTS=1` env
- [ ] Benchmark harness comparing overhead vs direct langchaingo calls
      (should be < 1% p50 latency)

## Known issues / deferred

- [ ] Tool-use streaming is genuinely hard cross-provider; ship
      non-streaming first
- [ ] Anthropic prompt caching is provider-native — our cache
      middleware is a separate concern and they can coexist
- [ ] No plans for hosted model inference (vLLM, TGI, Together.ai) in
      v1 — but the interface is provider-agnostic, so adding one is a
      ~200-line PR

## Related

- [../LLMGATE.md](../LLMGATE.md) — design doc.
- [ENGINE.md](./ENGINE.md) — the primary consumer.
