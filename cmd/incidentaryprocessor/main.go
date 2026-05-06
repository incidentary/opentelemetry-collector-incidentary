// Standalone sidecar entrypoint. Runs the OTel collector framework
// with a minimal pre-loaded set: OTLP receiver on
// 0.0.0.0:4317 (gRPC) and 0.0.0.0:4318 (HTTP), the Incidentary
// processor in pass_through:false mode, and the OTLP/HTTP exporter as
// the OPTIONAL pass-through downstream (default disabled).
//
// In production, build via OCB using cmd/incidentaryprocessor/builder.yaml
// — that produces a fully-featured collector with all standard receivers
// and exporters available. This main.go is the minimum-viable entrypoint
// used by the Helm chart's distroless container, and provides a stable
// `--version` / `--help` surface for the Dockerfile CMD.
package main

import (
	"fmt"
	"log"
	"os"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/exporter/otlphttpexporter"
	"go.opentelemetry.io/collector/otelcol"
	"go.opentelemetry.io/collector/receiver/otlpreceiver"

	"github.com/incidentary/opentelemetry-collector-incidentary/processor/incidentaryprocessor"
)

const (
	bridgeName    = "incidentary-bridge"
	bridgeVersion = "0.1.0"
)

func main() {
	factories, err := buildFactories()
	if err != nil {
		log.Fatalf("incidentary-bridge: build factories: %v", err)
	}

	info := component.BuildInfo{
		Command:     bridgeName,
		Description: "Incidentary Bridge — standalone OTel collector sidecar with the incidentaryprocessor pre-loaded.",
		Version:     bridgeVersion,
	}

	settings := otelcol.CollectorSettings{
		BuildInfo: info,
		Factories: func() (otelcol.Factories, error) { return factories, nil },
	}

	cmd := otelcol.NewCommand(settings)
	if err := cmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "incidentary-bridge: %v\n", err)
		os.Exit(1)
	}
}

func buildFactories() (otelcol.Factories, error) {
	receivers, err := otelcol.MakeFactoryMap(
		otlpreceiver.NewFactory(),
	)
	if err != nil {
		return otelcol.Factories{}, err
	}
	processors, err := otelcol.MakeFactoryMap(
		incidentaryprocessor.NewFactory(),
	)
	if err != nil {
		return otelcol.Factories{}, err
	}
	exporters, err := otelcol.MakeFactoryMap(
		otlphttpexporter.NewFactory(),
	)
	if err != nil {
		return otelcol.Factories{}, err
	}
	return otelcol.Factories{
		Receivers:  receivers,
		Processors: processors,
		Exporters:  exporters,
	}, nil
}
