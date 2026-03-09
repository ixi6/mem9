---
title: Evolving Mnemos into an OpenClaw ContextEngine Plugin
status: Draft
date: 2026-03-09
---

## Overview

OpenClaw v2026.3.7-beta.1 introduces a first-class `ContextEngine` plugin slot (PR #22201)
that grants plugins ownership of the full context lifecycle: bootstrap, assemble, ingest,
compact, subagent handoff. The current mnemos openclaw-plugin occupies the `memory` slot and
registers four lifecycle hooks (`before_prompt_build`, `after_compaction`, `before_reset`,
`agent_end`). This proposal describes a two-phase migration from that model to full
`ContextEngine` ownership, with a backward-compatible hook fallback for pre-beta.1 users.

---

## Problem Statement

The current hook model has five concrete gaps:

| Gap | Current hook | Impact |
|---|---|---|
| No pre-compaction flush | `after_compaction` is a log-only no-op | Memories lost when the context window is squashed |
| `after_compaction` is a no-op | fires after the window is gone | Plugin cannot react in time to save anything |
| Pinned memories cost tokens every turn | `before_prompt_build` injects all memories together | `prependContext` is not provider-cached |
| `allowPromptInjection` undocumented | `before_prompt_build` return value | beta.1 silently strips memory injection without this flag |
| No subagent memory handoff | `agent_end` only captures the current agent | Child agents start with an empty memory context |

---

## Proposed Changes

### Phase 1 — Immediate Hook Fixes (~140 LoC, low risk)

These are backward-compatible and target the existing hook surface.

**1. `allowPromptInjection` startup warning**

`allowPromptInjection` is a **host-side** hook policy field set in the user's `openclaw.json`
under `plugins.entries.mem9.hooks.allowPromptInjection`. The plugin cannot read or enforce it
— it is owned by the OpenClaw framework. The right intervention is a startup log that tells
the operator what to configure; adding it to `PluginConfig` would be wrong (the plugin has no
mechanism to observe or enforce a host-level policy).

```ts
// index.ts — in register(), emit once at startup
api.logger.info(
  "[mnemo] IMPORTANT: On OpenClaw beta.1+, memory injection requires " +
  '"hooks.allowPromptInjection": true in your plugin entry. ' +
  "Without it, before_prompt_build context injection is silently disabled."
);
```

No `PluginConfig` field added — this is documentation/guidance only.

**2. System-prompt caching for pinned memories**

`before_prompt_build` now supports `prependSystemContext` (PR #35177). Pinned memories
(stable facts, user preferences) should move there — they sit in the provider-cached system
prompt portion and no longer cost per-turn tokens.

Change in `hooks.ts` `before_prompt_build`:
```ts
const { pinned, dynamic } = splitByType(memories);
return {
  prependSystemContext: pinned.length > 0 ? formatMemoriesBlock(pinned, "system") : undefined,
  prependContext: dynamic.length > 0 ? formatMemoriesBlock(dynamic, "context") : undefined,
};
```

**3. Pre-compaction flush via `session:compact:before`**

The `after_compaction` hook is currently a no-op. Replace with `session:compact:before` to
flush the in-window conversation to mem9 ingest *before* the window is squashed.

```ts
api.on("session:compact:before", async (event: unknown) => {
  const evt = event as { messages?: unknown[]; sessionId?: string };
  await flushMessagesToIngest(backend, evt.messages, evt.sessionId, logger, maxIngestBytes);
  logger.info("[mnemo] Pre-compaction flush complete — memories preserved");
});
```

**Fallback policy when mem9 is unavailable during compaction (Phase 1 hook path):**

The hook path must not block compaction under any circumstances. The failure policy is:

1. **Timeout + bounded retry**: Wrap the `backend.ingest()` call with a 5-second timeout.
   If the call times out or throws, enqueue the serialized messages into a module-level
   durable queue (`compactQueue: Array<{messages, sessionId, ts}>`).
2. **Retry worker**: A `setInterval` worker (30-second cadence) drains `compactQueue` by
   replaying failed ingest calls. Queue is bounded to 10 entries (oldest-evicted) to cap
   memory growth. Each entry retries at most 3 times before being dropped with a warning log.
3. **Watchdog alerting**: If the queue depth exceeds 5 entries, emit a single `logger.error`
   per compaction cycle so operators know ingest is degraded.
4. **Explicit fail-safe**: After exhausting retries, the message batch is dropped and
   `logger.error("[mnemo] compact flush exhausted retries — batch dropped")` fires. This is
   the explicit data-loss boundary; it is logged, bounded, and operator-visible.

```ts
// hooks.ts — compact flush with retry queue
const compactQueue: Array<{ messages: unknown[]; sessionId: string; ts: number; attempts: number }> = [];
const COMPACT_QUEUE_LIMIT = 10;
const COMPACT_MAX_RETRIES = 3;
const COMPACT_TIMEOUT_MS = 5_000;

async function flushWithTimeout(backend: MemoryBackend, payload: IngestInput): Promise<void> {
  await Promise.race([
    backend.ingest(payload),
    new Promise<never>((_, reject) => setTimeout(() => reject(new Error("timeout")), COMPACT_TIMEOUT_MS)),
  ]);
}

api.on("session:compact:before", async (event: unknown) => {
  const evt = event as { messages?: unknown[]; sessionId?: string };
  const sessionId = evt.sessionId ?? `ses_${Date.now()}`;
  const selected = selectAndFormat(evt.messages, maxIngestBytes); // existing helper
  if (!selected.length) return;

  try {
    await flushWithTimeout(backend, { messages: selected, session_id: sessionId, agent_id: agentId, mode: "smart" });
    logger.info("[mnemo] Pre-compaction flush complete");
  } catch (err) {
    logger.error(`[mnemo] compact flush failed (${String(err)}) — queuing for retry`);
    if (compactQueue.length >= COMPACT_QUEUE_LIMIT) compactQueue.shift(); // evict oldest
    compactQueue.push({ messages: selected, sessionId, ts: Date.now(), attempts: 0 });
  }
});

// Retry worker — drains compactQueue in background
setInterval(async () => {
  if (compactQueue.length > 5) logger.error(`[mnemo] compact queue depth=${compactQueue.length} — ingest may be degraded`);
  for (let i = compactQueue.length - 1; i >= 0; i--) {
    const entry = compactQueue[i];
    entry.attempts++;
    try {
      await flushWithTimeout(backend, { messages: entry.messages as IngestMessage[], session_id: entry.sessionId, agent_id: agentId, mode: "smart" });
      compactQueue.splice(i, 1);
    } catch {
      if (entry.attempts >= COMPACT_MAX_RETRIES) {
        logger.error(`[mnemo] compact flush exhausted retries — batch dropped (sessionId=${entry.sessionId})`);
        compactQueue.splice(i, 1);
      }
    }
  }
}, 30_000);
```

> **Pre-beta.1 limitation**: `session:compact:before` does not exist on old OpenClaw. For
> pre-beta.1 users, compaction remains a memory-loss event — the window is squashed before
> any hook fires, leaving nothing to flush. The only partial mitigation is the existing
> `before_reset` hook, which saves the last 3 user messages on explicit `/reset`. Automatic
> compaction is not covered. This is a known limitation with no clean fix on the old API
> surface.

**Design note — alternatives considered for compaction flushing:**

| Option | Pros | Cons | Decision |
|---|---|---|---|
| **In-process retry queue (chosen)** | No external deps; bounded memory; operator-visible via logs | Lost on process restart; retries only while agent is running | Accepted — restart scenario is rare; persistent queue adds deployment complexity disproportionate to the risk |
| Persistent local file queue (e.g., `~/.mnemo/compact-queue.json`) | Survives process restart | Filesystem I/O in hot path; file locking; complicates plugin teardown | Rejected — overkill for a best-effort local queue |
| Skip on error, no retry | Simplest code | Silent data loss; no operator visibility | Rejected — violates the explicit fail-safe requirement |

**4. `tool_result_persist` transcript cleanup**

Injected `<relevant-memories>` blocks appear in tool result messages and get re-ingested as
if they were user content. Wire `stripInjectedContext` to the `tool_result_persist` hook.

```ts
api.on("tool_result_persist", (event: unknown) => {
  const evt = event as { content?: string };
  if (evt?.content) {
    return { content: stripInjectedContext(evt.content) };
  }
});
```

**5. `message_received` search pre-warm**

Currently `before_prompt_build` runs the search synchronously while the user waits. Pre-warm
the search on `message_received` (earlier in the pipeline) and cache the
`Promise<SearchResult>` keyed by prompt fingerprint. `before_prompt_build` then awaits the
already-in-flight promise.

```ts
const preWarmCache = new Map<string, Promise<SearchResult>>();

api.on("message_received", (event: unknown) => {
  const evt = event as { prompt?: string };
  const key = evt?.prompt?.slice(0, 200);
  if (key && key.length >= MIN_PROMPT_LEN && !preWarmCache.has(key)) {
    preWarmCache.set(key, backend.search({ q: key, limit: MAX_INJECT }));
    setTimeout(() => preWarmCache.delete(key), 3 * 60 * 1000); // 3-min TTL
  }
});
```

**Design note — alternatives considered for pre-warm cache:**

| Option | Pros | Cons | Decision |
|---|---|---|---|
| **In-memory Promise cache keyed by prompt prefix (chosen)** | Zero deps; 3-min TTL bounds growth; `before_prompt_build` awaits an already-in-flight promise | Cache is process-local; does not survive restarts; stale on rapid prompt edits | Accepted — TTL and eviction-on-hit keep it safe; restart cost is one extra search latency |
| No pre-warm (search at `before_prompt_build`) | Simplest code; no cache invalidation | Adds full search RTT to every user-visible prompt delay | Rejected — measurable UX regression, especially on remote mem9 servers |
| Persistent cache (Redis / local file) | Survives restart | External dependency or filesystem I/O; over-engineered for a < 3 min TTL optimization | Rejected — complexity far exceeds benefit |

**Phase 1 total: ~140 LoC — plugin only, no server changes**

> **Effort re-estimate (Phase 1):**
>
> | Bucket | LoC |
> |---|---|
> | Implementation | ~140 |
> | Contract validation (hook payload shapes verified against beta.1) | ~10 test assertions |
> | Compatibility tests (pre-beta.1 + beta.1 codepaths) | ~30 |
> | Release hardening (docs update, changelog) | ~20 prose lines |

---

### Phase 2 — ContextEngine Interface (~280 LoC)

New file: `openclaw-plugin/context-engine.ts`

This implements the `ContextEngine` interface and is registered only when
`api.capabilities?.contextEngine` is detected, preserving full backward compatibility with
pre-beta.1 deployments.

The Phase 2 code below is an **interface sketch** — method names, ctx field shapes, and the
`ctx.session` API must be verified against the actual beta.1 SDK type exports before
implementation. See Open Questions 1 and 2.

Known gaps to resolve before writing real code:
- `Logger` interface is not exported from `hooks.ts` — must be exported or redefined locally.
- `formatAndClean()` does not exist — must be extracted from `agent_end` handler in `hooks.ts`.
- `ctx.session`, `ctx.latestPrompt`, `ctx.childAgentId` — field names unverified against beta.1.
- `ContextEngineBootstrapCtx` / `ContextEngineAssembleCtx` etc. — type names unverified.

**Contract-validation checklist (hard gate — must pass before any Phase 2 coding begins):**

All items below must be verified against the actual beta.1 SDK source or changelog before
writing `context-engine.ts`. This checklist is a merge requirement for Phase 2.

| # | Item | SDK reference | Acceptance criteria |
|---|---|---|---|
| CV-1 | Capability detection mechanism | `api.capabilities?.contextEngine` (boolean) OR `typeof api.registerContextEngine === "function"` | Identify the correct guard; close Open Question 1 |
| CV-2 | `ContextEngine` type name and import path | beta.1 type exports | Confirm `ContextEngine` is the exported interface name |
| CV-3 | `bootstrap` ctx fields: `session.set`, `session.get` signatures | `ContextEngineBootstrapCtx` type | Confirm field names exactly |
| CV-4 | `assemble` ctx fields: `latestPrompt` (or equivalent) | `ContextEngineAssembleCtx` type | Confirm field name; check if prompt may be undefined |
| CV-5 | `ingest` ctx fields: `messages`, `sessionId`, `agentId` | `ContextEngineIngestCtx` type | Confirm all three field names |
| CV-6 | `compact` ctx fields: same as `ingest` | `ContextEngineCompactCtx` type | Confirm; note whether compact and ingest share a base ctx type |
| CV-7 | `prepareSubagentSpawn` ctx: `childAgentId` | `ContextEnginePrepareSpawnCtx` type | Confirm field name; confirm return shape `{pluginConfig:{...}}` |
| CV-8 | `prepareSubagentSpawn` return — agent identity field name | `PluginConfig` in beta.1 | Current contract uses `agentName` (`types.ts:13`). Confirm whether child config uses `agentName` or a different field (see Contract Consistency note below) |
| CV-9 | `message_received` payload: `prompt` field name | beta.1 hook payload types | Close Open Question 4 (hook payload shapes) |
| CV-10 | `tool_result_persist` payload: `content` field name | beta.1 hook payload types | Close Open Question 4 |
| CV-11 | `session:compact:before` payload: `messages`, `sessionId` field names | beta.1 hook payload types | Close Open Question 4 |
| CV-12 | `allowPromptInjection` scope: hooks only, or also `assemble()` | beta.1 docs or source | Close Open Question 3 |
| CV-13 | `prependSystemContext` supported by all configured providers | beta.1 provider matrix | Verify before enabling for pinned memories; add test case |

```ts
// context-engine.ts (sketch — verify SDK types before implementing)
import type { MemoryBackend } from "./backend.js";
// Logger must be exported from hooks.ts first:
import type { Logger } from "./hooks.js";
// formatAndClean must be extracted from agent_end logic in hooks.ts:
import { selectMessages, formatMemoriesBlock, formatAndClean } from "./hooks.js";

export function buildContextEngine(
  backend: MemoryBackend,
  logger: Logger,
  opts: { maxIngestBytes?: number; tenantID: string }
) {
  return {
    async bootstrap(ctx: /* verify */ unknown) {
      // Use memory_type=pinned filter, NOT tags:"pinned"
      // Requires memory_type added to SearchInput (see Phase 2 prerequisite below)
      const pinned = await backend.search({ memory_type: "pinned", limit: 20 });
      (ctx as { session: { set: (k: string, v: unknown) => void } })
        .session.set("mnemo:pinned", pinned.data);
      logger.info(`[mnemo] bootstrap: prefetched ${pinned.data.length} pinned memories`);
    },

    async assemble(ctx: /* verify */ unknown) {
      const c = ctx as { session: { get: (k: string) => unknown }; latestPrompt?: string };
      const pinned = (c.session.get("mnemo:pinned") as Memory[]) ?? [];
      const result = await backend.search({ q: c.latestPrompt, limit: 10 });
      const dynamic = result.data.filter(m => m.memory_type !== "pinned");
      return {
        prependSystemContext: pinned.length > 0 ? formatMemoriesBlock(pinned) : undefined,
        prependContext: dynamic.length > 0 ? formatMemoriesBlock(dynamic) : undefined,
      };
    },

    async ingest(ctx: /* verify */ unknown) {
      const c = ctx as { messages?: unknown[]; sessionId?: string; agentId?: string };
      if (!c.messages?.length) return;
      const selected = selectMessages(formatAndClean(c.messages), opts.maxIngestBytes);
      if (!selected.length) return;
      await backend.ingest({
        messages: selected,
        session_id: c.sessionId ?? `ses_${Date.now()}`,
        agent_id: c.agentId ?? "agent",
        mode: "smart",
      });
    },

    async compact(ctx: /* verify */ unknown) {
      // Flush window BEFORE OpenClaw squashes it.
      // compact + ingest can race — see duplicate-ingest design below.
      const c = ctx as { messages?: unknown[]; sessionId?: string; agentId?: string };
      if (!c.messages?.length) return;
      const selected = selectMessages(formatAndClean(c.messages), opts.maxIngestBytes);
      if (!selected.length) return;
      try {
        await Promise.race([
          backend.ingest({
            messages: selected,
            session_id: c.sessionId ?? `ses_${Date.now()}`,
            agent_id: c.agentId ?? "agent",
            mode: "smart",
          }),
          new Promise<never>((_, reject) => setTimeout(() => reject(new Error("timeout")), 5_000)),
        ]);
        logger.info(`[mnemo] compact: flushed ${selected.length} messages`);
      } catch (err) {
        // Log and enqueue for retry — do NOT block compaction on ingest failure.
        // Retry logic (in-process queue with 30s worker, 3 max retries, 10-entry bound)
        // is shared with the Phase 1 hook path. See compactQueue + retry worker in hooks.ts.
        logger.error(`[mnemo] compact flush failed: ${String(err)} — queued for retry`);
        enqueueCompactRetry(selected, c.sessionId ?? `ses_${Date.now()}`);
      }
      // Do NOT replace ctx.summary — let OpenClaw produce its structural summary
    },

    async afterTurn(_ctx: unknown) { /* reserved */ },

    async prepareSubagentSpawn(ctx: /* verify */ unknown) {
      const c = ctx as { childAgentId?: string };
      // NOTE: current contract uses `agentName` (types.ts:13, PluginConfig.agentName).
      // `agentId` is NOT a PluginConfig field. Use `agentName` until CV-8 confirms the
      // child plugin config field name for beta.1. This is a required contract fix before
      // this sketch becomes real code.
      return { pluginConfig: { tenantID: opts.tenantID, agentName: c.childAgentId ?? "subagent" } };
    },

    async onSubagentEnded(_ctx: unknown) {
      // Phase 3: query child-agent memories and surface to parent
    },
  };
}
```

**Phase 2 prerequisite — `SearchInput` extension:**

Before `bootstrap` and `assemble` can use `memory_type` filtering, `SearchInput` in `types.ts`
and `ServerBackend.search()` in `server-backend.ts` must be extended. The server already
accepts and applies `memory_type` (and `agent_id`, `session_id`) — only the plugin client is
missing these fields:

```ts
// types.ts — extend SearchInput
export interface SearchInput {
  q?: string;
  tags?: string;
  source?: string;
  limit?: number;
  offset?: number;
  memory_type?: string;   // ADD: maps to server ?memory_type=
  agent_id?: string;      // ADD: maps to server ?agent_id=
  session_id?: string;    // ADD: maps to server ?session_id=
}
```

```ts
// server-backend.ts — forward new fields in search()
if (input.memory_type) params.set("memory_type", input.memory_type);
if (input.agent_id)    params.set("agent_id",    input.agent_id);
if (input.session_id)  params.set("session_id",  input.session_id);
```

No server changes required — the server already handles these query parameters.

Registration in `index.ts` (version-guarded):

```ts
// Version-guarded: keep hooks for pre-beta.1 OpenClaw compatibility
// `resolveTenantID` is the existing lazy-resolution function defined in register().
// It resolves cfg.tenantID or auto-provisions. It is NOT a new field — it is the
// same function already used to create LazyServerBackend for hooks.
if (api.capabilities?.contextEngine) {
  // tenantID must be resolved before registering the ContextEngine.
  // Use the same resolveTenantID() function that LazyServerBackend uses.
  const tenantID = await resolveTenantID(cfg.agentName ?? "agent");
  api.registerContextEngine(buildContextEngine(hookBackend, api.logger, {
    maxIngestBytes: cfg.maxIngestBytes,
    tenantID,                   // resolved string, not a lazy reference
  }));
  api.logger.info("[mnemo] ContextEngine mode active (beta.1+)");
} else {
  registerHooks(api, hookBackend, api.logger, { maxIngestBytes: cfg.maxIngestBytes });
  api.logger.info("[mnemo] Hook mode active (pre-beta.1 compatibility)");
}
```

> **Contract note — `agentName` vs `agentId`**: The current `PluginConfig` (`types.ts:13`)
> defines `agentName?: string` as the agent identity field. The sketch above uses `agentName`
> consistently. Any new field added to `PluginConfig` for subagent identity must be named
> `agentName` unless CV-8 validation reveals that beta.1 uses a different field name, in which
> case both the type definition and this sketch must be updated together.

**Phase 2 total: ~280 LoC — plugin only, no server changes**

> **Effort re-estimate (Phase 2):**
>
> | Bucket | LoC / effort |
> |---|---|
> | Implementation (`context-engine.ts` + registration) | ~280 LoC |
> | Contract validation (CV-1 through CV-13 checklist) | ~40 assertions / spike |
> | Compatibility tests (pre-beta.1 path + beta.1 path, capability detection) | ~50 LoC |
> | Migration note (PluginConfig field additions, agentName alignment) | ~10 LoC + changelog entry |
> | Release hardening (README update, version bump) | ~15 prose lines |

---

### Phase 3 — Subagent Memory Continuity (~150 LoC, plugin only)

Deferred until beta.1 stabilizes. Scope:

- `onSubagentEnded`: query child-agent memories by filtering `agent_id=childAgentId`, surface
  top-N discoveries to parent via `backend.ingest` or `memory_store`.
- Cross-agent search: expose `agent_id` filter in the plugin's `SearchInput` (see Phase 2
  prerequisite — already covers this).

> **Phase 3 requires no server changes.** The server already accepts and applies `?agent_id=`
> and `?session_id=` as WHERE clause filters in search queries (`handler/memory.go:131`,
> `repository/tidb/memory.go:541`). The work is entirely on the plugin client side — adding
> `agent_id` to `SearchInput` and forwarding it in `ServerBackend.search()`, which is already
> captured as a Phase 2 prerequisite. Phase 3 only adds the `onSubagentEnded` logic that uses
> the filter.

---

## File Changes Summary

| File | Change | LoC | Phase |
|---|---|---|---|
| `hooks.ts` | `session:compact:before` flush; `tool_result_persist` cleanup; `message_received` pre-warm; system context split; export `Logger` type; extract `formatAndClean()` | ~90 | 1+2 |
| `types.ts` | Add `memory_type`, `agent_id`, `session_id` to `SearchInput` | ~10 | 2 prereq |
| `server-backend.ts` | Forward `memory_type`, `agent_id`, `session_id` in `search()` | ~10 | 2 prereq |
| `index.ts` | Startup warning for `allowPromptInjection`; version-guarded ContextEngine registration | ~30 | 1+2 |
| `context-engine.ts` | New file — `buildContextEngine()` with all 7 lifecycle methods | ~180 | 2 |
| `backend.ts` | No changes | — | — |
| `server/` | No changes | — | — |

**All phases are plugin-only. No server changes required.**
**Phase 1+2 total: ~320 LoC**
**Phase 3 total: ~150 LoC (uses SearchInput extension already in Phase 2)**

---

## Backward Compatibility

| Scenario | Behavior |
|---|---|
| OpenClaw < beta.1 | Hook path active; compact remains a memory-loss event (no `session:compact:before`); retry queue handles transient mem9 unavailability |
| OpenClaw < beta.1, `/reset` used | `before_reset` saves last 3 user messages — partial mitigation only |
| OpenClaw beta.1+, `api.capabilities?.contextEngine` absent | Hook path active (same as pre-beta.1 row above); startup warning logged for `allowPromptInjection` |
| OpenClaw beta.1+, `api.capabilities?.contextEngine` present | ContextEngine path active; full lifecycle ownership including safe compact with retry queue |
| OpenClaw beta.1+, ContextEngine active, `allowPromptInjection` not set in openclaw.json | Startup warning logged; `assemble()` return values may or may not be suppressed depending on CV-12 outcome (see Risks) |

> **Note**: ContextEngine activation is controlled by `api.capabilities?.contextEngine` — a
> runtime capability flag. It is independent of `allowPromptInjection`, which is a host-side
> hook policy that affects `before_prompt_build` injection. The prior draft incorrectly implied
> that setting `allowPromptInjection: true` activates the ContextEngine path.

---

## Risks & Mitigations

| Risk | Mitigation |
|---|---|
| beta.1 ContextEngine API unstable before GA | Version guard; keep hook path as permanent fallback |
| `compact` + `ingest` fire concurrently for the same session | **Not mitigated by server-side dedup** — ingest is not idempotent on `session_id`, no content-hash dedup exists. Mitigation: use a per-session in-memory flag (`compacting: Set<sessionId>`) to skip `ingest` while compact is in progress; or scope `compact` and `ingest` as mutually exclusive writers per session |
| mem9 unavailable during compaction | 5-second timeout + in-process retry queue (10-entry bound, 3 retries per batch, 30s retry cadence). Queue depth > 5 triggers `logger.error` for operator visibility. After 3 retries, batch is dropped with explicit error log. Compaction is never blocked. |
| `assemble` token budget overshoot | Conservative estimate: 1 token ≈ 4 bytes; prefer under-injecting |
| Pre-warm cache grows unbounded | 3-min TTL + Map key eviction on `before_prompt_build` hit |
| `prependSystemContext` treated as mutable by some providers | **Verification test required (CV-13)**: Before enabling, add an integration test that sends a multi-turn conversation with a pinned memory in `prependSystemContext` and asserts (a) the memory appears in turn 1 context, (b) turn 2 does not re-charge tokens for it (provider cache hit). If the test fails for any configured provider, fall back to `prependContext` for that provider. Only put truly stable (`memory_type=pinned`) memories in `prependSystemContext`. |
| `allowPromptInjection` scope unknown — may suppress `assemble()` too | **Verification test required (CV-12)**: On beta.1 with `allowPromptInjection` absent, call `assemble()` and assert its return value appears in the assembled context. If suppressed, add a dedicated `assemble()` capability check or emit a startup warning. Expected outcome: `assemble()` is NOT subject to `allowPromptInjection` (it is a ContextEngine method, not a hook); if that expectation is wrong, the risk escalates to a blocker. |

---

## Open Questions

Questions are categorized as **blocking** (must close before Phase 2 coding) or **non-blocking**.

### Blocking Decisions

These are not optional clarifications — they directly affect runtime behavior and API
compatibility. Each must be closed before Phase 2 implementation begins (see CV checklist above).

**DEC-1 — Capability detection mechanism**
- **Decision**: Does OpenClaw beta.1 expose `api.capabilities.contextEngine` as a boolean, or
  is detection done via `typeof api.registerContextEngine === "function"`?
- **Owner**: plugin maintainer
- **Due**: before Phase 2 sprint starts
- **Acceptance criteria**: One of the two guards is confirmed by reading beta.1 source or
  changelog; CV-1 is checked off; `index.ts` registration guard is updated accordingly.

**DEC-2 — ContextEngine SDK type names and ctx field shapes**
- **Decision**: Exact TypeScript type names for `ContextEngine`, `ContextEngineBootstrapCtx`,
  `ContextEngineAssembleCtx`, `ContextEngineIngestCtx`, `ContextEngineCompactCtx`,
  `ContextEnginePrepareSpawnCtx`; exact field names for `ctx.session`, `ctx.latestPrompt`,
  `ctx.childAgentId`, `ctx.messages`, `ctx.sessionId`, `ctx.agentId`.
- **Owner**: plugin maintainer
- **Due**: before Phase 2 sprint starts
- **Acceptance criteria**: CV-2 through CV-8 are all checked off; `context-engine.ts` replaces
  all `/* verify */` casts with typed parameters.

**DEC-3 — `allowPromptInjection` scope in ContextEngine mode**
- **Decision**: Does the `allowPromptInjection` host policy suppress `assemble()` return values,
  or does it only affect `before_prompt_build` hook injection?
- **Owner**: plugin maintainer (verify against beta.1 docs or source)
- **Due**: before Phase 2 sprint starts
- **Acceptance criteria**: CV-12 verification test result is recorded; if `assemble()` is also
  suppressed, a separate capability check or startup warning is added to `context-engine.ts`.

**DEC-4 — Hook payload shapes for Phase 1 hooks**
- **Decision**: Exact event payload field names for `message_received` (`prompt`?),
  `tool_result_persist` (`content`?), and `session:compact:before` (`messages`?, `sessionId`?).
- **Owner**: plugin maintainer
- **Due**: before Phase 1 items 3-5 land
- **Acceptance criteria**: CV-9, CV-10, CV-11 verified; all `as { field?: type }` casts in
  `hooks.ts` replaced with assertions or types based on confirmed shapes.

### Non-Blocking

**Q5 — Pinned memories scope**: Should `bootstrap` prefetch pinned memories globally for the
tenant, or scoped to the current `agent_id`? Affects the `SearchInput` call in bootstrap. Can
be decided during Phase 2 implementation based on observed behavior.

**Q6 — Versioning**: Should Phase 1 ship as a patch release and Phase 2 as a minor, given the
behavioral difference between hook and ContextEngine mode? Decide during release prep.

---

## Recommended Sequencing

1. **Now**: Land Phase 1 item 1 (`allowPromptInjection` startup warning) — prevents silent
   breakage for anyone already on beta.1. Low risk, no logic change.
2. **This sprint**: Land Phase 1 items 2-5 (system context split, compact flush with retry
   queue, transcript cleanup, pre-warm) — all hook-surface, no API dependency, no server
   changes. Close DEC-4 (hook payload shapes, CV-9/10/11) before items 3-5 land.
3. **Before Phase 2**: Close all four blocking decisions (DEC-1 through DEC-4) and pass the
   full contract-validation checklist (CV-1 through CV-13). This is a hard gate — no Phase 2
   code is written until every checklist item has an confirmed answer. Export `Logger` from
   `hooks.ts`; extract `formatAndClean()`. Add `memory_type`/`agent_id`/`session_id` to
   `SearchInput` and `ServerBackend.search()`.
4. **Next sprint**: Implement `context-engine.ts` behind the version guard — safe to merge to
   main before beta.1 GA since it is unreachable on older versions.
5. **After beta.1 GA**: Enable ContextEngine path by default; deprecate hook fallback.
6. **Phase 3**: Add `onSubagentEnded` logic using the `agent_id` search filter already
   unlocked in Phase 2.
