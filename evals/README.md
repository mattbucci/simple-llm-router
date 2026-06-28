# Evals

Two lightweight, dependency-light test harnesses that exercise a **running**
router end-to-end against a real fleet.

## 1. API evals (`run_evals.py`)

A self-contained [uv](https://docs.astral.sh/uv/) script (deps declared inline
via [PEP 723](https://peps.python.org/pep-0723/), so no venv to manage). It hits
the router's HTTP API directly and checks the behaviors the ADRs promise.

```bash
# Terminal 1: start the router against the real fleet
go build -o bin/router ./cmd/router
./bin/router --config config.local.yaml

# Terminal 2: run the evals
uv run evals/run_evals.py
RUN_SLOW=1 uv run evals/run_evals.py     # also run the (slow) fusion eval
```

What it covers:

| Check | ADR |
|-------|-----|
| `/healthz`, `/readyz` | 0011 |
| `/v1/models` discovery | 0005 |
| chat completion (north, gemma) | 0001 |
| reasoning fields survive passthrough | 0001 |
| alias → upstream-id model rewrite | 0004 |
| SSE streaming + `[DONE]` | 0007 |
| multimodal image part (gemma) | 0008 |
| pareto concrete-model selection | 0013 |
| `plugins` accepted + stripped | 0001/0013 |
| unknown model → 404 | 0006 |
| Anthropic `/v1/messages` consumer | 0016 |
| fusion synthesis (slow) | 0014 |

Exit code is the number of failed checks; a JSON report lands in
`evals/report/last.json`.

Override the fleet model ids if your config differs:

```bash
NORTH_ID=... GEMMA_ID=gemma4-31b ROUTER_URL=http://localhost:8080 uv run evals/run_evals.py
```

## 2. opencode harness (`opencode_smoke.sh`)

Drives the router with the real [opencode](https://opencode.ai) agent, proving a
genuine coding agent can use it. Relies on the project `opencode.jsonc`, which
registers the router as an OpenAI-compatible provider (`router/north`, etc.).

```bash
./evals/opencode_smoke.sh                       # north, gemma, smart
./evals/opencode_smoke.sh router/north          # a specific alias
```
