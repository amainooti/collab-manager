# outreach

Small backend with three functions — `research`, `draft_pitch`, `update_status` —
callable via REST or as an MCP server, backed by SQLite. A server-rendered
kanban web UI sits on top. A Telegram bot (phase 2) would call the same
`core.Service` the web UI and REST API already use.

## Layout

```
cmd/outreach/        entrypoint: `serve` (REST + web) or `mcp` (stdio MCP server)
internal/core/       the 3 functions + shared types — no HTTP/MCP/SQL/provider specifics
internal/store/      SQLite persistence (modernc.org/sqlite, no cgo)
internal/llmshared/  prompt text + JSON parsing shared by every model provider
internal/anthropicx/ Claude Sonnet: web-search-backed research, pitch drafting
internal/hfx/        Hugging Face Inference Providers: same job, free/open models, no web search
internal/llmrouter/  tries Claude first, falls back to Hugging Face on any error
internal/api/        REST handlers wrapping core.Service
internal/web/        kanban board + project detail pages (html/template, no JS build)
internal/mcpserver/  MCP tool server wrapping core.Service
```

## Run

```sh
cp .env.example .env   # fill in ANTHROPIC_API_KEY and/or HF_TOKEN
set -a; source .env; set +a

go run ./cmd/outreach serve   # http://localhost:8090
go run ./cmd/outreach mcp     # stdio MCP server
```

## Model providers

Claude (Sonnet 5) is the primary provider — it's the only one with web search, so
research briefs from it are grounded in current information. Hugging Face is an
optional fallback: if `HF_TOKEN` is set, any Claude failure (rate limited, quota
exhausted, an outage, or `ANTHROPIC_API_KEY` simply not being set) falls through to
a free/open model via Hugging Face's Inference Providers router instead of failing
the request outright.

- **At least one of** `ANTHROPIC_API_KEY` / `HF_TOKEN` **must be set**, or the
  server refuses to start.
- Only `ANTHROPIC_API_KEY` set → Claude only, same as before.
- Only `HF_TOKEN` set → runs entirely on the free model (no fallback to wire up).
- Both set → Claude first, Hugging Face as fallback (`internal/llmrouter`).
- `HF_MODEL` (default `meta-llama/Llama-3.1-8B-Instruct`) picks the model; `HF_TOKEN`
  is a free Hugging Face access token. The Hugging Face path has no web-search tool,
  so its research briefs are from the model's own knowledge — it's told to say so
  plainly rather than invent specifics, and every brief/draft records which model
  produced it (`Brief.Model` / `Draft.Model`, shown as a badge in the web UI).

## REST API

- `GET  /api/projects`
- `POST /api/projects`                        `{name, links}`
- `GET  /api/projects/{id}`                    full detail: project + brief + drafts + history
- `POST /api/projects/{id}/research`           `{refresh: bool}` — cached unless refresh
- `POST /api/projects/{id}/draft`              `{goal, context}`
- `POST /api/projects/{id}/status`             `{stage, notes}`

## Notes

- Research is two Claude Sonnet calls: one with the `web_search_20260209` tool to
  gather findings, one (no tools) to shape the findings into structured JSON. Kept as
  prompted JSON rather than `output_config.format` since the exact Go binding for
  that field wasn't available to verify against the installed SDK version.
- `internal/core` depends only on small `DB`/`LLM` interfaces, not on `store`,
  `anthropicx`, or `hfx` directly — swapping storage or model providers doesn't
  touch it. `internal/anthropicx` and `internal/hfx` both implement `core.LLM`
  and share prompt text via `internal/llmshared`; `internal/llmrouter` composes
  the two into a third `core.LLM` that neither provider package knows exists.
