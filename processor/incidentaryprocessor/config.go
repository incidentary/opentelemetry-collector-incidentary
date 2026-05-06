// Package incidentaryprocessor implements the Incidentary Bridge.
//
// The processor maintains a per-trace_id pre-alert ring buffer and
// flushes a trace to Incidentary when one of the four built-in alert
// triggers (or an optional OTTL escalation rule) fires.
package incidentaryprocessor

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"time"
)

// Config is the YAML-mappable configuration for the Bridge processor.
//
// Trigger thresholds are pinned to the four 1.0.0 SDKs' constants so
// the trigger-parity test holds without per-trigger tuning.
type Config struct {
	// Buffering
	PreAlertWindow time.Duration `mapstructure:"pre_alert_window"`
	MaxBufferSize  int           `mapstructure:"max_buffer_size"`

	// Triggers (the four-trigger evaluator, ported from the SDK)
	Triggers TriggersConfig `mapstructure:"triggers"`

	// OTTL escalation rules (additive — fires flush in addition to
	// the four triggers when the OTTL statement matches).
	ExtraFlushConditions []OTTLCondition `mapstructure:"extra_flush_conditions"`

	// Forwarding
	Forward ForwardConfig `mapstructure:"forward"`

	// Routing
	Routing RoutingConfig `mapstructure:"routing"`

	// DLQ controls optional restart durability for the in-memory
	// dead-letter queue. When set, the processor saves the queue
	// contents to disk on Shutdown and reloads on Start, so an
	// operator restart during a backend outage no longer loses the
	// queued retries. Empty = in-memory only.
	DLQ DLQConfig `mapstructure:"dlq"`
}

// DLQConfig controls dead-letter queue persistence.
type DLQConfig struct {
	// PersistPath is an absolute (or operator-controlled relative)
	// file path where the in-memory DLQ is serialized on Shutdown
	// and read back on Start. The serialization format is
	// length-delimited OTLP/protobuf — operator-readable via any
	// OTel pipeline tooling. Empty disables persistence.
	PersistPath string `mapstructure:"persist_path"`
}

// TriggersConfig groups the four built-in trigger thresholds.
type TriggersConfig struct {
	ErrorRate5xx    ErrorRate5xxConfig    `mapstructure:"5xx_error_rate"`
	SlowSuccessRate SlowSuccessRateConfig `mapstructure:"slow_success_rate"`
	RetryRate       RetryRateConfig       `mapstructure:"retry_rate"`
	InFlightPileup  InFlightPileupConfig  `mapstructure:"in_flight_pileup"`
}

// ErrorRate5xxConfig controls the 5xx_error_rate trigger.
// Defaults: high 0.10, low 0.02.
type ErrorRate5xxConfig struct {
	High float64 `mapstructure:"high"`
	Low  float64 `mapstructure:"low"`
}

// SlowSuccessRateConfig controls the slow_success_rate trigger.
// Defaults: high 0.20, mild 0.10.
type SlowSuccessRateConfig struct {
	High float64 `mapstructure:"high"`
	Mild float64 `mapstructure:"mild"`
}

// RetryRateConfig controls the retry_rate trigger.
// Defaults: high 0.10, mild 0.05.
type RetryRateConfig struct {
	High float64 `mapstructure:"high"`
	Mild float64 `mapstructure:"mild"`
}

// InFlightPileupConfig controls the in_flight_pileup trigger. The
// defaults match the SDK 1.0.0 RetryConfig and InFlightConfig
// constants verbatim — see `incidentary-sdk-go` for source of truth.
type InFlightPileupConfig struct {
	MinAbsoluteInFlight int64   `mapstructure:"min_absolute_in_flight"`
	BaselineMultiplier  float64 `mapstructure:"baseline_multiplier"`
	NetGrowthMin        int64   `mapstructure:"net_growth_min"`
	SevereHoldSecs      int64   `mapstructure:"severe_hold_secs"`
	MildHoldSecs        int64   `mapstructure:"mild_hold_secs"`
}

// OTTLCondition lets a customer flush a trace when a custom
// expression matches a span. This is additive: the four built-in
// triggers still fire independently.
type OTTLCondition struct {
	Statement string `mapstructure:"statement"`
}

