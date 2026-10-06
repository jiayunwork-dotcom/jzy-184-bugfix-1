package gdd

import (
	"math"
	"testing"
)

func approxEq(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// TestExample12 covers the spec's worked example: base 10, TMax 30, TMin 14,
// both extremes within [base,ceiling], daily GDD must be exactly 12 for
// every convention.
func TestExample12(t *testing.T) {
	for _, m := range []Method{MethodSine, MethodTriangle, MethodMean} {
		got := Daily(m, 30, 14, 10, 35)
		if !approxEq(got, 12) {
			t.Fatalf("%s: got %v want 12", m, got)
		}
	}
	// Also with a lower ceiling that still clears TMax.
	if got := Daily(MethodSine, 30, 14, 10, 30); !approxEq(got, 12) {
		t.Fatalf("sine ceiling==tmax: got %v want 12", got)
	}
}

// TestZeroWhenCold: TMax <= base implies zero for all conventions.
func TestZeroWhenCold(t *testing.T) {
	cases := [][2]float64{{10, -5}, {9, 9}, {0, -20}, {10, 10}}
	for _, c := range cases {
		for _, m := range []Method{MethodSine, MethodTriangle, MethodMean} {
			if got := Daily(m, c[0], c[1], 10, 30); got != 0 {
				t.Fatalf("%s (%v,%v): got %v want 0", m, c[0], c[1], got)
			}
		}
	}
}

// TestNeverNegative sweeps a broad grid.
func TestNeverNegative(t *testing.T) {
	for tx := -40.0; tx <= 50; tx += 1 {
		for tn := -40.0; tn <= tx; tn += 1 {
			for _, base := range []float64{0, 5, 10, 15} {
				for _, m := range []Method{MethodSine, MethodTriangle, MethodMean} {
					if got := Daily(m, tx, tn, base, 35); got < 0 {
						t.Fatalf("%s(%v,%v,base=%v) = %v < 0", m, tx, tn, base, got)
					}
				}
			}
		}
	}
}

// TestRaiseBaseNeverIncreases: GDD under a higher base must be <= GDD under a
// lower base on the same day, for every day of a wide grid. This is exactly
// the property "raising the base only delays stages".
func TestRaiseBaseNeverIncreases(t *testing.T) {
	for tx := -10.0; tx <= 45; tx += 0.5 {
		for tn := -15.0; tn <= tx; tn += 0.5 {
			lo := Daily(MethodSine, tx, tn, 8, 35)
			hi := Daily(MethodSine, tx, tn, 12, 35)
			if hi > lo+1e-12 {
				t.Fatalf("sine base 12 (%v) > base 8 (%v) at tx=%v tn=%v", hi, lo, tx, tn)
			}
			loT := Daily(MethodTriangle, tx, tn, 8, 35)
			hiT := Daily(MethodTriangle, tx, tn, 12, 35)
			if hiT > loT+1e-12 {
				t.Fatalf("triangle base 12 (%v) > base 8 (%v) at tx=%v tn=%v", hiT, loT, tx, tn)
			}
			loM := Daily(MethodMean, tx, tn, 8, 35)
			hiM := Daily(MethodMean, tx, tn, 12, 35)
			if hiM > loM+1e-12 {
				t.Fatalf("mean base 12 (%v) > base 8 (%v) at tx=%v tn=%v", hiM, loM, tx, tn)
			}
		}
	}
}

// TestCeilingBounds: a day entirely above the ceiling contributes exactly
// ceil-base; a ceiling never raises GDD above the unclipped value.
func TestCeilingBounds(t *testing.T) {
	if got := Daily(MethodSine, 35, 32, 10, 30); !approxEq(got, 20) {
		t.Fatalf("all-hot day: got %v want 20", got)
	}
	if got := Daily(MethodTriangle, 35, 32, 10, 30); !approxEq(got, 20) {
		t.Fatalf("triangle all-hot day: got %v want 20", got)
	}
	for tx := 20.0; tx <= 45; tx += 0.5 {
		for tn := 5.0; tn <= 25; tn += 0.5 {
			s := Daily(MethodSine, tx, tn, 10, 30)
			// Unclipped reference: the same wave with an infinite ceiling.
			ref := Daily(MethodSine, tx, tn, 10, 1e6)
			if s > ref+1e-12 {
				t.Fatalf("ceiling raised GDD: sine %v > unclipped %v (tx=%v tn=%v)", s, ref, tx, tn)
			}
			if s > 20+1e-12 {
				t.Fatalf("GDD %v exceeds ceil-base=20", s)
			}
		}
	}
}

// TestSineExceedsMeanOnColdMornings documents where the default and the mean
// convention differ most: on days whose minimum is below base but afternoon
// is warm, the sine curve recovers real daytime heat while the mean method
// loses it. On those days sine GDD > 0 while mean may be 0.
func TestSineExceedsMeanOnColdMornings(t *testing.T) {
	// TMax 18, TMin 2, base 10: mean = 0 -> floored to 0, but the sine curve
	// integrates the warm afternoon above base and yields a positive value.
	s := Daily(MethodSine, 18, 2, 10, 35)
	m := Daily(MethodMean, 18, 2, 10, 35)
	if !(s > 2 && m == 0) {
		t.Fatalf("cold-morning warm-day: sine=%v mean=%v", s, m)
	}
	// When both extremes are inside the thresholds the two agree exactly.
	if got := math.Abs(Daily(MethodSine, 28, 16, 10, 35) - Mean(28, 16, 10)); got > 1e-12 {
		t.Fatalf("interior disagreement: %v", got)
	}
}
