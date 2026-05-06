package incidentaryprocessor

import (
	"testing"
	"time"
)

// Config defaults must pin to the four 1.0.0 SDKs.
func TestDefaultConfig_PinnedToSDKThresholds(t *testing.T) {
	cfg := DefaultConfig()

	if cfg.PreAlertWindow != 5*time.Minute {
		t.Errorf("pre_alert_window default = %v, want 5m (plan §3.3.1)", cfg.PreAlertWindow)
	}
	if cfg.MaxBufferSize != 50_000 {
		t.Errorf("max_buffer_size default = %d, want 50000 (plan §3.3.1)", cfg.MaxBufferSize)
	}

	// Plan §3.2.2 — these are the trigger constants the SDK 1.0.0 ships.
	// The trigger-parity test (3.4.3) silently breaks if these drift.
	if cfg.Triggers.ErrorRate5xx.High != 0.10 || cfg.Triggers.ErrorRate5xx.Low != 0.02 {
		t.Errorf("5xx_error_rate = %+v, want {High:0.10, Low:0.02} per plan §3.2.2",
			cfg.Triggers.ErrorRate5xx)
	}
	if cfg.Triggers.SlowSuccessRate.High != 0.20 || cfg.Triggers.SlowSuccessRate.Mild != 0.10 {
		t.Errorf("slow_success_rate = %+v, want {High:0.20, Mild:0.10} per plan §3.2.2",
			cfg.Triggers.SlowSuccessRate)
	}
	if cfg.Triggers.RetryRate.High != 0.10 || cfg.Triggers.RetryRate.Mild != 0.05 {
		t.Errorf("retry_rate = %+v, want {High:0.10, Mild:0.05} per plan §3.2.2",
			cfg.Triggers.RetryRate)
	}

	// Forwarding defaults — plan §3.5.1 / §3.5.4.
	if cfg.Forward.IncidentaryEndpoint != "https://api.incidentary.com/api/v1/otlp/v1/traces" {
		t.Errorf("forward.endpoint default = %q, want plan-spec'd default",
			cfg.Forward.IncidentaryEndpoint)
	}
	if !cfg.Forward.PassThrough {
		t.Errorf("forward.pass_through default must be true per plan §3.5.4")
	}
}

// Validate must reject malformed config so the collector
// Never starts a Bridge with an empty endpoint or zero buffer.
//
// Validate must also reject SSRF-bait
// Endpoint URLs (RFC1918, loopback, link-local, AWS IMDS) unless the
// Operator opts into `allow_insecure`. The customer controls
// `forward.endpoint` via collector YAML / Helm values; without these
// Checks a hostile config exfiltrates the bearer token to internal
// Targets like 169.254.169.254 (cloud metadata service).
func TestConfig_Validate(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(*Config)
		wantErrIs string
	}{
		{name: "default is valid"},
		{
			name:      "negative pre_alert_window",
			mutate:    func(c *Config) { c.PreAlertWindow = -time.Second },
			wantErrIs: "pre_alert_window",
		},
		{
			name:      "zero max_buffer_size",
			mutate:    func(c *Config) { c.MaxBufferSize = 0 },
			wantErrIs: "max_buffer_size",
		},
		{
			name:      "empty endpoint",
			mutate:    func(c *Config) { c.Forward.IncidentaryEndpoint = "" },
			wantErrIs: "forward.endpoint",
		},
		{
			name:      "5xx high above 1",
			mutate:    func(c *Config) { c.Triggers.ErrorRate5xx.High = 1.5 },
			wantErrIs: "5xx_error_rate",
		},
		{
			name:      "slow success mild > high",
			mutate:    func(c *Config) { c.Triggers.SlowSuccessRate.Mild = 0.99 },
			wantErrIs: "slow_success_rate",
		},
		{
			name:      "in_flight severe < mild",
			mutate:    func(c *Config) { c.Triggers.InFlightPileup.SevereHoldSecs = 1 },
			wantErrIs: "in_flight_pileup",
		},
		// SSRF guards.
		{
			name: "endpoint scheme http rejected by default",
			mutate: func(c *Config) {
				c.Forward.IncidentaryEndpoint = "http://api.incidentary.com/api/v1/otlp/v1/traces"
			},
			wantErrIs: "scheme",
		},
		{
			name: "endpoint scheme ftp rejected",
			mutate: func(c *Config) {
				c.Forward.IncidentaryEndpoint = "ftp://api.incidentary.com/v1/traces"
			},
			wantErrIs: "scheme",
		},
		{
			name: "endpoint AWS IMDS link-local rejected",
			mutate: func(c *Config) {
				c.Forward.IncidentaryEndpoint = "https://169.254.169.254/latest/meta-data/"
			},
			wantErrIs: "link-local",
		},
		{
			name: "endpoint RFC1918 10.x rejected",
			mutate: func(c *Config) {
				c.Forward.IncidentaryEndpoint = "https://10.0.0.1/v1/traces"
			},
			wantErrIs: "private",
		},
		{
			name: "endpoint RFC1918 192.168.x rejected",
			mutate: func(c *Config) {
				c.Forward.IncidentaryEndpoint = "https://192.168.1.1/v1/traces"
			},
			wantErrIs: "private",
		},
		{
			name: "endpoint RFC1918 172.16.x rejected",
			mutate: func(c *Config) {
				c.Forward.IncidentaryEndpoint = "https://172.16.0.1/v1/traces"
			},
			wantErrIs: "private",
		},
		{
			name: "endpoint loopback 127.0.0.1 rejected",
			mutate: func(c *Config) {
				c.Forward.IncidentaryEndpoint = "https://127.0.0.1/v1/traces"
			},
			wantErrIs: "loopback",
		},
		{
			name: "endpoint localhost host rejected",
			mutate: func(c *Config) {
				c.Forward.IncidentaryEndpoint = "https://localhost/v1/traces"
			},
			wantErrIs: "loopback",
		},
		{
			name: "endpoint missing host rejected",
			mutate: func(c *Config) {
				c.Forward.IncidentaryEndpoint = "https:///v1/traces"
			},
			wantErrIs: "host",
		},
		// Opt-in escape hatch for self-hosted deployments.
		{
			name: "endpoint http accepted when allow_insecure=true",
			mutate: func(c *Config) {
				c.Forward.IncidentaryEndpoint = "http://collector.internal/v1/traces"
				c.Forward.AllowInsecure = true
			},
		},
		{
			name: "endpoint loopback accepted when allow_insecure=true",
			mutate: func(c *Config) {
				c.Forward.IncidentaryEndpoint = "https://localhost/v1/traces"
				c.Forward.AllowInsecure = true
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := DefaultConfig()
			if tt.mutate != nil {
				tt.mutate(cfg)
			}
			err := cfg.Validate()
			if tt.wantErrIs == "" {
				if err != nil {
					t.Fatalf("expected nil error, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error mentioning %q, got nil", tt.wantErrIs)
			}
			if !contains(err.Error(), tt.wantErrIs) {
				t.Fatalf("error %q does not mention %q", err.Error(), tt.wantErrIs)
			}
		})
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
