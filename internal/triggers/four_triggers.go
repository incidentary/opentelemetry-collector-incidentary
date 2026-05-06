// Package triggers implements the four-trigger Incidentary alert
// evaluator.
//
// This is a Bridge-shaped port of incidentary-sdk-go's per-service
// trigger engine. The four triggers — 5xx_error_rate, slow_success_rate,
// retry_rate, and in_flight_pileup — share the SDK's thresholds and
// 10-second sliding window so the trigger-parity test holds.
//
// The Bridge differs from the SDK in two ways:
//   * Per-service map with LRU+TTL eviction so cluster-wide traffic
//     does not unbounded-grow trigger state.
//   * Webhook-fired flush is NOT implemented — backend-fired flush
//     ships in a future minor release.
package triggers

import (
	"sync"
	"time"
)

const (
	windowBuckets    = 10
	windowSeconds    = 10
	defaultMaxServices = 4096
	defaultServiceTTL  = 10 * time.Minute
	minSamplesDefault  = 20
	slowMultiplier     = 3.0
	ewmaAlpha          = 0.05
	minBaselineNs      = int64(time.Millisecond)
	maxBaselineNs      = int64(time.Hour)
	retryWindowMs      = int64(10_000)
	retryProbeLimit    = 8
	retryTableSize     = 256
)

// Config configures the per-service trigger evaluator.
//
// Field names mirror the SDK's prearm constants. Defaults are pinned
// to the SDK 1.0.0 values so the trigger-parity test stays trivial.
type Config struct {
	// ErrorRate5xx
	ErrorRate5xxHigh float64
	ErrorRate5xxLow  float64

	// SlowSuccessRate
	SlowSuccessHigh float64
	SlowSuccessMild float64

	// RetryRate
	RetryHigh float64
	RetryMild float64

	// InFlightPileup
	InFlightMinAbsolute int64
	InFlightMultiplier  float64
	InFlightNetGrowth   int64
	SevereHoldSecs      int64
	MildHoldSecs        int64

	// LRU cap on per-service state.
	MaxServices int
	ServiceTTL  time.Duration

	// MinSamples avoids firing on tiny windows (e.g., 1 in 1).
	MinSamples int
}

// DefaultConfig returns the Config the four 1.0.0 SDKs encode. Tests
// expect these exact constants — the parity contract depends on them.
func DefaultConfig() Config {
	return Config{
		ErrorRate5xxHigh:    0.10,
		ErrorRate5xxLow:     0.02,
		SlowSuccessHigh:     0.20,
		SlowSuccessMild:     0.10,
		RetryHigh:           0.10,
		RetryMild:           0.05,
		InFlightMinAbsolute: 16,
		InFlightMultiplier:  3.0,
		InFlightNetGrowth:   8,
		SevereHoldSecs:      6,
		MildHoldSecs:        3,
		MaxServices:         defaultMaxServices,
		ServiceTTL:          defaultServiceTTL,
		MinSamples:          minSamplesDefault,
	}
}

// Severity is "mild" or "severe" — empty string means "did not fire".
type Severity string

const (
	SeverityMild   Severity = "mild"
	SeveritySevere Severity = "severe"
)

// TriggerType names the four triggers verbatim.
type TriggerType string

const (
	TriggerErrorRate5xx    TriggerType = "5xx_error_rate"
	TriggerSlowSuccessRate TriggerType = "slow_success_rate"
	TriggerRetryRate       TriggerType = "retry_rate"
	TriggerInFlightPileup  TriggerType = "in_flight_pileup"
)

// FlushReason describes why a trigger fired. The Bridge passes this
// up to the processor which then drains the matching trace from the
// ring buffer.
type FlushReason struct {
	Service        string
	TriggerType    TriggerType
	Severity       Severity
	ObservedValue  float64
	ThresholdValue float64
	FiredAtUnixMs  int64
}

// RequestSignal is what the processor feeds the evaluator after a
// single span completes. Fields the evaluator does not care about
// for a given span (e.g., StatusCode for an outbound RPC retry probe)
// can be left at zero — the trigger logic gates on Kind.
type RequestSignal struct {
	Kind                 SignalKind
	StatusCode           int
	DurationNs           int64
	OutboundRetryKeyHash uint64
	IsRetry              *bool // explicit retry hint, optional
	NowUnixMs            int64
}

// SignalKind narrows the per-span semantic. Maps to OTel SpanKind:
//   SignalServer  ← SpanKind=SPAN_KIND_SERVER     (HTTP/RPC inbound)
//   SignalClient  ← SpanKind=SPAN_KIND_CLIENT     (HTTP/RPC outbound)
//   SignalOther   ← internal/producer/consumer    (ignored)
type SignalKind int

const (
	SignalOther SignalKind = iota
	SignalServer
	SignalClient
)

