// Package forward owns the OTLP/JSON HTTP transport that pushes
// flushed traces to the Incidentary backend.
//
// The Bridge is fundamentally a forwarder: spans flow in via the
// collector's normal pipeline, the buffer + triggers decide WHEN to
// flush, and forward.Client pushes the flushed traces to Incidentary's
// /api/v1/otlp/v1/traces endpoint with the X-Incidentary-Surface:
// processor header so the backend stamps surface=processor.
package forward

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	corecingest "github.com/incidentary/incidentary-go-core/ingest"
	coresurface "github.com/incidentary/incidentary-go-core/surface"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/collector/pdata/ptrace/ptraceotlp"
	"go.uber.org/zap"
)

// Config is the input to NewClient. The Bridge processor builds this
// from its own ForwardConfig at startup.
type Config struct {
	Endpoint     string
	APIKey       string
	AgentVersion string
	Timeout      time.Duration
	Logger       *zap.Logger
	HTTPClient   *http.Client // optional; tests inject
}

// Client wraps an incidentary-go-core ingest.Client with the
// processor-surface header pinned and OTLP/JSON encoding.
type Client struct {
	core   *corecingest.Client
	logger *zap.Logger
}

// NewClient constructs a Client. The X-Incidentary-Surface header is
// always "processor" — that's the entire point of the Bridge.
func NewClient(cfg Config) (*Client, error) {
	if cfg.Endpoint == "" {
		return nil, errors.New("forward: Endpoint is required")
	}
	if cfg.APIKey == "" {
		return nil, errors.New("forward: APIKey is required")
	}
	if cfg.AgentVersion == "" {
		cfg.AgentVersion = "0.1.0"
	}
	if cfg.Logger == nil {
		cfg.Logger = zap.NewNop()
	}
	base, _, err := splitEndpoint(cfg.Endpoint)
	if err != nil {
		return nil, err
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		timeout := cfg.Timeout
		if timeout <= 0 {
			timeout = 30 * time.Second
		}
		httpClient = &http.Client{Timeout: timeout}
	}
	core, err := corecingest.NewClient(corecingest.Config{
		BaseURL:      base,
		APIKey:       cfg.APIKey,
		AgentVersion: cfg.AgentVersion,
		Surface:      coresurface.SurfaceProcessor,
		HTTPClient:   httpClient,
		UserAgent:    fmt.Sprintf("incidentary-bridge/%s", cfg.AgentVersion),
	})
	if err != nil {
		return nil, err
	}
	return &Client{core: core, logger: cfg.Logger}, nil
}

// SendTraces serializes ptrace.Traces as OTLP/JSON and posts it to the
// configured endpoint. Retry, auth header construction, and surface
// header live in incidentary-go-core/ingest.
func (c *Client) SendTraces(ctx context.Context, td ptrace.Traces) error {
	if td.SpanCount() == 0 {
		return nil
	}
	req := ptraceotlp.NewExportRequestFromTraces(td)
	body, err := req.MarshalJSON()
	if err != nil {
		return fmt.Errorf("forward: marshal OTLP/JSON: %w", err)
	}
	if _, err := c.core.PostOTLP(ctx, body); err != nil {
		c.logger.Warn("flush failed",
			zap.Int("span_count", td.SpanCount()),
			zap.Error(err),
		)
		return err
	}
	c.logger.Debug("flushed",
		zap.Int("span_count", td.SpanCount()),
		zap.Int("byte_count", len(body)),
	)
	return nil
}

// splitEndpoint pulls the base URL out of a full endpoint URL. The
// incidentary-go-core ingest.Client wants BaseURL only and appends
// /api/v1/otlp/v1/traces internally.
func splitEndpoint(endpoint string) (base string, path string, err error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", "", fmt.Errorf("forward: parse endpoint: %w", err)
	}
	if u.Scheme == "" || u.Host == "" {
		return "", "", fmt.Errorf("forward: endpoint must be absolute (got %q)", endpoint)
	}
	// u.Host already includes the port when one was present in the URL.
	base = fmt.Sprintf("%s://%s", u.Scheme, u.Host)
	path = u.Path
	return base, path, nil
}
