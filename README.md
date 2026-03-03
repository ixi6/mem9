<p align="center">
  <img src="assets/logo.png" alt="mnemos" width="180" />
</p>

<h1 align="center">mnemos</h1>

<p align="center">
  <strong>Persistent Memory for AI Agents.</strong><br/>
  Your agents forget everything between sessions. mnemos fixes that.
</p>

<p align="center">
  <a href="https://tidbcloud.com"><img src="https://img.shields.io/badge/Powered%20by-TiDB%20Cloud%20Serverless-E60C0C?style=flat&logo=data:image/svg+xml;base64,PHN2ZyB4bWxucz0iaHR0cDovL3d3dy53My5vcmcvMjAwMC9zdmciIHdpZHRoPSIyNCIgaGVpZ2h0PSIyNCIgdmlld0JveD0iMCAwIDI0IDI0IiBmaWxsPSJub25lIj48cGF0aCBkPSJNMTEuOTk4NCAxLjk5OTAyTDMuNzE4NzUgNy40OTkwMkwzLjcxODc1IDE3TDExLjk5NjQgMjIuNUwyMC4yODE0IDE3VjcuNDk5MDJMMTEuOTk4NCAxLjk5OTAyWiIgZmlsbD0id2hpdGUiLz48L3N2Zz4=" alt="Powered by TiDB Cloud Serverless"></a>
  <a href="https://goreportcard.com/report/github.com/qiffang/mnemos/server"><img src="https://goreportcard.com/badge/github.com/qiffang/mnemos/server" alt="Go Report Card"></a>
  <a href="https://github.com/qiffang/mnemos/blob/main/LICENSE"><img src="https://img.shields.io/badge/license-Apache--2.0-blue.svg" alt="License"></a>
  <a href="https://github.com/qiffang/mnemos"><img src="https://img.shields.io/github/stars/qiffang/mnemos?style=social" alt="Stars"></a>
</p>

---

## The Problem

AI coding agents — Claude Code, OpenCode, OpenClaw, and others — often maintain separate local memory files. The result:

- 🧠 **Amnesia** — Agent forgets everything when a session ends
- 🏝️ **Silos** — One agent can't access what another learned yesterday
- 📁 **Local files** — Memory is tied to a single machine, lost when you switch devices
- 🚫 **No team sharing** — Your teammate's agent can't benefit from your agent's discoveries

