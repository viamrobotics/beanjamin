package main

// Least-squares and summary statistics shared by the ice commands: ice-level
// fits measured contents height against the poured level.

import "math"

// fitPoint is one capture reduced to the two columns the fit needs.
type fitPoint struct {
	truth float64 // fill level as filled, 0..1
	z     float64 // measured contents height, px or world mm depending on the caller
	label string
}

// leastSquares fits z = slope*truth + intercept and returns the fit with its R².
func leastSquares(points []fitPoint) (slope, intercept, r2 float64) {
	n := float64(len(points))
	var sumX, sumY, sumXY, sumXX float64
	for _, p := range points {
		sumX += p.truth
		sumY += p.z
		sumXY += p.truth * p.z
		sumXX += p.truth * p.truth
	}
	denom := n*sumXX - sumX*sumX
	if denom == 0 {
		return 0, 0, 0
	}
	slope = (n*sumXY - sumX*sumY) / denom
	intercept = (sumY - slope*sumX) / n

	meanY := sumY / n
	var ssRes, ssTot float64
	for _, p := range points {
		fitted := slope*p.truth + intercept
		ssRes += (p.z - fitted) * (p.z - fitted)
		ssTot += (p.z - meanY) * (p.z - meanY)
	}
	if ssTot == 0 {
		return slope, intercept, 0
	}
	return slope, intercept, 1 - ssRes/ssTot
}

func mean(vals []float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	total := 0.0
	for _, v := range vals {
		total += v
	}
	return total / float64(len(vals))
}

// stddev returns the sample standard deviation, or 0 for fewer than 2 values.
func stddev(vals []float64) float64 {
	if len(vals) < 2 {
		return 0
	}
	m := mean(vals)
	variance := 0.0
	for _, v := range vals {
		variance += (v - m) * (v - m)
	}
	return math.Sqrt(variance / float64(len(vals)-1))
}