// Evaluator is the per-service trigger engine map.
type Evaluator struct {
	mu  sync.Mutex
	cfg Config
	now func() time.Time

	services map[string]*serviceState
	order    []string // FIFO for LRU eviction
}

// New constructs an Evaluator. now=nil falls back to time.Now.
func New(cfg Config, now func() time.Time) *Evaluator {
	if now == nil {
		now = time.Now
	}
	if cfg.MaxServices <= 0 {
		cfg.MaxServices = defaultMaxServices
	}
	if cfg.ServiceTTL <= 0 {
		cfg.ServiceTTL = defaultServiceTTL
	}
	if cfg.MinSamples <= 0 {
		cfg.MinSamples = minSamplesDefault
	}
	return &Evaluator{
		cfg:      cfg,
		now:      now,
		services: make(map[string]*serviceState),
	}
}

// OnRequestStart records that a request is in-flight for a service.
// Pair with OnRequestComplete; uses now() for the timestamp.
func (e *Evaluator) OnRequestStart(service string) {
	if service == "" {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	st := e.touch(service)
	st.inFlightCurrent++
	if st.inFlightCurrent > st.inFlightPeak {
		st.inFlightPeak = st.inFlightCurrent
	}
}

// OnRequestComplete updates the trigger windows for a single completed
// span. Returns the firing reason (mild→severe transition included)
// or nil. If service is empty, the call is a no-op.
func (e *Evaluator) OnRequestComplete(service string, sig RequestSignal) *FlushReason {
	if service == "" {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	now := e.now()
	if sig.NowUnixMs == 0 {
		sig.NowUnixMs = now.UnixMilli()
	}
	st := e.touch(service)

	if st.inFlightCurrent > 0 {
		st.inFlightCurrent--
	}

	bucket := int(sig.NowUnixMs/1000) % windowBuckets
	st.advanceBucket(bucket, sig.NowUnixMs/1000)

	switch sig.Kind {
	case SignalServer:
		st.errorWindow.total[bucket]++
		if sig.StatusCode >= 500 && sig.StatusCode < 600 {
			st.errorWindow.hits[bucket]++
		}
		st.successWindow.recordIfSuccessLike(sig, bucket)
	case SignalClient:
		st.retryWindow.total[bucket]++
		if isRetry(st, sig) {
			st.retryWindow.hits[bucket]++
		}
	}

	// Evaluate each trigger after the update.
	if r := st.evaluate5xx(e.cfg, sig.NowUnixMs, service); r != nil {
		return r
	}
	if r := st.evaluateSlowSuccess(e.cfg, sig.NowUnixMs, service); r != nil {
		return r
	}
	if r := st.evaluateRetry(e.cfg, sig.NowUnixMs, service); r != nil {
		return r
	}
	if r := st.evaluateInFlight(e.cfg, sig.NowUnixMs, now, service); r != nil {
		return r
	}
	return nil
}

// PruneStale removes services whose last activity is older than
// ServiceTTL. Called periodically to bound memory.
//
// The previous implementation used the
// `survivors := e.order[:0]` in-place filter idiom, which is
// memory-correct in isolation but reuses the original backing array.
// The slot tail (which holds evicted service-name string headers)
// is not garbage collected until the slice grows past its original
// capacity. Under sustained synthetic-name flooding this causes the
// retained backing array to hold up to ~MaxServices × avg-name-len
// bytes of stale references — small in absolute terms but visible
// on heap snapshots and proportional to attacker-controlled input.
// Allocate a fresh slice for survivors so the old backing array
// (and its retained string headers) becomes eligible for GC
// immediately.
func (e *Evaluator) PruneStale() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	removed := 0
	survivors := make([]string, 0, len(e.order))
	for _, name := range e.order {
		st, ok := e.services[name]
		if !ok {
			continue
		}
		if now.Sub(st.lastSeen) > e.cfg.ServiceTTL {
			delete(e.services, name)
			removed++
			continue
		}
		survivors = append(survivors, name)
	}
	e.order = survivors
	return removed
}

// Services returns the current count of per-service entries.
func (e *Evaluator) Services() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.services)
}

func (e *Evaluator) touch(service string) *serviceState {
	st, ok := e.services[service]
	if !ok {
		// LRU eviction if at cap.
		for len(e.services) >= e.cfg.MaxServices && len(e.order) > 0 {
			oldest := e.order[0]
			e.order = e.order[1:]
			delete(e.services, oldest)
		}
		st = newServiceState()
		e.services[service] = st
		e.order = append(e.order, service)
	} else {
		// Without this, e.order is pure
		// insertion-order FIFO and an attacker who floods MaxServices
		// distinct synthetic service names will evict the oldest
		// real production service regardless of how recently it was
		// touched. Move-to-tail makes the eviction queue a true LRU.
		moveServiceToTail(e, service)
	}
	st.lastSeen = e.now()
	return st
}

