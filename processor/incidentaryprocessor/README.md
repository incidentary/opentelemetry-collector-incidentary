# `incidentaryprocessor`

An OpenTelemetry collector processor that adds an unsampled in-pipeline
pre-alert buffer plus the four-trigger Incidentary alert evaluator.

This processor follows the
[`opentelemetry-collector-contrib` component conventions](https://github.com/open-telemetry/opentelemetry-collector-contrib/blob/main/CONTRIBUTING.md)
so an eventual contrib donation is mechanical.

## Configuration

```yaml
processors:
  incidentary:
    pre_alert_window: 5m
    max_buffer_size: 50000
    triggers:
      5xx_error_rate:
        high: 0.10
        low: 0.02
      slow_success_rate:
        high: 0.20
        mild: 0.10
      retry_rate:
        high: 0.10
        mild: 0.05
      in_flight_pileup:
        threshold: <implementation parity with 1.0.0 SDKs>
    forward:
      endpoint: https://api.incidentary.com/api/v1/otlp/v1/traces
      token: ${INCIDENTARY_API_KEY}
      pass_through: true
    routing:
      statements:
        # OTTL routing — optional, additive
        - >-
          route() where attributes["service.name"] == "checkout"
    dlq:
      # Optional file/redis-backed DLQ via the collector storage extension.
      storage_id: file_storage/incidentary
```

## Status table (per OTel contrib conventions)

| Status                   |                                |
| ------------------------ | ------------------------------ |
| Stability                | alpha                          |
| Supported pipeline types | traces                         |
| Distributions            | core, contrib (eventual)       |

## See also

- Repo README: `../../README.md`