// ForwardConfig describes how flushed traces reach Incidentary.
type ForwardConfig struct {
	IncidentaryEndpoint string        `mapstructure:"endpoint"`
	IncidentaryToken    string        `mapstructure:"token"`
	PassThrough         bool          `mapstructure:"pass_through"`
	Timeout             time.Duration `mapstructure:"timeout"`

	// AllowInsecure opts out of the SSRF guards on `endpoint`. By
	// default (false), Validate rejects non-HTTPS schemes and any
	// host that resolves to a private/loopback/link-local address —
	// the customer-controlled endpoint must not be allowed to point
	// at AWS IMDS (169.254.169.254), the kube apiserver, or any
	// RFC1918 internal target. Self-hosted deployments behind a
	// trusted internal collector can opt in by setting this flag.
	AllowInsecure bool `mapstructure:"allow_insecure"`
}

// RoutingConfig optionally restricts which traces enter the buffer.
type RoutingConfig struct {
	Statements []string `mapstructure:"statements"`
}

const (
	// DefaultPreAlertWindow is the time window within which a span
	// remains in the pre-alert buffer.
	DefaultPreAlertWindow = 5 * time.Minute
	// DefaultMaxBufferSize is the hard cap on spans-per-trace held
	// in the pre-alert buffer.
	DefaultMaxBufferSize = 50_000
	// DefaultForwardEndpoint is the public Incidentary OTLP HTTP
	// trace endpoint.
	DefaultForwardEndpoint = "https://api.incidentary.com/api/v1/otlp/v1/traces"
	// DefaultForwardTimeout — conservative; OTLP HTTP forwarder.
	DefaultForwardTimeout = 30 * time.Second
)

// DefaultConfig returns the on-disk default for the Bridge.
//
// Trigger constants are pinned to the four 1.0.0 SDKs verbatim. The
// trigger-parity test depends on these defaults staying in sync with
// the SDK's prearm-defaults spec.
func DefaultConfig() *Config {
	return &Config{
		PreAlertWindow: DefaultPreAlertWindow,
		MaxBufferSize:  DefaultMaxBufferSize,
		Triggers: TriggersConfig{
			ErrorRate5xx: ErrorRate5xxConfig{
				High: 0.10,
				Low:  0.02,
			},
			SlowSuccessRate: SlowSuccessRateConfig{
				High: 0.20,
				Mild: 0.10,
			},
			RetryRate: RetryRateConfig{
				High: 0.10,
				Mild: 0.05,
			},
			InFlightPileup: InFlightPileupConfig{
				MinAbsoluteInFlight: 16,
				BaselineMultiplier:  3.0,
				NetGrowthMin:        8,
				SevereHoldSecs:      6,
				MildHoldSecs:        3,
			},
		},
		Forward: ForwardConfig{
			IncidentaryEndpoint: DefaultForwardEndpoint,
			PassThrough:         true,
			Timeout:             DefaultForwardTimeout,
		},
	}
}

// Validate rejects malformed configuration before the processor starts.
func (c *Config) Validate() error {
	if c.PreAlertWindow <= 0 {
		return errors.New("pre_alert_window must be > 0")
	}
	if c.MaxBufferSize <= 0 {
		return errors.New("max_buffer_size must be > 0")
	}
	if c.Forward.IncidentaryEndpoint == "" {
		return errors.New("forward.endpoint must be set")
	}
	if err := validateForwardEndpoint(c.Forward.IncidentaryEndpoint, c.Forward.AllowInsecure); err != nil {
		return fmt.Errorf("forward.endpoint: %w", err)
	}
	if c.Forward.Timeout < 0 {
		return errors.New("forward.timeout must be >= 0")
	}
	if err := c.Triggers.ErrorRate5xx.validate(); err != nil {
		return fmt.Errorf("triggers.5xx_error_rate: %w", err)
	}
	if err := c.Triggers.SlowSuccessRate.validate(); err != nil {
		return fmt.Errorf("triggers.slow_success_rate: %w", err)
	}
	if err := c.Triggers.RetryRate.validate(); err != nil {
		return fmt.Errorf("triggers.retry_rate: %w", err)
	}
	if err := c.Triggers.InFlightPileup.validate(); err != nil {
		return fmt.Errorf("triggers.in_flight_pileup: %w", err)
	}
	return nil
}