// moveServiceToTail removes `service` from its current position in
// `e.order` and re-appends it. O(n) over MaxServices on every touch,
// but n is small (default 4096) and the alternative — a doubly linked
// list keyed by service name — adds significant complexity for a hot
// path that is dwarfed by the trigger evaluator's own work.
func moveServiceToTail(e *Evaluator, service string) {
	for i, name := range e.order {
		if name == service {
			e.order = append(e.order[:i], e.order[i+1:]...)
			break
		}
	}
	e.order = append(e.order, service)
}

type serviceState struct {
	lastSeen time.Time

	inFlightCurrent     int64
	inFlightPeak        int64
	inFlightEWMA        float64
	inFlightConditionAt int64 // unix ms when condition first met
	lastInFlightSev     Severity

	errorWindow   countingWindow
	successWindow successWindow
	retryWindow   countingWindow

	lastErrSev   Severity
	lastSlowSev  Severity
	lastRetrySev Severity

	retryTableHash    [retryTableSize]uint64
	retryTableLastSec [retryTableSize]int64
}

func newServiceState() *serviceState {
	st := &serviceState{}
	st.errorWindow.init()
	st.successWindow.init()
	st.retryWindow.init()
	return st
}

func (s *serviceState) advanceBucket(bucket int, sec int64) {
	s.errorWindow.maybeRoll(bucket, sec)
	s.successWindow.maybeRoll(bucket, sec)
	s.retryWindow.maybeRoll(bucket, sec)
}

// countingWindow is hits / total over a 10-second sliding window.
type countingWindow struct {
	hits      [windowBuckets]int64
	total     [windowBuckets]int64
	bucketSec [windowBuckets]int64
}

func (w *countingWindow) init() {
	for i := range w.bucketSec {
		w.bucketSec[i] = -1
	}
}

func (w *countingWindow) maybeRoll(bucket int, sec int64) {
	if w.bucketSec[bucket] == sec {
		return
	}
	w.bucketSec[bucket] = sec
	w.hits[bucket] = 0
	w.total[bucket] = 0
}

func (w *countingWindow) collect(nowSec int64) (hits, total int64) {
	minSec := nowSec - (windowSeconds - 1)
	for i := 0; i < windowBuckets; i++ {
		s := w.bucketSec[i]
		if s >= minSec && s <= nowSec {
			hits += w.hits[i]
			total += w.total[i]
		}
	}
	return hits, total
}

// successWindow tracks slow_success / total_success with an EWMA
// baseline for "slow".
type successWindow struct {
	countingWindow
	ewmaNs float64
}

func (w *successWindow) init() {
	w.countingWindow.init()
}

func (w *successWindow) recordIfSuccessLike(sig RequestSignal, bucket int) {
	if sig.StatusCode < 200 || sig.StatusCode >= 400 {
		return
	}
	w.total[bucket]++
	durationClamped := clamp64(sig.DurationNs, minBaselineNs, maxBaselineNs)
	if w.ewmaNs <= 0 {
		w.ewmaNs = float64(durationClamped)
	} else {
		w.ewmaNs += ewmaAlpha * (float64(durationClamped) - w.ewmaNs)
	}
	threshold := int64(w.ewmaNs * slowMultiplier)
	if sig.DurationNs > threshold {
		w.hits[bucket]++
	}
}

func (s *serviceState) evaluate5xx(cfg Config, nowMs int64, service string) *FlushReason {
	nowSec := nowMs / 1000
	hits, total := s.errorWindow.collect(nowSec)
	if total < int64(cfg.MinSamples) {
		s.lastErrSev = ""
		return nil
	}
	rate := float64(hits) / float64(total)
	current := classifyTwoTier(rate, cfg.ErrorRate5xxLow, cfg.ErrorRate5xxHigh)
	prev := s.lastErrSev
	s.lastErrSev = current
	if !shouldEmitTransition(prev, current) {
		return nil
	}
	threshold := cfg.ErrorRate5xxLow
	if current == SeveritySevere {
		threshold = cfg.ErrorRate5xxHigh
	}
	return &FlushReason{
		Service:        service,
		TriggerType:    TriggerErrorRate5xx,
		Severity:       current,
		ObservedValue:  rate,
		ThresholdValue: threshold,
		FiredAtUnixMs:  nowMs,
	}
}

