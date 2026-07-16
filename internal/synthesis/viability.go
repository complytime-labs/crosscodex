package synthesis

import (
	"math"

	"github.com/complytime-labs/crosscodex/pkg/config"
)

// abstractionIndex maps level names to numeric indices for distance calculation.
// Python parity: Strategic=0, Tactical=1, Operational=2, unknown=1.
func abstractionIndex(level string) int {
	switch level {
	case "Strategic":
		return 0
	case "Tactical":
		return 1
	case "Operational":
		return 2
	default:
		return 1 // Python default for unknown levels
	}
}

// typeFactor computes the type compatibility factor.
// Python parity: same type or "Both" in either → 1.0,
// "Unknown" or "None" in either → 0.9, else → TypeMismatchFactor.
func typeFactor(srcType, tgtType string, cfg config.ViabilityConfig) float64 {
	if srcType == tgtType || srcType == "Both" || tgtType == "Both" {
		return 1.0
	}
	if srcType == "Unknown" || tgtType == "Unknown" || srcType == "None" || tgtType == "None" {
		return 0.9
	}
	return cfg.TypeMismatchFactor
}

// levelFactor computes the level proximity factor.
// Python parity: abs(srcIdx - tgtIdx) <= 1 → 1.0, else → SkipLevelFactor.
func levelFactor(srcLevel, tgtLevel string, cfg config.ViabilityConfig) float64 {
	s := abstractionIndex(srcLevel)
	t := abstractionIndex(tgtLevel)
	if intAbs(s-t) <= 1 {
		return 1.0
	}
	return cfg.SkipLevelFactor
}

// intAbs returns the absolute value of x.
func intAbs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// round2 rounds to 2 decimal places, matching Python's round(x, 2).
func round2(x float64) float64 {
	return math.Round(x*100) / 100
}

// ComputeViability computes the viability score with Python-compatible rounding.
// This is the INNER rounding step (round 1 of 2).
func ComputeViability(score float64, srcType, tgtType, srcLevel, tgtLevel string, cfg config.ViabilityConfig) float64 {
	tf := typeFactor(srcType, tgtType, cfg)
	lf := levelFactor(srcLevel, tgtLevel, cfg)
	return round2(score * tf * lf)
}

// ComputeViabilityWeight computes the viability weight with two-round rounding.
// Round 1: ComputeViability (inner). Round 2: multiply by contribution factor
// and round again. Two rounds preserve exact Python behavior.
func ComputeViabilityWeight(score float64, srcType, tgtType, srcLevel, tgtLevel, contribType string, cfg config.ViabilityConfig) float64 {
	viability := ComputeViability(score, srcType, tgtType, srcLevel, tgtLevel, cfg)
	factor := 1.0
	if contribType == "INTEGRAL_TO" {
		factor = cfg.IntegralToFactor
	}
	return round2(viability * factor)
}
