package consensus

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// Computer computes consensus from votes with configurable strategies.
type Computer struct {
	threshold     float64 // Minimum confidence fraction required (default: 0.5)
	minValidVotes int     // Minimum number of successful votes required
	maxErrorRate  float64 // Maximum fraction of votes that can fail [0.0, 1.0]
	tracer        trace.Tracer
	histogram     metric.Float64Histogram
}

// Option configures a Computer.
type Option func(*Computer)

// WithThreshold sets the minimum confidence fraction (default: 0.5).
// threshold=0.5 means simple majority, 0.6 means 60%, 1.0 means unanimous.
func WithThreshold(t float64) Option {
	return func(c *Computer) {
		if t < 0.0 || t > 1.0 {
			t = 0.5
		}
		c.threshold = t
	}
}

// WithMinValidVotes sets the minimum number of successful votes required.
// If fewer valid votes are received, Compute returns ErrInsufficientVotes.
func WithMinValidVotes(n int) Option {
	return func(c *Computer) {
		c.minValidVotes = n
	}
}

// WithMaxErrorRate sets the maximum fraction of votes that can fail.
// Example: 0.25 means allow up to 25% of votes to error.
func WithMaxErrorRate(f float64) Option {
	return func(c *Computer) {
		if f < 0.0 || f > 1.0 {
			f = 0.0
		}
		c.maxErrorRate = f
	}
}

// WithTelemetry configures OTel instrumentation.
func WithTelemetry(tracer trace.Tracer, histogram metric.Float64Histogram) Option {
	return func(c *Computer) {
		c.tracer = tracer
		c.histogram = histogram
	}
}

// New creates a Computer with the given options.
func New(opts ...Option) *Computer {
	c := &Computer{
		threshold:     0.5,
		minValidVotes: 0,
		maxErrorRate:  0.0,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Compute calculates weighted consensus from a set of votes.
// Returns ErrInsufficientVotes if minimum vote thresholds are not met.
func (c *Computer) Compute(votes []Vote) (Result, error) {
	if len(votes) == 0 {
		return Result{}, fmt.Errorf("consensus: no votes provided")
	}

	ctx := context.Background()
	var span trace.Span
	if c.tracer != nil {
		ctx, span = c.tracer.Start(ctx, "consensus.Compute")
		defer span.End()
	}

	// Count weighted votes
	var yesWeight, noWeight, totalWeight float64
	validCount := 0
	for _, v := range votes {
		if v.Decision == nil {
			continue
		}
		weight := v.Weight
		if weight == 0 {
			weight = 1.0 // Default weight
		}
		totalWeight += weight
		validCount++

		if *v.Decision {
			yesWeight += weight
		} else {
			noWeight += weight
		}
	}

	// Check minimum valid votes threshold
	if c.minValidVotes > 0 && validCount < c.minValidVotes {
		err := &ErrInsufficientVotes{
			ValidCount:  validCount,
			RequiredMin: c.minValidVotes,
			TotalCount:  len(votes),
			ErrorCount:  len(votes) - validCount,
		}
		if span != nil {
			span.RecordError(err)
			span.SetAttributes(attribute.String("error.type", "insufficient_votes"))
		}
		return Result{}, err
	}

	// Check maximum error rate threshold
	if c.maxErrorRate > 0 && validCount < len(votes) {
		errorRate := float64(len(votes)-validCount) / float64(len(votes))
		if errorRate > c.maxErrorRate {
			err := &ErrInsufficientVotes{
				ValidCount:  validCount,
				RequiredMin: int((1.0 - c.maxErrorRate) * float64(len(votes))),
				TotalCount:  len(votes),
				ErrorCount:  len(votes) - validCount,
			}
			if span != nil {
				span.RecordError(err)
				span.SetAttributes(attribute.String("error.type", "excessive_error_rate"))
			}
			return Result{}, err
		}
	}

	if validCount == 0 {
		// All votes errored
		return Result{
			Decision:           false,
			ConfidenceFraction: 0.0,
			Unanimous:          false,
			ValidVoteCount:     0,
			TotalVoteCount:     len(votes),
			TotalWeight:        0.0,
		}, nil
	}

	// Determine decision (weighted majority)
	decision := yesWeight > noWeight
	majorityWeight := yesWeight
	if !decision {
		majorityWeight = noWeight
	}

	confidenceFraction := majorityWeight / totalWeight
	unanimous := (yesWeight == totalWeight) || (noWeight == totalWeight)

	result := Result{
		Decision:           decision,
		ConfidenceFraction: confidenceFraction,
		Unanimous:          unanimous,
		ValidVoteCount:     validCount,
		TotalVoteCount:     len(votes),
		TotalWeight:        totalWeight,
	}

	// Record metrics
	if c.histogram != nil {
		c.histogram.Record(ctx, confidenceFraction,
			metric.WithAttributes(
				attribute.Bool("consensus.decision", decision),
				attribute.Bool("consensus.unanimous", unanimous),
			),
		)
	}

	if span != nil {
		span.SetAttributes(
			attribute.Bool("consensus.decision", decision),
			attribute.Float64("consensus.confidence", confidenceFraction),
			attribute.Bool("consensus.unanimous", unanimous),
			attribute.Int("consensus.valid_votes", validCount),
			attribute.Int("consensus.total_votes", len(votes)),
			attribute.Float64("consensus.total_weight", totalWeight),
		)
	}

	return result, nil
}

// MeetsThreshold checks if the result meets the configured threshold.
func (c *Computer) MeetsThreshold(r Result) bool {
	return r.ConfidenceFraction >= c.threshold
}
