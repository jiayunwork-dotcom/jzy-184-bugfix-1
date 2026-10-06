// Package gdd computes daily growing degree days (GDD, 日积温, °C·day)
// from daily extreme temperatures.
//
// Three conventions are provided:
//
//   - Sine (default): the single-sine horizontal-cutoff method (Baskerville &
//     Emin 1969; the convention used by UC IPM and most maize guidance). The
//     diurnal temperature path is modeled by one half-sine wave from TMin at
//     sunrise to TMax in the afternoon and back to TMin. Temperatures below
//     base and above ceiling are clipped to base/ceiling before integrating,
//     which captures heat gained on days whose minimum is below the base.
//   - Triangle: same truncation but with a piecewise-linear (triangular) path.
//   - Mean: (TMax+TMin)/2 - base with no upper cutoff and a zero floor.
//
// All conventions obey:
//
//  1. when TMin >= base and TMax <= ceiling, daily GDD = mean - base;
//  2. when TMax <= base, daily GDD = 0;
//  3. the result is never negative;
//  4. raising the base can never increase daily GDD (and can never move a
//     stage date earlier) — see monotone_test.go.
package gdd

import "math"

// Method selects a daily GDD convention.
type Method string

const (
	MethodSine     Method = "sine"
	MethodTriangle Method = "triangle"
	MethodMean     Method = "mean"
)

// Daily computes one day's GDD with the given convention.
func Daily(m Method, tmax, tmin, base, ceil float64) float64 {
	switch m {
	case MethodMean:
		return Mean(tmax, tmin, base)
	case MethodTriangle:
		return Triangle(tmax, tmin, base, ceil)
	default:
		return Sine(tmax, tmin, base, ceil)
	}
}

// Mean is the simple convention: average temperature minus base, floored at 0.
// It intentionally has no upper cutoff, matching the stated "直接取平均减基点"
// rule; the ceiling only enters the two curve methods.
func Mean(tmax, tmin, base float64) float64 {
	v := (tmax+tmin)/2 - base
	if v < 0 {
		return 0
	}
	return v
}

// meanAbove is the mean over [0,2π] of max(cos u - c, 0). The diurnal wave is
// parameterized with cos u so that u=0 is the temperature maximum and u=π the
// minimum; cos and sin share a distribution, so the closed form is identical.
//
// If cos u <= c everywhere (c >= 1), the mean is 0. If cos u > c everywhere
// (c <= -1) it is -c.
func meanAbove(c float64) float64 {
	switch {
	case c >= 1:
		return 0
	case c <= -1:
		return -c
	}
	t := math.Acos(c)
	// cos u > c on [-t,t]; ∫cos u du = 2 sin t, ∫c du = 2ct.
	return (2*math.Sin(t) - 2*c*t) / (2 * math.Pi)
}

// Sine returns daily GDD using the single-sine method with horizontal cutoff.
//
// The wave temperature is W(u) = m + b cos u, u ∈ [0,2π], where
// m = (Tx+Tn)/2, b = (Tx-Tn)/2, u=0 is the daily maximum and u=π the minimum.
// After horizontal clipping into [base,ceil],
//
//	clip(W) = W - max(W-ceil,0) + max(base-W,0)
//	mean(clip(W)) = m - b*meanAbove(αc) + b*meanAbove(αb)
//
// with αc = (ceil-m)/b, αb = (m-base)/b, and GDD = mean(clip(W)) - base.
// W > ceil exactly when cos u > αc, and W < base exactly when cos u < -αb,
// whose mean equals meanAbove(αb) by cos(π-u)=-cos u symmetry.
func Sine(tmax, tmin, base, ceil float64) float64 {
	if tmax <= base {
		return 0
	}
	if tmin >= ceil {
		return ceil - base
	}
	// Guard against bad input; callers validate but stay defensive.
	if tmax < tmin {
		tmax, tmin = tmin, tmax
	}
	if !(ceil > base) {
		// Degenerate thresholds: fall back to mean convention.
		return Mean(tmax, tmin, base)
	}

	// Interior: keep the ORIGINAL wave m,b; horizontal clipping is applied
	// purely through the integral correction terms. Clamping the extrema here
	// would wrongly distort the diurnal amplitude.
	m := (tmax + tmin) / 2
	b := (tmax - tmin) / 2
	if b <= 0 {
		v := math.Max(base, math.Min(ceil, m)) - base
		if v < 0 {
			return 0
		}
		return v
	}

	alphaC := (ceil - m) / b
	alphaB := (m - base) / b
	clipMean := m - b*meanAbove(alphaC) + b*meanAbove(alphaB)
	v := clipMean - base
	if v < 0 {
		return 0
	}
	return v
}

// Triangle returns daily GDD with a triangular (piecewise-linear) diurnal
// approximation and horizontal cutoff. On the rising half-day temperature
// moves linearly Tn→Tx and on the falling half-day Tx→Tn.
func Triangle(tmax, tmin, base, ceil float64) float64 {
	if tmax <= base {
		return 0
	}
	if tmin >= ceil {
		return ceil - base
	}
	if tmax < tmin {
		tmax, tmin = tmin, tmax
	}
	if !(ceil > base) {
		return Mean(tmax, tmin, base)
	}

	// Integrate each half segment by splitting at the threshold kinks and
	// averaging the clipped linear temperature with the trapezoid rule.
	rise := segAvg(tmin, tmax, base, ceil)
	fall := segAvg(tmax, tmin, base, ceil)
	v := (rise+fall)/2 - base
	if v < 0 {
		return 0
	}
	return v
}

// segAvg integrates clip11(f(x))/1 over x∈[0,1], where f is linear from a to
// b and clip11 clamps into [base,ceil], by integrating exactly between the
// kink positions.
func segAvg(a, b, base, ceil float64) float64 {
	xs := []float64{0, 1}
	add := func(threshold float64) {
		if b != a {
			x := (threshold - a) / (b - a)
			if x > 0 && x < 1 {
				xs = append(xs, x)
			}
		}
	}
	add(base)
	add(ceil)
	sortFloat(xs)

	sum := 0.0
	for i := 1; i < len(xs); i++ {
		x0, x1 := xs[i-1], xs[i]
		xm := (x0 + x1) / 2
		t := a + (b-a)*xm
		t = math.Max(base, math.Min(ceil, t))
		sum += t * (x1 - x0)
	}
	return sum
}

func sortFloat(xs []float64) {
	for i := 1; i < len(xs); i++ {
		for j := i; j > 0 && xs[j-1] > xs[j]; j-- {
			xs[j-1], xs[j] = xs[j], xs[j-1]
		}
	}
}
