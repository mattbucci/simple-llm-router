# /// script
# requires-python = ">=3.10"
# dependencies = ["httpx>=0.27"]
# ///
"""
simple-llm-router eval harness.

Runs a battery of behavioral checks against a *running* router, exercising the
guarantees the ADRs promise: transparent passthrough, model rewrite + plugins
stripping, streaming, multimodal, pareto selection, fusion, the Anthropic
consumer endpoint, and error mapping.

Usage (router must already be running, e.g. ./bin/router --config config.local.yaml):

    uv run evals/run_evals.py
    ROUTER_URL=http://localhost:8080 RUN_SLOW=1 uv run evals/run_evals.py

Exit code == number of failed checks (0 = all green).
"""
from __future__ import annotations

import base64
import json
import os
import struct
import sys
import time
import zlib
from dataclasses import dataclass, field

import httpx

ROUTER_URL = os.environ.get("ROUTER_URL", "http://localhost:8080").rstrip("/")
RUN_SLOW = os.environ.get("RUN_SLOW", "") not in ("", "0", "false")
TIMEOUT = float(os.environ.get("EVAL_TIMEOUT", "150"))

# Upstream model ids the router rewrites aliases to. Defaults match the generic
# config.example.yaml; override NORTH_ID / GEMMA_ID to match your own config.
NORTH_ID = os.environ.get("NORTH_ID", "/models/North-Mini-Code-1.0-fp8")
GEMMA_ID = os.environ.get("GEMMA_ID", "gemma4-31b")

GREEN, RED, YELLOW, DIM, RESET = "\033[32m", "\033[31m", "\033[33m", "\033[2m", "\033[0m"


@dataclass
class Result:
    name: str
    ok: bool
    detail: str = ""
    skipped: bool = False
    ms: int = 0


RESULTS: list[Result] = []


def record(name, ok, detail="", skipped=False, ms=0):
    RESULTS.append(Result(name, ok, detail, skipped, ms))
    if skipped:
        tag = f"{YELLOW}SKIP{RESET}"
    elif ok:
        tag = f"{GREEN}PASS{RESET}"
    else:
        tag = f"{RED}FAIL{RESET}"
    extra = f" {DIM}({ms}ms){RESET}" if ms else ""
    print(f"  [{tag}] {name}{extra}")
    if detail:
        print(f"        {DIM}{detail}{RESET}")


def check(name, slow=False):
    """Decorator: run a check fn, capturing pass/fail/skip and timing."""
    def deco(fn):
        def run():
            if slow and not RUN_SLOW:
                record(name, True, "set RUN_SLOW=1 to run", skipped=True)
                return
            t0 = time.time()
            try:
                detail = fn() or ""
                record(name, True, detail, ms=int((time.time() - t0) * 1000))
            except AssertionError as e:
                record(name, False, str(e), ms=int((time.time() - t0) * 1000))
            except Exception as e:  # noqa: BLE001
                record(name, False, f"{type(e).__name__}: {e}", ms=int((time.time() - t0) * 1000))
        run.__name__ = fn.__name__
        return run
    return deco


def client() -> httpx.Client:
    return httpx.Client(base_url=ROUTER_URL, timeout=TIMEOUT)


def chat(model, messages, *, stream=False, extra=None):
    body = {"model": model, "messages": messages}
    if extra:
        body.update(extra)
    if stream:
        body["stream"] = True
    return body


def tiny_png(width=16, height=16, rgb=(220, 40, 40)) -> str:
    """Build a valid solid-color PNG with stdlib only; return base64."""
    def chunk(tag: bytes, data: bytes) -> bytes:
        return (struct.pack(">I", len(data)) + tag + data
                + struct.pack(">I", zlib.crc32(tag + data) & 0xFFFFFFFF))

    sig = b"\x89PNG\r\n\x1a\n"
    ihdr = struct.pack(">IIBBBBB", width, height, 8, 2, 0, 0, 0)
    row = b"\x00" + bytes(rgb) * width
    raw = row * height
    idat = zlib.compress(raw, 9)
    png = sig + chunk(b"IHDR", ihdr) + chunk(b"IDAT", idat) + chunk(b"IEND", b"")
    return base64.b64encode(png).decode()


# --------------------------------------------------------------------------- #
# Checks
# --------------------------------------------------------------------------- #

@check("healthz returns 200")
def t_healthz():
    with client() as c:
        r = c.get("/healthz")
        assert r.status_code == 200, f"got {r.status_code}"


@check("readyz returns 200 (>=1 backend healthy)")
def t_readyz():
    with client() as c:
        # Give the health loop a moment to complete its first probe.
        for _ in range(20):
            r = c.get("/readyz")
            if r.status_code == 200:
                return f"ready: {r.text.strip()[:60]}"
            time.sleep(1)
        raise AssertionError(f"never became ready (last {r.status_code})")


