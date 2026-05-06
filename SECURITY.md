# Security policy

This file documents the security posture of
`incidentary/opentelemetry-collector-incidentary` (the Bridge) and the
process for reporting vulnerabilities.

## Reporting a vulnerability

**Email:** `security@incidentary.com` (PGP key on request).

Please include:

- A description of the vulnerability and the conditions under which it
  triggers.
- The minimum Bridge version + OTel collector version + Helm chart values
  you observed it on.
- Any proof-of-concept code, exploit script, or reproduction steps. We
  prefer minimal repros that don't require running a full production
  cluster.

We acknowledge every report within 3 business days. We do not run a
public bug bounty; valid reports are credited in the relevant release
note unless the reporter requests otherwise.

**Do not** open a public GitHub issue for a security report.

## Response policy

| Severity | Patch target | Notes |
|---|---|---|
| **CRITICAL** | within 7 days | CVSS 9.0+ in the Bridge image or its own code. |
| **HIGH** | within 14 days | CVSS 7.0–8.9. The release workflow's Trivy gate enforces this for image CVEs (see [.github/workflows/release.yml](./.github/workflows/release.yml)). |
| **MEDIUM** | next minor release | CVSS 4.0–6.9. |
| **LOW** | next major release | CVSS < 4.0. |

The Trivy CVE gate in the release workflow blocks publication of any new
image tag whose Trivy scan reports HIGH or CRITICAL severity issues for
fixed packages (`ignore-unfixed: true` — we don't block on a CVE the
upstream cannot yet patch).

## Threat model

### What the Bridge can do

The Bridge runs as a sidecar (Kubernetes pod) or as an in-pipeline
collector processor. It has the following authority:

- **Inbound:** OTLP gRPC (`:4317`) and OTLP HTTP (`:4318`) on all
  interfaces inside the pod. The Helm chart's NetworkPolicy default
  restricts ingress to pods in the same namespace.
- **In-memory state:** a circular buffer of completed traces sized to
  `MaxBufferSize` (default 50K) with time-eviction at `PreAlertWindow`
  (default 5m).
- **Outbound HTTPS** to the configured Incidentary backend (default
  `api.incidentary.com:443`) carrying flushed batches of OTel spans
  signed with a per-workspace bearer token from `INCIDENTARY_API_KEY`.

The Bridge **cannot**:

- Read Kubernetes resources. The chart's ServiceAccount has no role
  bindings and `automountServiceAccountToken: false`.
- Write any Kubernetes resource.
- Read from or write to the host filesystem (the chart sets
  `readOnlyRootFilesystem: true` and mounts `/tmp` as a small in-memory
  emptyDir).
- Open inbound connections except `:4317`, `:4318`, the metrics port,
  and the health endpoint.
- Execute commands inside cluster pods.

### What the Bridge must protect

- The API key (a workspace-bearer token) in the referenced Secret. The
  Bridge reads it via env var on startup and on Secret rotation;
  it never persists it outside Kubernetes.
- The integrity of inbound spans. The Bridge does not authenticate OTLP
  receivers — applications inside the same namespace trust the cluster.
  Cross-namespace ingress is blocked by the NetworkPolicy.
- The integrity of outbound batches. Transport security is plain TLS
  with the workspace bearer token; mTLS is on the roadmap.

### Trust boundaries

| Boundary | Crossed by | Authentication |
|---|---|---|
| Application pod ↔ Bridge | every span batch | None at the Bridge; cluster network is the trust anchor (NetworkPolicy enforces same-namespace) |
| Bridge ↔ Incidentary backend | every flush, every pass-through batch | Bearer token from the API-key Secret |
| Prometheus scraper ↔ Bridge metrics | scrape | Plain HTTP by default; restricted by NetworkPolicy when `allowPrometheusScrape=true` |

### What the Bridge does NOT trust

- The OTel attribute set on inbound spans. The processor enforces a
  hard cap of 50,000 spans per trace (workspace circuit breaker at
  500K spans / 60s) regardless of what the application emits.
- The configured backend's response: 4xx other than 429 are treated as
  permanent (no retry); 429 / 5xx / network errors retry with
  exponential backoff to a 60-second window.

## Supply chain

- All container images are built reproducibly from the Dockerfile in
  this repo via Docker BuildKit.
- All images are pushed to
  `ghcr.io/incidentary/opentelemetry-collector-incidentary` and signed
  with cosign keyless via GitHub OIDC at release time.
- The Helm chart is signed with cosign at the same time as the image.
- An SBOM (CycloneDX JSON) is attached to every GitHub release.
- Dependencies are tracked in `go.mod` / `go.sum` and audited by
  Dependabot weekly.

Verify a published image:

```bash
cosign verify ghcr.io/incidentary/opentelemetry-collector-incidentary:v0.1.0 \
  --certificate-identity-regexp 'https://github.com/incidentary/opentelemetry-collector-incidentary/.+' \
  --certificate-oidc-issuer 'https://token.actions.githubusercontent.com'
```

Verify a published chart:

```bash
cosign verify ghcr.io/incidentary/opentelemetry-collector-incidentary/charts/incidentary-bridge:0.1.0 \
  --certificate-identity-regexp 'https://github.com/incidentary/opentelemetry-collector-incidentary/.+' \
  --certificate-oidc-issuer 'https://token.actions.githubusercontent.com'
```

## Network policy

The chart ships a NetworkPolicy template, **on by default**. When
enforced (cluster running a NetworkPolicy-aware CNI), the Bridge pod is
restricted to:

- **Egress:** DNS (CoreDNS), TCP 443 to the Incidentary backend.
  Tighten with `networkPolicy.egressTo` to a specific CIDR.
- **Ingress:** OTLP (4317 / 4318) from pods in the same namespace.
  Optionally Prometheus scrape on the metrics port when
  `networkPolicy.allowPrometheusScrape: true`.

The NetworkPolicy is only enforced on clusters running a
NetworkPolicy-aware CNI (Calico, Cilium, Antrea, etc.). On other CNIs
the policy is silently ignored.

## Security contact

Email: `security@incidentary.com`.
