# deploy/ — Kubernetes manifests (GitOps via Flux)

These manifests run simple-llm-router on the **homelab** k3s cluster. They are
applied by Flux, which is configured in the separate
[`hellauseful`](https://github.com/mattbucci/hellauseful) GitOps repo
(`clusters/homelab/simple-llm-router-{source,kustomization}.yaml`). Flux watches
this `./deploy` path and pins the image tag automatically.

## How a change ships

1. Push to `main` → the `Docker` workflow builds and pushes
   `ghcr.io/mattbucci/simple-llm-router:main-<sha>-<ts>`.
2. Flux's `ImagePolicy` (in hellauseful) selects the newest tag and writes it
   into the homelab `Kustomization`.
3. Flux applies the updated Deployment to the cluster.

## One-time cluster prerequisites

Both are applied out-of-band (not committed) so real fleet addresses and
registry creds stay out of the repo.

**1. GHCR pull secret** (per flux.md, in every namespace this app uses):

```sh
kubectl create secret docker-registry github-registry-auth \
  --namespace=default \
  --docker-server=ghcr.io \
  --docker-username=<github-user> \
  --docker-password=<ghcr-token>
```

**2. Router config Secret** — holds the real backends/aliases. Build it from your
local config (e.g. the gitignored `config.local.yaml`):

```sh
kubectl -n default create secret generic simple-llm-router-config \
  --from-file=config.yaml=./config.local.yaml \
  --dry-run=client -o yaml | kubectl apply -f -
```

The committed [`config.example.yaml`](./config.example.yaml) documents the
expected shape; the running pod reads only the Secret.

**3. Audio gateway token** (only if the `audio:` block is configured, ADR-0022).
The config references it as `${VOICE_API_TOKEN}`, which the router interpolates from
the process env at startup; `deployment.yaml` injects it from this Secret (the key
is `optional`, so it is only needed when audio is in use):

```sh
kubectl -n default create secret generic simple-llm-router-secrets \
  --from-literal=voice-api-token=<gateway-token> \
  --dry-run=client -o yaml | kubectl apply -f -
```

## Files

| file | purpose |
|------|---------|
| `deployment.yaml` | router Deployment — probes, non-root, config from Secret |
| `service.yaml` | ClusterIP `simple-llm-router.default.svc` :80 → :8080 |
| `kustomization.yaml` | kustomize entrypoint Flux applies |
| `config.example.yaml` | placeholder config documenting the Secret's shape |