@check("/v1/models lists discovered upstream ids")
def t_models():
    with client() as c:
        r = c.get("/v1/models")
        assert r.status_code == 200, f"got {r.status_code}: {r.text[:200]}"
        ids = {m["id"] for m in r.json().get("data", [])}
        assert NORTH_ID in ids or GEMMA_ID in ids, f"expected fleet ids in {sorted(ids)}"
        return f"{len(ids)} models advertised"


@check("chat: north (non-stream) returns content")
def t_chat_north():
    with client() as c:
        # north reasons before answering; give it room so the reasoning phase
        # doesn't exhaust the budget (reasoning-model trait, not a router issue).
        r = c.post("/v1/chat/completions", json=chat(
            "north", [{"role": "user", "content": "Reply with exactly: OK"}],
            extra={"max_tokens": 256, "temperature": 0}))
        assert r.status_code == 200, f"got {r.status_code}: {r.text[:200]}"
        j = r.json()
        content = j["choices"][0]["message"]["content"]
        assert content and content.strip(), "empty content"
        return f"content={content.strip()[:40]!r}"


@check("chat: gemma (non-stream) returns content")
def t_chat_gemma():
    with client() as c:
        r = c.post("/v1/chat/completions", json=chat(
            "gemma", [{"role": "user", "content": "Reply with exactly: OK"}],
            extra={"max_tokens": 64, "temperature": 0}))
        assert r.status_code == 200, f"got {r.status_code}: {r.text[:200]}"
        content = r.json()["choices"][0]["message"]["content"]
        assert content and content.strip(), "empty content"
        return f"content={content.strip()[:40]!r}"


@check("passthrough: north reasoning fields survive (ADR-0001)")
def t_reasoning_passthrough():
    with client() as c:
        r = c.post("/v1/chat/completions", json=chat(
            "north", [{"role": "user", "content": "Briefly: what is 2+2?"}],
            extra={"max_tokens": 256, "temperature": 0}))
        assert r.status_code == 200, f"got {r.status_code}: {r.text[:200]}"
        j = r.json()
        msg = j["choices"][0]["message"]
        usage = j.get("usage", {})
        has_reasoning = ("reasoning_content" in msg) or (usage.get("reasoning_tokens", 0) > 0)
        assert has_reasoning, (
            "neither message.reasoning_content nor usage.reasoning_tokens present — "
            "router may be dropping non-standard fields")
        return f"reasoning_tokens={usage.get('reasoning_tokens')}"


@check("model rewrite: response reports upstream id, not the alias (ADR-0004)")
def t_model_rewrite():
    with client() as c:
        r = c.post("/v1/chat/completions", json=chat(
            "north", [{"role": "user", "content": "hi"}],
            extra={"max_tokens": 16, "temperature": 0}))
        assert r.status_code == 200, f"got {r.status_code}"
        m = r.json().get("model", "")
        assert m != "north", "response model is still the alias 'north'"
        return f"model={m!r}"


@check("streaming: SSE chunks + [DONE] (ADR-0007)")
def t_streaming():
    chunks, content, saw_done = 0, [], False
    with client() as c:
        with c.stream("POST", "/v1/chat/completions", json=chat(
                "north", [{"role": "user", "content": "Count: 1 2 3"}],
                stream=True, extra={"max_tokens": 64, "temperature": 0})) as r:
            assert r.status_code == 200, f"got {r.status_code}"
            ctype = r.headers.get("content-type", "")
            assert "text/event-stream" in ctype, f"content-type={ctype!r}"
            for line in r.iter_lines():
                if not line.startswith("data:"):
                    continue
                data = line[len("data:"):].strip()
                if data == "[DONE]":
                    saw_done = True
                    break
                chunks += 1
                try:
                    delta = json.loads(data)["choices"][0]["delta"]
                    if delta.get("content"):
                        content.append(delta["content"])
                except (KeyError, IndexError, json.JSONDecodeError):
                    pass
    assert chunks > 0, "no data chunks received"
    assert saw_done, "no [DONE] terminator"
    return f"{chunks} chunks, content={''.join(content)[:40]!r}"


@check("multimodal: gemma accepts an image part (ADR-0008)")
def t_multimodal():
    img = f"data:image/png;base64,{tiny_png()}"
    with client() as c:
        r = c.post("/v1/chat/completions", json=chat("gemma", [{
            "role": "user",
            "content": [
                {"type": "text", "text": "What color dominates this image? One word."},
                {"type": "image_url", "image_url": {"url": img}},
            ],
        }], extra={"max_tokens": 32, "temperature": 0}))
        assert r.status_code == 200, f"got {r.status_code}: {r.text[:300]}"
        j = r.json()
        content = j["choices"][0]["message"]["content"]
        assert content and content.strip(), "empty content for multimodal request"
        img_tokens = (j.get("usage", {}).get("prompt_tokens_details") or {}).get("image_tokens")
        note = f", image_tokens={img_tokens}" if img_tokens else ""
        return f"answer={content.strip()[:30]!r}{note}"


