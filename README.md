# Incidentary OpenTelemetry Collector

The Incidentary Bridge — published OpenTelemetry collector
distribution and processor that adds an unsampled in-pipeline
pre-alert buffer plus the four-trigger Incidentary alert evaluator,
without requiring any application-code changes.

The same Go module produces two deployment shapes from one binary:

1. **Collector processor.** Drops into your existing OpenTelemetry
   collector pipeline as `incidentaryprocessor`. Standard contrib-
   component layout.
2. **Standalone sidecar.** A `cmd/incidentaryprocessor/main.go` binary
   runs the OTel collector framework with the processor pre-loaded.
   Distributed as a Docker image and a Helm chart.

## Why this exists

OpenTelemetry won the standardization war. A product that demands a
*separate* SDK in 2026 fights a losing battle against the
install-friction tax. A product that *uses* OTel as its substrate
rides the wave.

So Incidentary gives away the foundation (OTel ingestion via the
[Exporter surface](https://incidentary.com/otel-exporter)) and charges
for the operational guarantees the protocol alone doesn't deliver.
The Bridge is the OTel-native middle tier: customers who already run
a collector or want pre-alert capture without per-language SDK work
add the Bridge to their pipeline and get the same fidelity outcomes
the [Full SDK surface](https://incidentary.com/sdk) delivers
in-process.

The four 1.0.0 SDKs (`incidentary-sdk-{node,python,go,dotnet}`) are
preserved unchanged. The Bridge is not a replacement for them — it's
a different deployment shape for the same fidelity contract.

## What it does

- **Pre-alert circular buffer** — every span entering the pipeline
  goes into a circular buffer keyed by trace_id. Default time
  eviction at 5 minutes; hard count cap at 50,000 spans.
- **Four-trigger evaluator** — port of the same per-service alert
  logic from the existing 1.0.0 SDKs (5xx error rate, slow success
  rate, retry rate, in-flight pile-up). The constants live in the
  shared [`incidentary-wire-format`](https://github.com/incidentary/wire-format)
  spec.
- **Cross-batch trace circuit breaker** — cumulative per-trace span
  counter that catches runaway traces split across many small
  batches. Three tiers (5K warn / 50K truncate / 500K hard breaker).
- **Dead-letter queue with optional disk persistence** — failed
  forwards spool to memory by default; set `dlq.storage_id` to a
  collector storage extension (`file_storage`, `redis_storage`, etc.)
  for restart-durable buffering.
- **OTTL routing** — optional escalation rules that trigger flush on
  customer-defined conditions in addition to the four built-in
  triggers.
- **V2 wire-format encoder** — encodes flushed spans as Incidentary V2
  CEs and forwards to the Incidentary backend via the standard
  OTLP/HTTP transport.
- **Pass-through** — by default, all spans continue down the
  customer's existing exporter chain unaltered (collector mode only).
  The Bridge is observation, not interception.

## What it does NOT do

- **No webhook listener.** Backend-fired flush is deferred to a
  future minor release. v0.1.0 evaluates triggers locally only.
- **No SDK fork.** This is not a parallel-universe OTel SDK. It's a
  processor that sits in the OTel pipeline as designed by the OTel
  community.
- **No backend functionality.** All assembly, storage, and incident
  artefacts happen at the Incidentary backend. The Bridge is an
  in-pipeline component.

## Deployment shapes

### Shape 1 — Collector processor (recommended if you already run a collector)

```yaml
processors:
  incidentary:
    pre_alert_window: 5m
    max_buffer_size: 50000
    triggers:
      5xx_error_rate:
        high: 0.10
        low: 0.02
    forward:
      endpoint: https://api.incidentary.com/api/v1/otlp/v1/traces
      token: ${INCIDENTARY_API_KEY}
```

Add `incidentary` to your `service.pipelines.traces.processors` list.
Pass-through is on by default; your existing exporter chain is
unaffected.

### Shape 2 — Standalone sidecar (recommended if you don't have a collector)

```bash
helm install incidentary-bridge \
  oci://ghcr.io/incidentary/opentelemetry-collector-incidentary/charts/incidentary-bridge \
  --version 0.1.0 \
  --namespace incidentary-system --create-namespace \
  --set apiKey=sk_live_<your-key>
```

Then configure your application to send OTLP to
`incidentary-bridge:4317` (gRPC) or `:4318` (HTTP).

## Supply-chain verification

Every release publishes:

- The container image at
  `ghcr.io/incidentary/opentelemetry-collector-incidentary:<tag>`,
  scanned by Trivy on HIGH/CRITICAL fixable CVEs and signed with
  cosign keyless via GitHub's OIDC.
- The Helm chart at
  `ghcr.io/incidentary/opentelemetry-collector-incidentary/charts/incidentary-bridge:<chart-version>`,
  cosign-signed under the same identity.
- A CycloneDX SBOM attached to the GitHub Release.

Verify before installing:

```bash
cosign verify ghcr.io/incidentary/opentelemetry-collector-incidentary:v0.1.0 \
  --certificate-identity-regexp "https://github.com/incidentary/.+" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com

cosign verify ghcr.io/incidentary/opentelemetry-collector-incidentary/charts/incidentary-bridge:0.1.0 \
  --certificate-identity-regexp "https://github.com/incidentary/.+" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

## Repository layout

```
.
├── README.md, LICENSE (Apache 2.0)
├── go.mod, go.sum
├── processor/incidentaryprocessor/
│   ├── factory.go, processor.go, config.go
│   ├── metadata.yaml
│   ├── README.md
│   └── testdata/
├── cmd/incidentaryprocessor/
│   ├── main.go         # standalone sidecar entrypoint
│   └── builder.yaml    # OCB manifest
├── internal/
│   ├── buffer/         # ring buffer + time-eviction
│   ├── triggers/       # four-trigger evaluator
│   └── forward/        # V2 wire format forwarder
├── charts/incidentary-bridge/   # Helm chart (sidecar deployment)
├── Dockerfile
└── .github/workflows/
```

This layout deliberately matches the
[`opentelemetry-collector-contrib` processor convention](https://github.com/open-telemetry/opentelemetry-collector-contrib/tree/main/processor)
so an eventual contrib donation is mechanical.

## License

Apache 2.0. See [LICENSE](./LICENSE).

## Contributing

See [CONTRIBUTING.md](./CONTRIBUTING.md). Issues and PRs welcome
once you've signed the DCO; the process matches
`opentelemetry-collector-contrib`.

## Maintainers

- Ahmed Mostafa
