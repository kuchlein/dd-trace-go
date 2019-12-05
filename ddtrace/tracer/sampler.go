// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-2019 Datadog, Inc.

package tracer

import (
	"encoding/json"
	"io"
	"math"
	"os"
	"regexp"
	"strconv"
	"sync"
	"time"

	"gopkg.in/DataDog/dd-trace-go.v1/ddtrace"
	"gopkg.in/DataDog/dd-trace-go.v1/ddtrace/ext"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/log"

	"golang.org/x/time/rate"
)

// Sampler is the generic interface of any sampler. It must be safe for concurrent use.
type Sampler interface {
	// Sample returns true if the given span should be sampled.
	Sample(span Span) bool
}

// RateSampler is a sampler implementation which randomly selects spans using a
// provided rate. For example, a rate of 0.75 will permit 75% of the spans.
// RateSampler implementations should be safe for concurrent use.
type RateSampler interface {
	Sampler

	// Rate returns the current sample rate.
	Rate() float64

	// SetRate sets a new sample rate.
	SetRate(rate float64)
}

// rateSampler samples from a sample rate.
type rateSampler struct {
	sync.RWMutex
	rate float64
}

// NewAllSampler is a short-hand for NewRateSampler(1). It is all-permissive.
func NewAllSampler() RateSampler { return NewRateSampler(1) }

// NewRateSampler returns an initialized RateSampler with a given sample rate.
func NewRateSampler(rate float64) RateSampler {
	return &rateSampler{rate: rate}
}

// Rate returns the current rate of the sampler.
func (r *rateSampler) Rate() float64 {
	r.RLock()
	defer r.RUnlock()
	return r.rate
}

// SetRate sets a new sampling rate.
func (r *rateSampler) SetRate(rate float64) {
	r.Lock()
	r.rate = rate
	r.Unlock()
}

// constants used for the Knuth hashing, same as agent.
const knuthFactor = uint64(1111111111111111111)

// Sample returns true if the given span should be sampled.
func (r *rateSampler) Sample(spn ddtrace.Span) bool {
	if r.rate == 1 {
		// fast path
		return true
	}
	s, ok := spn.(*span)
	if !ok {
		return false
	}
	r.RLock()
	defer r.RUnlock()
	return sampledByRate(s.TraceID, r.rate)
}

// sampledByRate verifies if the number n should be sampled at the specified
// rate.
func sampledByRate(n uint64, rate float64) bool {
	if rate < 1 {
		return n*knuthFactor < uint64(rate*math.MaxUint64)
	}
	return true
}

// prioritySampler holds a set of per-service sampling rates and applies
// them to spans.
type prioritySampler struct {
	mu          sync.RWMutex
	rates       map[string]float64
	defaultRate float64
}

func newPrioritySampler() *prioritySampler {
	return &prioritySampler{
		rates:       make(map[string]float64),
		defaultRate: 1.,
	}
}

// readRatesJSON will try to read the rates as JSON from the given io.ReadCloser.
func (ps *prioritySampler) readRatesJSON(rc io.ReadCloser) error {
	var payload struct {
		Rates map[string]float64 `json:"rate_by_service"`
	}
	if err := json.NewDecoder(rc).Decode(&payload); err != nil {
		return err
	}
	rc.Close()
	const defaultRateKey = "service:,env:"
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.rates = payload.Rates
	if v, ok := ps.rates[defaultRateKey]; ok {
		ps.defaultRate = v
		delete(ps.rates, defaultRateKey)
	}
	return nil
}

// getRate returns the sampling rate to be used for the given span. Callers must
// guard the span.
func (ps *prioritySampler) getRate(spn *span) float64 {
	key := "service:" + spn.Service + ",env:" + spn.Meta[ext.Environment]
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	if rate, ok := ps.rates[key]; ok {
		return rate
	}
	return ps.defaultRate
}

// apply applies sampling priority to the given span. Caller must ensure it is safe
// to modify the span.
func (ps *prioritySampler) apply(spn *span) {
	rate := ps.getRate(spn)
	if sampledByRate(spn.TraceID, rate) {
		spn.SetTag(ext.SamplingPriority, ext.PriorityAutoKeep)
	} else {
		spn.SetTag(ext.SamplingPriority, ext.PriorityAutoReject)
	}
	spn.SetTag(keySamplingPriorityRate, rate)
}

type rulesSampler struct {
	rules   []SamplingRule
	rate    float64
	limiter *rate.Limiter
	// "effective rate" calculations
	mu           sync.Mutex
	ts           int64
	allowed      int
	total        int
	previousRate float64
}

func newRulesSampler(rules []SamplingRule) *rulesSampler {
	rate := sampleRate()
	return &rulesSampler{
		rules:   samplingRules(rules),
		rate:    rate,
		limiter: newRateLimiter(rate),
	}
}