@check("pareto: 'smart' selects a concrete model (ADR-0013)")
def t_pareto():
    with client() as c:
        r = c.post("/v1/chat/completions", json=chat(
            "smart", [{"role": "user", "content": "Reply with exactly: OK"}],
            extra={"max_tokens": 32, "temperature": 0}))
        assert r.status_code == 200, f"got {r.status_code}: {r.text[:200]}"
        m = r.json().get("model", "")
        assert m in (NORTH_ID, GEMMA_ID), f"unexpected concrete model {m!r}"
        return f"selected={m!r}"


@check("pareto plugins override is accepted and stripped (ADR-0001/0013)")
def t_plugins():
    with client() as c:
        r = c.post("/v1/chat/completions", json=chat(
            "smart", [{"role": "user", "content": "Reply with exactly: OK"}],
            extra={"max_tokens": 32, "temperature": 0,
                   "plugins": [{"id": "pareto", "min_quality": 0.85}]}))
        # If 'plugins' leaked upstream, a strict backend might 400; we expect 200.
        assert r.status_code == 200, f"got {r.status_code}: {r.text[:200]}"
        m = r.json().get("model", "")
        return f"selected={m!r} (min_quality=0.85 favors north)"


@check("error mapping: unknown model -> 404 model_not_found (ADR-0006)")
def t_unknown_model():
    with client() as c:
        r = c.post("/v1/chat/completions", json=chat(
            "no-such-model-xyz", [{"role": "user", "content": "hi"}]))
        assert r.status_code == 404, f"got {r.status_code}: {r.text[:200]}"
        code = (r.json().get("error") or {}).get("code", "")
        assert code == "model_not_found", f"error code={code!r}"
        return "404 model_not_found"


@check("Anthropic consumer: /v1/messages -> openai backend (ADR-0016)")
def t_anthropic_consumer():
    with client() as c:
        # north is a reasoning model: give it headroom + pin temperature so the
        # reasoning phase doesn't consume the whole token budget before the
        # answer (otherwise stop_reason=max_tokens with empty content — a model
        # trait, not a translation bug; verified against gemma at 64 tokens).
        r = c.post("/v1/messages", json={
            "model": "north",
            "max_tokens": 512,
            "temperature": 0,
            "messages": [{"role": "user", "content": "Reply with exactly: OK"}],
        })
        assert r.status_code == 200, f"got {r.status_code}: {r.text[:300]}"
        j = r.json()
        assert j.get("type") == "message", f"not an anthropic message: {list(j)[:6]}"
        blocks = j.get("content", [])
        text = "".join(b.get("text", "") for b in blocks if b.get("type") == "text")
        assert text.strip(), f"no text block in {blocks}"
        return f"content blocks ok, text={text.strip()[:30]!r}"


@check("fusion: 'council' synthesizes an answer (ADR-0014)", slow=True)
def t_fusion():
    with client() as c:
        r = c.post("/v1/chat/completions", json=chat(
            "council", [{"role": "user", "content": "In one sentence, what is a load balancer?"}],
            extra={"max_tokens": 512, "temperature": 0.7}))
        assert r.status_code == 200, f"got {r.status_code}: {r.text[:300]}"
        content = r.json()["choices"][0]["message"]["content"]
        assert content and content.strip(), "empty synthesis"
        return f"synthesis={content.strip()[:60]!r}"


CHECKS = [
    t_healthz, t_readyz, t_models,
    t_chat_north, t_chat_gemma,
    t_reasoning_passthrough, t_model_rewrite,
    t_streaming, t_multimodal,
    t_pareto, t_plugins,
    t_unknown_model, t_anthropic_consumer,
    t_fusion,
]


def main() -> int:
    print(f"\n{DIM}simple-llm-router evals → {ROUTER_URL}{RESET}\n")
    # Fail fast if nothing is listening.
    try:
        httpx.get(f"{ROUTER_URL}/healthz", timeout=3)
    except Exception as e:  # noqa: BLE001
        print(f"{RED}router not reachable at {ROUTER_URL}: {e}{RESET}")
        print("Start it first:  ./bin/router --config config.local.yaml")
        return 1

    for c in CHECKS:
        c()

    passed = sum(1 for r in RESULTS if r.ok and not r.skipped)
    failed = sum(1 for r in RESULTS if not r.ok and not r.skipped)
    skipped = sum(1 for r in RESULTS if r.skipped)
    print(f"\n{'-'*54}")
    color = GREEN if failed == 0 else RED
    print(f"{color}{passed} passed{RESET}, {RED if failed else DIM}{failed} failed{RESET}, "
          f"{DIM}{skipped} skipped{RESET}\n")

    os.makedirs(os.path.join(os.path.dirname(__file__), "report"), exist_ok=True)
    report = {
        "router_url": ROUTER_URL,
        "passed": passed, "failed": failed, "skipped": skipped,
        "results": [r.__dict__ for r in RESULTS],
    }
    with open(os.path.join(os.path.dirname(__file__), "report", "last.json"), "w") as f:
        json.dump(report, f, indent=2)

    return failed


if __name__ == "__main__":
    sys.exit(main())