**mnemos** gives every agent a shared, cloud-persistent memory with hybrid vector + keyword search — powered by <a href="https://tidbcloud.com"><img src="assets/tidb-logo.png" alt="TiDB Cloud Serverless" height="24" align="center" /></a> [TiDB Cloud Serverless](https://tidbcloud.com).

## Why <img src="assets/tidb-logo.png" alt="TiDB" height="32" align="center" /> TiDB Serverless?

mnemos uses <a href="https://tidbcloud.com"><img src="assets/tidb-logo.png" alt="TiDB Cloud Serverless" height="24" align="center" /></a> [TiDB Cloud Serverless](https://tidbcloud.com) as its storage layer. Here’s why:

| Feature | What it means for you |
|---|---|
| **Free tier** | 25 GiB storage, 250M Request Units/month — enough for most individual and small team use |
| **Native VECTOR type** | Hybrid search (vector + keyword) without a separate vector database |
| **HTTP Data API** | Plugins talk to TiDB via `fetch`/`curl` — no database drivers, no connection pools |
| **Zero ops** | No servers to manage, no scaling to worry about, automatic backups |
| **MySQL compatible** | Migrate to self-hosted TiDB or MySQL anytime |

This architecture means your agent plugins are **truly stateless** — all state lives in TiDB Serverless, making deployments simple and portable.

## Supported Agents

mnemos provides native plugins for major AI coding agent platforms:

| Platform | Plugin | How It Works | Install Guide |
|---|---|---|---|
| **Claude Code** | Hooks + Skills | Auto-loads memories on session start, auto-saves on stop | [`claude-plugin/README.md`](claude-plugin/README.md) — or run `/plugin marketplace add qiffang/mnemos` |
| **OpenCode** | Plugin SDK | `system.transform` injects memories, `session.idle` auto-captures | [`opencode-plugin/README.md`](opencode-plugin/README.md) |
| **OpenClaw** | Memory Plugin | Replaces built-in memory slot (`kind: "memory"`), framework manages lifecycle | [`openclaw-plugin/README.md`](openclaw-plugin/README.md) |
| **Any HTTP client** | REST API / SQL | `curl` to mnemo-server or TiDB HTTP Data API directly | [API Reference](#api-reference-server-mode) |

All plugins expose the same 5 tools: `memory_store`, `memory_search`, `memory_get`, `memory_update`, `memory_delete`.

> **Agent**: To help a user install mnemos, identify which platform they use, then follow the corresponding Install Guide link above. Each guide covers both Direct mode and Server mode.

## Two Modes, One Plugin

The same plugin works in two modes — just change the config:

```
                          ┌─────────────────────────┐
                          │     Agent Plugin         │
                          │ (Claude Code / OpenCode  │
                          │  / OpenClaw / curl)      │
                          └────────────┬─────────────┘
                                       │
                          ┌────────────┴────────────┐
                          │                         │
                   MNEMO_DB_HOST →           MNEMO_API_URL →
                    (direct mode)            (server mode)
                          │                         │
                          ▼                         ▼
                ┌──────────────────┐     ┌───────────────────┐
                │  TiDB Cloud      │     │  mnemo-server     │
                │  Serverless      │     │  (Go, self-host)  │
                │                  │     │                   │
                │  Free tier       │     │  Multi-agent      │
                │  Zero ops        │     │  Space isolation  │
                │  HTTP Data API   │     │  LLM merge (v2)   │
                └──────────────────┘     └────────┬──────────┘
                                                  │
                                         ┌────────┴────────┐
                                         │  TiDB / MySQL   │
                                         └─────────────────┘
```

| | Direct Mode | Server Mode |
|---|---|---|
| **For** | Individual developer, small team | Organization, multi-agent teams |
| **Deploy** | Nothing — use TiDB Cloud free tier | Self-host `mnemo-server` |
| **Config** | Database credentials | API URL + token |
| **Vector search** | Yes (TiDB native VECTOR) | Yes (server-side) |
| **Conflict resolution** | LWW (last writer wins) | LWW → LLM merge (Phase 2) |

**Direct mode is the default.** Mode is inferred from config: `MNEMO_DB_HOST` → direct, `MNEMO_API_URL` → server.

## Quick Start — Direct Mode (30 seconds)

For individual developers. Connect your agent directly to TiDB Cloud — no server to deploy.

1. Create a free [TiDB Cloud Serverless](https://tidbcloud.com) cluster
2. Install the plugin for your agent platform (pick one):

| Platform | Install command |
|---|---|
| Claude Code | **Marketplace**: `/plugin marketplace add qiffang/mnemos` then `/plugin install mnemo-memory@mnemos` <br/> **Manual**: See [`claude-plugin/README.md`](claude-plugin/README.md) |
| OpenCode | Add `"plugin": ["mnemo-opencode"]` to `opencode.json` → set env vars |
| OpenClaw | `npm install mnemo-openclaw` → add to `openclaw.json` |
3. Set your database credentials (Claude Code / OpenCode use env vars, OpenClaw uses `openclaw.json` config):

```bash
# Claude Code / OpenCode — env vars
export MNEMO_DB_HOST="gateway01.us-east-1.prod.aws.tidbcloud.com"
export MNEMO_DB_USER="xxx.root"
export MNEMO_DB_PASS="xxx"
export MNEMO_DB_NAME="mnemos"
```

That's it. The plugin auto-creates the table, loads past memories on session start, and saves new ones on session end. **Zero deployment, zero ops.**

Each plugin's README has the complete step-by-step guide with verification steps.

## Quick Start — Server Mode (Team Setup)

For teams with multiple agents that need to share memory. Deploy a mnemo-server that manages spaces, tokens, and (soon) LLM conflict merge.

**Why server mode?**
- **Space isolation** — each team/project gets its own memory pool
- **Per-agent tokens** — every agent instance gets a scoped API token
- **Centralized control** — rate limiting, auth, and audit in one place
- **LLM conflict merge (Phase 2)** — smart resolution when two agents update the same memory

```bash
# 1. Deploy server
cd server && MNEMO_DSN="user:pass@tcp(host:4000)/mnemos?parseTime=true" go run ./cmd/mnemo-server

# 2. Create a shared space (no auth required for bootstrap)
curl -s -X POST localhost:8080/api/spaces \
  -H "Content-Type: application/json" \
  -d '{"name":"backend-team","agent_name":"alice-claude","agent_type":"claude_code"}'
# → {"ok":true, "space_id":"...", "api_token":"mnemo_abc"}

# 3. Add more agents to the same space
curl -s -X POST localhost:8080/api/spaces/<space_id>/tokens \
  -H "Authorization: Bearer mnemo_abc" \
  -H "Content-Type: application/json" \
  -d '{"agent_name":"bob-opencode","agent_type":"opencode"}'
# → {"ok":true, "api_token":"mnemo_def"}

# 4. Configure any agent to use its token
export MNEMO_API_URL="http://localhost:8080"
export MNEMO_API_TOKEN="mnemo_abc"
```

Each agent uses its own token. All agents in the same space share one memory pool.

## Stateless Agents, Cloud Memory

A key design principle: **agent plugins carry zero state.** All memory lives in TiDB Serverless (direct mode) or mnemo-server (server mode). This means:

- **Agent plugins stay stateless** — deploy any number of agent instances freely; they all share the same memory pool via TiDB
- **Switch machines freely** — your agent's memory follows you, not your laptop
- **Multi-agent collaboration** — Claude Code, OpenCode, OpenClaw, and any HTTP client share the same memories when pointed at the same database
- **No migration needed** — start with Direct mode, switch to Server mode by changing one env var

## Hybrid Search (Vector + Keyword)

Search auto-upgrades when an embedding provider is configured:

```
                    Embedding provider configured?
                    ┌─────────┴─────────┐
                   Yes                  No
                    │                    │
              Hybrid search        Keyword only
              (vector + keyword)   (LIKE '%q%')
                    │
         ┌──────────┴──────────┐
    Vector results         Keyword results
    (ANN cosine)           (substring match)
         └──────────┬──────────┘
              Merge & rank
```

- **No embedding config** → keyword search works immediately
- **Add an API key** → hybrid search activates automatically
- **No schema migration** — VECTOR column is nullable from day one

Supports OpenAI, Ollama, LM Studio, or any OpenAI-compatible endpoint:

```bash
# OpenAI (default)
export MNEMO_EMBED_API_KEY="sk-..."

# Ollama (local, free)
export MNEMO_EMBED_BASE_URL="http://localhost:11434/v1"
export MNEMO_EMBED_MODEL="nomic-embed-text"
export MNEMO_EMBED_DIMS="768"
```

## API Reference (Server Mode)

Auth: `Authorization: Bearer <token>`. Server resolves token → space + agent.

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/api/memories` | Create/upsert. Server generates embedding if configured. |
| `GET` | `/api/memories` | Search: `?q=`, `?tags=`, `?source=`, `?key=`, `?limit=`, `?offset=` |
| `GET` | `/api/memories/:id` | Get single memory |
| `PUT` | `/api/memories/:id` | Update. Optional `If-Match` for version check. |
| `DELETE` | `/api/memories/:id` | Delete |
| `POST` | `/api/memories/bulk` | Bulk create (max 100) |
| `POST` | `/api/spaces` | Create space + first token (no auth) |
| `POST` | `/api/spaces/:id/tokens` | Add agent to space |
| `GET` | `/api/spaces/:id/info` | Space metadata |

## Self-Hosting

### Environment Variables

| Variable | Required | Default | Description |
|----------|----------|---------|-------------|
| `MNEMO_DSN` | Yes | — | Database connection string |
| `MNEMO_PORT` | No | `8080` | HTTP listen port |
| `MNEMO_RATE_LIMIT` | No | `100` | Requests/sec per IP |
| `MNEMO_RATE_BURST` | No | `200` | Burst size |
| `MNEMO_EMBED_API_KEY` | No | — | Embedding provider API key |
| `MNEMO_EMBED_BASE_URL` | No | OpenAI | Custom embedding endpoint |
| `MNEMO_EMBED_MODEL` | No | `text-embedding-3-small` | Model name |
| `MNEMO_EMBED_DIMS` | No | `1536` | Vector dimensions |

### Build & Run

```bash
cd server
go build -o mnemo-server ./cmd/mnemo-server
MNEMO_DSN="user:pass@tcp(host:4000)/mnemos?parseTime=true" ./mnemo-server
```

### Docker

```bash
docker build -t mnemo-server ./server
docker run -e MNEMO_DSN="..." -p 8080:8080 mnemo-server
```

## Project Structure

```
mnemos/
├── server/                     # Go API server (server mode)
│   ├── cmd/mnemo-server/       # Entry point
│   ├── internal/
│   │   ├── config/             # Env var config loading
│   │   ├── domain/             # Core types, errors, token generation
│   │   ├── embed/              # Embedding provider (OpenAI/Ollama/any)
│   │   ├── handler/            # HTTP handlers + chi router
│   │   ├── middleware/         # Auth + rate limiter
│   │   ├── repository/         # Interface + TiDB SQL implementation
│   │   └── service/            # Business logic (upsert, LWW, hybrid search)
│   ├── schema.sql
│   └── Dockerfile
│
├── opencode-plugin/            # OpenCode agent plugin (TypeScript)
│   └── src/                    # Plugin SDK tools + hooks + dual-mode backend
│
├── openclaw-plugin/            # OpenClaw agent plugin (TypeScript)
│   ├── index.ts                # Tool registration (mode-agnostic)
│   ├── direct-backend.ts       # Direct: @tidbcloud/serverless → SQL
│   ├── server-backend.ts       # Server: fetch → mnemo API
│   └── embedder.ts             # Embedding provider abstraction
│
├── claude-plugin/              # Claude Code plugin (Hooks + Skills)
│   ├── hooks/                  # Lifecycle hooks (bash + curl)
│   └── skills/                 # memory-recall + memory-store skills
│
└── docs/DESIGN.md              # Full design document
```

## Roadmap

| Phase | What | Status |
|-------|------|--------|
| **Phase 1** | Core server + CRUD + auth + hybrid search + upsert + dual-mode plugins | ✅ Done |
| **Phase 2** | LLM conflict merge, auto-tagging | 🔜 Planned |
| **Phase 3** | Web dashboard, bulk import/export, CLI wizard | 📋 Planned |

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for development setup and guidelines.

## License

[Apache-2.0](LICENSE)

---

<p align="center">
  <a href="https://tidbcloud.com"><img src="assets/tidb-logo.png" alt="TiDB Cloud Serverless" height="36" /></a>
  <br/>
  <sub>Built with <a href="https://tidbcloud.com">TiDB Cloud Serverless</a> — zero-ops database with native vector search.</sub>
</p>
