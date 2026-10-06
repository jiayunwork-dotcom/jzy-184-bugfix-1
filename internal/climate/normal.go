// Package climate builds per-station climatological daily normals and fills
// missing daily observations.
//
// Filling policy (chosen deliberately, see README §缺测补值):
//
//  1. Same-station climatology for the same calendar day (a ±5-day circular
//     day-of-year window over all archived observed years), needing at least
//     three samples. A local 30-year average is the best unbiased estimator
//     for a site and needs no metadata beyond the station's own history.
//  2. Otherwise, same-day observations from nearby stations, elevation
//     lapse-rate corrected (0.65 °C / 100 m) and inverse-squared-distance
//     weighted within 200 km. Those rows are recorded as the fill's basis so a
//     later correction of a basis observation re-triggers the fill.
//  3. Otherwise nearby stations' climatology (same lapse/IDW correction).
//  4. If none of the above exists, the day stays unfilled and accumulation
//     treats it as zero contribution; once any source arrives it is replaced.
//
// Only observed (never filled) rows feed the climatology, so an imputed value
// can never recursively bias the "normal".
package climate

import (
	"math"

	"agriheat/internal/model"
)

const (
	// DoyWindow is the half-width of the day-of-year averaging window.
	DoyWindow = 5
	// MinSamples is the minimum number of archived observed days in-window.
	MinSamples = 3
	// MaxNeighborKm limits which stations may be used as neighbors.
	MaxNeighborKm = 200.0
	// LapseRate is the standard environmental lapse rate, °C per metre of
	// elevation difference (0.65 °C / 100 m).
	LapseRate = 0.0065
	// IDWPow is the inverse-distance weighting exponent.
	IDWPow = 2.0
)

// Normal is a station's climatological expectation for a calendar day.
type Normal struct {
	TMax    float64
	TMin    float64
	Samples int
}

func leapDays(year int) int {
	if (year%4 == 0 && year%100 != 0) || year%400 == 0 {
		return 366
	}
	return 365
}

// doy returns the 1-based day of year.
func doy(d model.Date) int {
	return d.Time().YearDay()
}

// circularDist returns circular distance between two day-of-year indices on a
// year of length n.
func circularDist(a, b, n int) int {
	x := a - b
	if x < 0 {
		x = -x
	}
	if h := n - x; h < x {
		x = h
	}
	return x
}

// ComputeNormals builds a doy-indexed normal table (slice index = doy-1) from
// observed weather rows of one station. Rows from leap and non-leap years are
// aligned by circular day-of-year distance; each sample is compared on the
// year length it belongs to.
func ComputeNormals(obs []model.WeatherDay) []Normal {
	// Aggregate into 366 buckets; bucket 366 only receives Feb-29 samples and
	// is merged circularly when queried.
	sumMax := make([]float64, 366)
	sumMin := make([]float64, 366)
	cnt := make([]int, 366)
	for _, w := range obs {
		if w.Source != model.SourceObs {
			continue
		}
		i := doy(w.Date) - 1
		sumMax[i] += w.TMax
		sumMin[i] += w.TMin
		cnt[i]++
	}

	out := make([]Normal, 366)
	for target := 1; target <= 366; target++ {
		var sx, sn float64
		n := 0
		for i := 0; i < 366; i++ {
			if cnt[i] == 0 {
				continue
			}
			sampleDOY := i + 1
			yearLen := 365
			if sampleDOY == 366 {
				yearLen = 366
			}
			if circularDist(target, sampleDOY, yearLen) <= DoyWindow {
				sx += sumMax[i]
				sn += sumMin[i]
				n += cnt[i]
			}
		}
		if n >= MinSamples {
			out[target-1] = Normal{TMax: sx / float64(n), TMin: sn / float64(n), Samples: n}
		}
	}
	return out
}

// LookupNormal returns the normal for a date and whether one exists.
func LookupNormal(table []Normal, d model.Date) (Normal, bool) {
	i := doy(d) - 1
	if i < 0 || i >= len(table) {
		return Normal{}, false
	}
	n := table[i]
	if n.Samples == 0 {
		return Normal{}, false
	}
	return n, true
}

// haversineKm returns great-circle distance in kilometres.
func haversineKm(lat1, lon1, lat2, lon2 float64) float64 {
	const r = 6371.0
	la1, lo1 := lat1*math.Pi/180, lon1*math.Pi/180
	la2, lo2 := lat2*math.Pi/180, lon2*math.Pi/180
	dla := la2 - la1
	dlo := lo2 - lo1
	a := math.Sin(dla/2)*math.Sin(dla/2) +
		math.Cos(la1)*math.Cos(la2)*math.Sin(dlo/2)*math.Sin(dlo/2)
	return 2 * r * math.Asin(math.Sqrt(a))
}

// elevAdjust corrects a temperature observed at fromElev to the elevation
// toElev using the standard lapse rate (0.65 °C / 100 m). Higher elevations
// are colder, so moving "downhill" to a lower target warms the value.
func elevAdjust(t, fromElev, toElev float64) float64 {
	return t + LapseRate*(toElev-fromElev)
}