// samplingRules validates the user-provided rules and returns an internal representation.
// If the DD_TRACE_SAMPLING_RULES environment variable is set, then the rules from
// tracer.WithSamplingRules are ignored.
func samplingRules(rules []SamplingRule) []SamplingRule {
	rulesFromEnv := os.Getenv("DD_TRACE_SAMPLING_RULES")
	if rulesFromEnv != "" {
		rules = rules[:0]
		jsonRules := []struct {
			Service   string      `json:"service"`
			Operation string      `json:"operation"`
			Rate      json.Number `json:"rate"`
		}{}
		err := json.Unmarshal([]byte(rulesFromEnv), &jsonRules)
		if err != nil {
			log.Warn("error parsing DD_TRACE_SAMPLING_RULES: %v", err)
			return nil
		}
		for _, v := range jsonRules {
			if v.Rate == "" {
				log.Warn("error parsing rule: rate not provided")
				continue
			}
			rate, err := v.Rate.Float64()
			if err != nil {
				log.Warn("error parsing rule: invalid rate: %v", err)
				continue
			}
			switch {
			case v.Service != "" && v.Operation != "":
				rules = append(rules, ServiceOperationRule(v.Service, v.Operation, rate))
			case v.Service != "":
				rules = append(rules, ServiceRule(v.Service, rate))
			case v.Operation != "":
				rules = append(rules, OperationRule(v.Operation, rate))
			}
		}
	}
	validRules := make([]SamplingRule, 0, len(rules))
	for _, v := range rules {
		if !(v.Rate >= 0.0 && v.Rate <= 1.0) {
			log.Warn("ignoring rule %+v: rate is out of range", v)
			continue
		}
		validRules = append(validRules, v)
	}
	return validRules
}

func sampleRate() float64 {
	rate := 0.0
	v := os.Getenv("DD_TRACE_SAMPLE_RATE")
	if v == "" {
		return rate
	}
	r, err := strconv.ParseFloat(v, 64)
	if err != nil {
		log.Warn("using default rate %f because DD_TRACE_SAMPLE_RATE is invalid: %v", rate, err)
		return rate
	}
	if r >= 0.0 && r <= 1.0 {
		return r
	}
	log.Warn("using default rate %f because provided value is out of range: %f", rate, r)
	return rate
}

func newRateLimiter(r float64) *rate.Limiter {
	v := os.Getenv("DD_TRACE_RATE_LIMIT")
	if v == "" {
		return nil
	}
	l, err := strconv.ParseFloat(v, 64)
	if err != nil {
		log.Warn("using default rate limit because DD_TRACE_RATE_LIMIT is invalid: %v", err)
		return nil
	}
	switch {
	case l < 0.0:
		return nil
	case l == 0.0:
		return rate.NewLimiter(0.0, 0)
	case (r * l) < 1.0:
		return rate.NewLimiter(rate.Limit(l), 1)
	default:
		return rate.NewLimiter(rate.Limit(l), int(math.Ceil(r*l)))
	}
}

func (rs *rulesSampler) apply(span *span) bool {
	matched := false
	sr := 0.0
	for _, v := range rs.rules {
		if v.match(span) {
			matched = true
			sr = v.Rate
			break
		}
	}
	if !matched {
		if rs.rate == 0.0 {
			return false
		}
		sr = rs.rate
	}
	// rate sample
	span.SetTag("_dd.rule_psr", sr)
	if !sampledByRate(span.TraceID, sr) {
		span.SetTag(ext.SamplingPriority, ext.PriorityAutoReject)
		return true
	}
	// global rate limit and effective rate calculations
	defer rs.mu.Unlock()
	rs.mu.Lock()
	if ts := time.Now().Unix(); ts > rs.ts {
		// update "previous rate" and reset
		if ts-rs.ts == 1 && rs.total > 0 && rs.allowed > 0 {
			rs.previousRate = float64(rs.allowed) / float64(rs.total)
		} else {
			rs.previousRate = 0.0
		}
		rs.ts = ts
		rs.allowed = 0
		rs.total = 0
	}

	rs.total++
	if rs.limiter != nil && !rs.limiter.Allow() {
		span.SetTag(ext.SamplingPriority, ext.PriorityAutoReject)
	} else {
		rs.allowed++
		span.SetTag(ext.SamplingPriority, ext.PriorityAutoKeep)
	}
	// calculate effective rate, and tag the span
	er := (rs.previousRate + (float64(rs.allowed) / float64(rs.total))) / 2.0
	span.SetTag("_dd.limit_psr", er)

	return true
}

// SamplingRule is used for applying sampling rates to spans that match
// the service name, operation or both.
// It's recommended to use the helper functions (ServiceRule, OperationRule,
// ServiceOperationRule) instead of directly creating a SamplingRule.
type SamplingRule struct {
	Service   *regexp.Regexp
	Operation *regexp.Regexp
	Rate      float64
	// fields for exact match instead of regexp
	svc string
	op  string
}

// ServiceRule returns a SamplingRule that applies the provided sampling rate
// to spans that match the service name provided.
func ServiceRule(service string, rate float64) SamplingRule {
	return SamplingRule{
		svc:  service,
		Rate: rate,
	}
}

// OperationRule returns a SamplingRule that applies the provided sampling rate
// to spans that match the operation name provided.
func OperationRule(operation string, rate float64) SamplingRule {
	return SamplingRule{
		op:   operation,
		Rate: rate,
	}
}

// ServiceOperationRule returns a SamplingRule that applies the provided sampling rate
// to spans matching both the service and operation names provided.
func ServiceOperationRule(service string, operation string, rate float64) SamplingRule {
	return SamplingRule{
		svc:  service,
		op:   operation,
		Rate: rate,
	}
}

// RateRule returns a SamplingRule that applies the provided sampling rate to all spans.
func RateRule(rate float64) SamplingRule {
	return SamplingRule{
		Rate: rate,
	}
}

func (sr *SamplingRule) match(s *span) bool {
	if sr.Service != nil && !sr.Service.MatchString(s.Service) {
		return false
	} else if sr.svc != "" && sr.svc != s.Service {
		return false
	}
	if sr.Operation != nil && !sr.Operation.MatchString(s.Name) {
		return false
	} else if sr.op != "" && sr.op != s.Name {
		return false
	}
	return true
}
