package incidentaryprocessor

import (
	"context"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/processor"
)

// typeStr is the on-disk component type. The collector's config loader
// matches `processors.<typeStr>` against this string.
const typeStr = "incidentary"

// stability is alpha for v0.1.0 per metadata.yaml. Will graduate after
// dogfood (plan §3.7.4 → 3.8.1).
var stability = component.StabilityLevelAlpha

// NewFactory returns the standard processor.Factory for incidentary.
func NewFactory() processor.Factory {
	return processor.NewFactory(
		component.MustNewType(typeStr),
		func() component.Config { return DefaultConfig() },
		processor.WithTraces(createTracesProcessor, stability),
	)
}

func createTracesProcessor(
	ctx context.Context,
	set processor.Settings,
	cfg component.Config,
	next consumer.Traces,
) (processor.Traces, error) {
	pCfg := cfg.(*Config)
	return newTracesProcessor(ctx, set, pCfg, next)
}