func (s *serviceState) evaluateSlowSuccess(cfg Config, nowMs int64, service string) *FlushReason {
	nowSec := nowMs / 1000
	hits, total := s.successWindow.collect(nowSec)
	if total < int64(cfg.MinSamples) {
		s.lastSlowSev = ""
		return nil
	}
	rate := float64(hits) / float64(total)
	current := classifyTwoTier(rate, cfg.SlowSuccessMild, cfg.SlowSuccessHigh)
	prev := s.lastSlowSev
	s.lastSlowSev = current
	if !shouldEmitTransition(prev, current) {
		return nil
	}
	threshold := cfg.SlowSuccessMild
	if current == SeveritySevere {
		threshold = cfg.SlowSuccessHigh
	}
	return &FlushReason{
		Service:        service,
		TriggerType:    TriggerSlowSuccessRate,
		Severity:       current,
		ObservedValue:  rate,
		ThresholdValue: threshold,
		FiredAtUnixMs:  nowMs,
	}
}

func (s *serviceState) evaluateRetry(cfg Config, nowMs int64, service string) *FlushReason {
	nowSec := nowMs / 1000
	hits, total := s.retryWindow.collect(nowSec)
	if total < int64(cfg.MinSamples) {
		s.lastRetrySev = ""
		return nil
	}
	rate := float64(hits) / float64(total)
	current := classifyTwoTier(rate, cfg.RetryMild, cfg.RetryHigh)
	prev := s.lastRetrySev
	s.lastRetrySev = current
	if !shouldEmitTransition(prev, current) {
		return nil
	}
	threshold := cfg.RetryMild
	if current == SeveritySevere {
		threshold = cfg.RetryHigh
	}
	return &FlushReason{
		Service:        service,
		TriggerType:    TriggerRetryRate,
		Severity:       current,
		ObservedValue:  rate,
		ThresholdValue: threshold,
		FiredAtUnixMs:  nowMs,
	}
}

func (s *serviceState) evaluateInFlight(cfg Config, nowMs int64, now time.Time, service string) *FlushReason {
	threshold := cfg.InFlightMinAbsolute
	if int64(s.inFlightEWMA*cfg.InFlightMultiplier) > threshold {
		threshold = int64(s.inFlightEWMA * cfg.InFlightMultiplier)
	}
	conditionMet := s.inFlightCurrent >= threshold

	if conditionMet {
		if s.inFlightConditionAt == 0 {
			s.inFlightConditionAt = nowMs
		}
	} else {
		s.inFlightConditionAt = 0
	}

	// Update EWMA peak for adaptive baseline (only when not pre-armed).
	if s.lastInFlightSev == "" && s.inFlightPeak > 0 {
		if s.inFlightEWMA <= 0 {
			s.inFlightEWMA = float64(s.inFlightPeak)
		} else {
			s.inFlightEWMA += 0.05 * (float64(s.inFlightPeak) - s.inFlightEWMA)
		}
	}

	current := Severity("")
	if conditionMet && s.inFlightConditionAt > 0 {
		holdSecs := (nowMs - s.inFlightConditionAt) / 1000
		switch {
		case holdSecs >= cfg.SevereHoldSecs:
			current = SeveritySevere
		case holdSecs >= cfg.MildHoldSecs:
			current = SeverityMild
		}
	}
	prev := s.lastInFlightSev
	s.lastInFlightSev = current
	if !shouldEmitTransition(prev, current) {
		return nil
	}
	_ = now
	return &FlushReason{
		Service:        service,
		TriggerType:    TriggerInFlightPileup,
		Severity:       current,
		ObservedValue:  float64(s.inFlightCurrent),
		ThresholdValue: float64(threshold),
		FiredAtUnixMs:  nowMs,
	}
}

func isRetry(st *serviceState, sig RequestSignal) bool {
	if sig.IsRetry != nil {
		return *sig.IsRetry
	}
	if sig.OutboundRetryKeyHash == 0 {
		return false
	}
	keyHash := sig.OutboundRetryKeyHash
	idx := int(keyHash & uint64(retryTableSize-1))
	for offset := 0; offset < retryProbeLimit; offset++ {
		i := (idx + offset) & (retryTableSize - 1)
		if st.retryTableHash[i] == 0 {
			st.retryTableHash[i] = keyHash
			st.retryTableLastSec[i] = sig.NowUnixMs / 1000
			return false
		}
		if st.retryTableHash[i] == keyHash {
			ageMs := sig.NowUnixMs - st.retryTableLastSec[i]*1000
			st.retryTableLastSec[i] = sig.NowUnixMs / 1000
			return ageMs <= retryWindowMs
		}
	}
	// Probe limit exceeded — overwrite oldest in the probe sequence.
	st.retryTableHash[idx] = keyHash
	st.retryTableLastSec[idx] = sig.NowUnixMs / 1000
	return false
}

func classifyTwoTier(rate, mild, high float64) Severity {
	if rate >= high {
		return SeveritySevere
	}
	if rate >= mild {
		return SeverityMild
	}
	return ""
}

func shouldEmitTransition(prev, current Severity) bool {
	if current == "" {
		return false
	}
	if prev == "" {
		return true
	}
	return prev == SeverityMild && current == SeveritySevere
}

func clamp64(v, lo, hi int64) int64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
