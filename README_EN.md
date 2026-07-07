# CPA Plugin: CodexComp

[![Go 1.26+](https://img.shields.io/badge/Go-1.26%2B-00ADD8?logo=go)](https://go.dev/)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

[简体中文](README.md) | [English](README_EN.md)

A [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) plugin that detects and repairs gpt-5.5 reasoning truncation in streaming Responses API requests.

gpt-5.5 in agent scenarios frequently stops reasoning at exactly `518n−2` tokens (516, 1034, 1552, ...) — a model-side scheduling behavior, not a context-window limit. This causes unexpected reasoning degradation. This plugin detects the truncation, continues reasoning via `encrypted_content` replay, and folds multiple rounds into a single response transparent to the downstream client.

## Quick Install (Agent)

If you're using an AI agent (Codex, Claude Code, etc.), send it this prompt:

```
Please install the CPA plugin codexcomp for me. Installation instructions are at https://github.com/uf-hy/cpa-plugin-codexcomp/blob/master/SETUP.md — read that document first, then proceed with installation.
```

## How It Works

1. **Intercept**: The plugin registers as a `model_router` + `executor` via CPA's C ABI plugin system, intercepting `gpt-5.5` streaming Responses API requests.
2. **Detect**: After each upstream round completes, it checks `reasoning_tokens` against the `518n−2` pattern. If matched and `encrypted_content` is present, a continuation round is triggered.
3. **Continue**: The continuation round replays the original input plus all previous reasoning items (with `encrypted_content`) and a `phase: commentary` nudge message (`Continue thinking...`). The model resumes from the truncation point instead of restarting.
4. **Fold**: Up to 3 continuation rounds are attempted. Reasoning events are streamed live to the downstream client. Non-reasoning output (message, tool calls) is buffered per round and only flushed when a clean (non-truncated) round completes.
5. **Reconstruct**: The final `response.completed` event is rebuilt with merged output, a folded single-response `usage` view (real multi-round billing in `metadata.proxy_billed_usage`), and fold metadata.

### Async Streaming & First-Byte Latency

gpt-5.5 with high reasoning effort can take 25-30 seconds before the first SSE event. Many clients (including Codex CLI) timeout at 10 seconds. This plugin uses CPA's async streaming mode: the response header is returned immediately, and a goroutine handles the fold logic. Upstream events are forwarded to the client as soon as they arrive — simple queries see first-byte under 500ms, and complex reasoning gets the first event forwarded immediately once upstream produces it.

## Scope

The plugin only intercepts requests that match **all** of:

- Model is `gpt-5.5`
- Source format is `openai-response` (Responses API)
- Request is streaming (`stream: true`)
- `input` is a JSON array (Codex-style)
- No `previous_response_id` present

All other requests pass through to CPA's normal routing.

## How This Differs From codexcomp / CodexCont

| | [CodexCont](https://github.com/neteroster/CodexCont) | [codexcomp](https://github.com/dzshzx/codexcomp) | This Plugin |
|---|---|---|---|
| **Language** | Python (Starlette/uvicorn) | Python (uv) | Go (C ABI shared library) |
| **Deployment** | Standalone local proxy (127.0.0.1:8787) | Standalone local proxy (127.0.0.1:8787) | CPA plugin (loaded in-process) |
| **Integration** | Manual `openai_base_url` rewrite | Manual `openai_base_url` rewrite | Auto-routed by CPA, no config change |
| **Transport** | HTTP/SSE | WebSocket + SSE fallback | CPA host model stream (`host.model.execute_stream`) |
| **Recursion guard** | N/A (separate process) | N/A (separate process) | `host_callback_id` skips own router/interceptors |
| **Concurrency** | Single process | Single process | CPA-managed, plugin goroutine per request |
| **Config** | `config.toml` | Zero-config (uv tool) | Zero-config (self-registers via C ABI) |
| **Fold logic** | Original `518n−2` detection + continuation | Refined fold (transport-agnostic) | Go port of codexcomp's `fold.py` |

CodexCont is the original continuation mechanism. codexcomp refined it into a transport-agnostic fold. This plugin ports that fold logic to Go and integrates it directly into CPA's plugin system, eliminating the need for a separate proxy process.

## Manual Installation

Download the latest `.so` from [Releases](https://github.com/uf-hy/cpa-plugin-codexcomp/releases/latest):

- `codexcomp-linux-amd64.so` — Linux x86_64 (most common)
- `codexcomp-linux-arm64.so` — Linux ARM64

```bash
# Choose one command for the CPU architecture where CPA runs.
# Linux x86_64:
wget -qO <CPA_DIR>/plugins/codexcomp.so \
  "https://github.com/uf-hy/cpa-plugin-codexcomp/releases/latest/download/codexcomp-linux-amd64.so"

# Linux ARM64:
wget -qO <CPA_DIR>/plugins/codexcomp.so \
  "https://github.com/uf-hy/cpa-plugin-codexcomp/releases/latest/download/codexcomp-linux-arm64.so"
```

Enable the plugin:

1. Put `codexcomp.so` in `<CPA_DIR>/plugins/`
2. Enable plugins in `config.yaml`:

```yaml
plugins:
  enabled: true
  dir: plugins
  configs:
    codexcomp:
      enabled: true
      priority: 1
```

3. If using Docker, mount the plugins directory in `docker-compose.yml`:

```yaml
volumes:
  - ./plugins:/CLIProxyAPI/plugins:ro
```

4. Restart CPA.

For AI-agent-friendly installation steps, see [SETUP.md](SETUP.md).

## Configuration

No extra configuration needed. The plugin self-registers via the C ABI and routes automatically.

## Metadata Injection

The final `response.completed` event includes:

- `metadata.proxy_rounds` — per-round info (round number, reasoning tokens, truncation tier `n`)
- `metadata.proxy_billed_usage` — summed usage across all rounds
- `metadata.proxy_stopped_reason` — non-empty when the fold stopped for a non-natural reason (`no_encrypted_content`, `max_continue`, `tier_out_of_window`)

## Benchmark

Tested with the candy problem from [codex-candy-eval](https://github.com/haowang02/codex-candy-eval) — a reasoning depth test that triggers gpt-5.5 truncation. The correct answer is 21.

The repo includes a streaming test script `scripts/candy_eval_cpa.py` that sends streaming Responses API requests to your CPA endpoint:

**Command**:

```bash
python3 scripts/candy_eval_cpa.py \
  --url http://your-cpa:port/v1/responses \
  --key YOUR_KEY -n 5 -r high
```

### Without plugin (baseline)

| Run | Reasoning Tokens | Answer | Correct |
|-----|-----------------|--------|---------|
| 1   | 516             | 29     | ✗       |
| 2   | 516             | 29     | ✗       |
| 3   | 1552            | 21     | ✓       |
| 4   | 516             | 29     | ✗       |
| 5   | 3069            | 21     | ✓       |

**Accuracy: 2/5 (40%)** — 3 out of 5 responses were truncated at 516 tokens (n=1), producing wrong answers.

### With plugin

| Run | Reasoning Tokens | Answer | Correct |
|-----|-----------------|--------|---------|
| 1   | 3641            | 21     | ✓       |
| 2   | 2059            | 21     | ✓       |
| 3   | 3273            | 21     | ✓       |
| 4   | 4555            | 21     | ✓       |
| 5   | 3100            | 21     | ✓       |

**Accuracy: 5/5 (100%)** — all truncations were detected and continued, every answer correct.

## Disclaimer

This plugin relies on non-contractual model behavior (the `518n−2` truncation pattern and `encrypted_content` field). If OpenAI changes the truncation pattern or removes `encrypted_content`, the plugin will simply stop firing and become a transparent passthrough. Continuation rounds consume real tokens; the total is recorded in `metadata.proxy_billed_usage`.

## Acknowledgments

- **[CodexCont](https://github.com/neteroster/CodexCont)** (MIT) — the original continuation mechanism that identified the `518n−2` truncation pattern and pioneered the `encrypted_content` replay approach
- **[codexcomp](https://github.com/dzshzx/codexcomp)** (MIT) — the refined transport-agnostic fold algorithm; this plugin is a direct Go port of its `fold.py`
- **[codex-candy-eval](https://github.com/haowang02/codex-candy-eval)** — the reasoning depth benchmark used in this README
- **[CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI)** — the plugin host framework that makes in-process interception possible
- **[LINUX DO](https://linux.do)** community — where the gpt-5.5 “516” reasoning degradation was discussed, diagnosed, and validated; thanks for the reproductions, deployment feedback, and testing that shaped this plugin

## License

MIT. This plugin includes code derived from [CodexCont](https://github.com/neteroster/CodexCont) and [codexcomp](https://github.com/dzshzx/codexcomp). See [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md) for details.