func (e ErrorRate5xxConfig) validate() error {
	if e.High <= 0 || e.High > 1 {
		return fmt.Errorf("high (%.4f) must be in (0,1]", e.High)
	}
	if e.Low <= 0 || e.Low > e.High {
		return fmt.Errorf("low (%.4f) must be in (0,high]", e.Low)
	}
	return nil
}

func (s SlowSuccessRateConfig) validate() error {
	if s.High <= 0 || s.High > 1 {
		return fmt.Errorf("high (%.4f) must be in (0,1]", s.High)
	}
	if s.Mild <= 0 || s.Mild > s.High {
		return fmt.Errorf("mild (%.4f) must be in (0,high]", s.Mild)
	}
	return nil
}

func (r RetryRateConfig) validate() error {
	if r.High <= 0 || r.High > 1 {
		return fmt.Errorf("high (%.4f) must be in (0,1]", r.High)
	}
	if r.Mild <= 0 || r.Mild > r.High {
		return fmt.Errorf("mild (%.4f) must be in (0,high]", r.Mild)
	}
	return nil
}

func (i InFlightPileupConfig) validate() error {
	if i.MinAbsoluteInFlight <= 0 {
		return errors.New("min_absolute_in_flight must be > 0")
	}
	if i.BaselineMultiplier <= 0 {
		return errors.New("baseline_multiplier must be > 0")
	}
	if i.NetGrowthMin < 0 {
		return errors.New("net_growth_min must be >= 0")
	}
	if i.SevereHoldSecs < i.MildHoldSecs {
		return errors.New("severe_hold_secs must be >= mild_hold_secs")
	}
	return nil
}

// validateForwardEndpoint enforces the SSRF guards on
// `forward.endpoint`. The customer controls this URL via collector
// YAML / Helm values; without these checks a hostile config can
// exfiltrate the bearer token to AWS IMDS, the kube apiserver, or
// any other internal target. `allowInsecure` opts out for
// self-hosted deployments that genuinely need a private address.
func validateForwardEndpoint(raw string, allowInsecure bool) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("not a valid URL: %w", err)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return fmt.Errorf("unsupported scheme %q (want https)", u.Scheme)
	}
	if u.Scheme == "http" && !allowInsecure {
		return fmt.Errorf("scheme %q requires allow_insecure=true (default forbids non-HTTPS to prevent credential leakage)", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return errors.New("URL must include a host")
	}
	if !allowInsecure {
		if reason, blocked := isBlockedHost(host); blocked {
			return fmt.Errorf("host %q is %s — set allow_insecure=true to permit", host, reason)
		}
	}
	return nil
}

// isBlockedHost reports whether `host` is an SSRF-bait address. The
// caller has already decided whether to honour the `allow_insecure`
// escape hatch.
func isBlockedHost(host string) (string, bool) {
	// Direct IP literal.
	if ip := net.ParseIP(host); ip != nil {
		return blockedIPReason(ip)
	}
	// `localhost` resolves to loopback addresses on virtually every
	// system; flag it explicitly so the error message stays
	// actionable even when DNS lookup is offline.
	if host == "localhost" {
		return "loopback (localhost)", true
	}
	addrs, err := net.LookupHost(host)
	if err != nil {
		// DNS unreachable at config-time — defer to runtime checks
		// rather than blocking a legitimate hostname that happens
		// to be unresolvable on the validating machine.
		return "", false
	}
	for _, addr := range addrs {
		if ip := net.ParseIP(addr); ip != nil {
			if reason, blocked := blockedIPReason(ip); blocked {
				return reason, true
			}
		}
	}
	return "", false
}

func blockedIPReason(ip net.IP) (string, bool) {
	switch {
	case ip.IsLoopback():
		return "a loopback address", true
	case ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast():
		return "a link-local address", true
	case ip.IsPrivate():
		return "a private (RFC1918/ULA) address", true
	case ip.IsUnspecified():
		return "the unspecified address", true
	case ip.IsMulticast():
		return "a multicast address", true
	}
	return "", false
}
