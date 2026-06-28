# ADR-0010: Configuration & validation

- **Status:** Accepted
- **Date:** 2026-06-28
- **Deciders:** Matthew Bucci

## Context

The router needs operators to declare backends, friendly aliases, routing
strategies, health probing, timeouts, and inbound auth. This configuration is
written and reviewed by humans: it needs comments, is diffed in version control,
and is occasionally hand-edited under pressure. It also references secrets
(provider credentials, inbound tokens) that must never be committed.

Two failure modes must be designed out:

- **Silent misconfiguration** — an alias pointing at a non-existent backend, a
  malformed URL — surfacing only as confusing runtime 404s/502s.
- **Secret leakage** — credentials baked into a file that lands in git.

## Decision

A single **YAML** config file, passed via `--config`, loaded **once at startup**
and **fully validated before the server begins listening**. Validation failures
abort the process with a clear, specific error — fail fast, never serve a
half-valid config.

```mermaid
flowchart TD
    F[--config path] --> L[read file]
    L --> E[expand ${ENV} refs]
    E --> P[parse YAML]
    P --> V{validate}
    V -->|error| X[print error, exit non-zero]
    V -->|ok| W[wire backends, aliases, health loop]
    W --> S[start listening]
```

### Schema

| Key | Meaning |
|-----|---------|
| `listen` | Address the HTTP server binds (e.g. `":8080"`). |
| `backends[]` | `name`, `base_url`, `protocol` (`openai`\|`anthropic`, default `openai`, see [ADR-0016](0016-multi-protocol.md)), optional `credentials` (env-interpolated). |
| `aliases{}` | Per alias: `type` (`proxy`\|`fusion`, default `proxy`), `model` (upstream id), `backends[]`, `selector` (`round_robin`\|`pareto`), `quality` scores, and fusion `panel`/`judge` params. See [ADR-0004](0004-model-aliasing.md), [ADR-0006](0006-routing-and-failover.md), [ADR-0013](0013-pareto-routing.md), [ADR-0014](0014-fusion-routing.md). |
| `health` | `interval`, `timeout` ([ADR-0005](0005-backend-discovery-and-health.md)). |
| `timeouts` | `connect`, `request`, `idle`. |
| `max_body_size` | Inbound body cap, sized for multimodal payloads ([ADR-0008](0008-multimodal-and-large-bodies.md)). |
| `auth.tokens[]` | Accepted inbound bearer tokens ([ADR-0009](0009-authentication.md)). |

### Secrets via environment interpolation

Any value may reference an environment variable as `${VAR}`; references are
expanded after read, before parse. Secrets (provider credentials, inbound
tokens) **MUST** be supplied this way — the committed config holds `${VAR}`
placeholders, never literal secrets. `config.example.yaml` is the documented,
secret-free template.

### Validation rules

- Every `alias.backends` entry **must** name a defined backend.
- Every `base_url` **must** be a valid absolute `http(s)` URL.
- Backend `name`s **must** be unique.
- A referenced upstream `model` need **not** be live at startup — a down backend
  may recover ([ADR-0004](0004-model-aliasing.md), [ADR-0005](0005-backend-discovery-and-health.md)).
- `type`/`selector`/`protocol` **must** be one of their allowed enum values.
- **Fail closed on auth** ([ADR-0009](0009-authentication.md)): an absent or
  empty `auth.tokens` means "trust the LAN", but a present entry that resolved to
  empty almost certainly came from an unset `${ENV}` placeholder — silently
  dropping it would turn an intended require-a-key config into an open router, so
  the loader **must** reject it. (The YAML decoder preserves such empty entries
  rather than discarding them, so validation can see them.)
- Every `protocol: anthropic` backend **must** supply both `credentials.api_key`
  and `credentials.anthropic_version` ([ADR-0009](0009-authentication.md)):
  without them the outbound injection is incomplete and every call/probe 5xxes at
  runtime, so startup fails with a clear message instead.

### Dependency exception: YAML

YAML has no stdlib parser. We accept **`gopkg.in/yaml.v3`** as a sanctioned
carve-out from the stdlib-only rule, justified by the need for human-friendly,
commentable operator config (JSON cannot carry comments). The general rule that
any further dependency requires its own justification plus an allowlist update
is canonical in [ADR-0015](0015-code-style.md).

### No hot reload (v1)

Config is applied at startup only; to change it, restart the process (it is
stateless and restarts cleanly — [ADR-0006](0006-routing-and-failover.md)).
A `SIGHUP`-driven reload is a plausible future decision but is explicitly out of
scope for v1 to avoid concurrent-state complexity.

## Consequences

**Positive**
- Misconfiguration is caught before traffic flows; errors are precise.
- Secrets stay out of git; the example file is safe to publish.
- One small, justified dependency; everything else stdlib.

**Negative / trade-offs**
- One YAML dependency, consciously accepted.
- Config changes require a restart until a reload decision is made.

## Compliance

- **MUST** load exactly one YAML config from `--config` at startup.
- **MUST** validate fully and **exit non-zero** with a clear message on any
  validation error, before listening.
- **MUST** reject configs where an alias references an undefined backend, a
  `base_url` is not a valid `http(s)` URL, backend names collide, or an enum
  field has an illegal value.
- **MUST NOT** require referenced models to be live at startup.
- **MUST** support `${ENV}` interpolation and **MUST NOT** contain literal
  secrets in committed config; secrets come from the environment.
- **MUST** reject a config whose `auth.tokens` contains an entry that resolved to
  empty (an unset `${ENV}` placeholder would otherwise silently disable auth =
  open router), failing closed ([ADR-0009](0009-authentication.md)).
- **MUST** require both `credentials.api_key` and `credentials.anthropic_version`
  for every `protocol: anthropic` backend, failing startup with a clear message
  ([ADR-0009](0009-authentication.md)).
- **MUST** treat `gopkg.in/yaml.v3` as the sole sanctioned non-stdlib config
  dependency; new dependencies are governed by [ADR-0015](0015-code-style.md).
- **MAY** add `SIGHUP` reload later via a new ADR; v1 applies config at startup
  only.
