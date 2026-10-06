package climate

import (
	"agriheat/internal/model"
)

// NeighborDay is one candidate value usable for filling a target station/day.
type NeighborDay struct {
	StationID int64
	Date      model.Date
	TMax      float64
	TMin      float64
	Elev      float64
	Lat       float64
	Lon       float64
	// Observed is true for a real same-day observation, false for a climatology
	// value of the neighbor.
	Observed bool
}

// Estimate is the result of a fill attempt.
type Estimate struct {
	TMax   float64
	TMin   float64
	Source string
	Basis  []model.WeatherBasis
	Found  bool
}

// weightedMean lapse-corrects candidates to target elevation and blends them
// with inverse-squared-distance weights. Returns ok=false when no candidate
// is within MaxNeighborKm or all collapse to the target itself.
func weightedMean(targetLat, targetLon, targetElev float64, cands []NeighborDay) (tmax, tmin float64, basis []model.WeatherBasis, ok bool) {
	var wsum, sx, sn float64
	for _, c := range cands {
		d := haversineKm(targetLat, targetLon, c.Lat, c.Lon)
		if d > MaxNeighborKm {
			continue
		}
		// Avoid divide-by-zero for a co-located station: give it dominant but
		// finite weight.
		w := 1.0 / ((d + 0.5) * (d + 0.5))
		if IDWPow != 2.0 {
			w = 1.0
			for i := 0; i < int(IDWPow); i++ {
				w /= d + 0.5
			}
		}
		sx += w * elevAdjust(c.TMax, c.Elev, targetElev)
		sn += w * elevAdjust(c.TMin, c.Elev, targetElev)
		wsum += w
		if c.Observed {
			basis = append(basis, model.WeatherBasis{StationID: c.StationID, Date: c.Date})
		}
	}
	if wsum == 0 {
		return 0, 0, nil, false
	}
	return sx / wsum, sn / wsum, basis, true
}

// EstimateFill fills one missing station/day.
//
// ownNormal is the target station's climatology table (may be nil).
// obsNeighbors are same-date observed values at other stations.
// normalNeighbors are candidates carrying each neighbor's climatology for the
// target date (Observed=false). Precedence follows the package doc.
func EstimateFill(
	target model.Station,
	day model.Date,
	ownNormal []Normal,
	obsNeighbors, normalNeighbors []NeighborDay,
) Estimate {
	if n, ok := LookupNormal(ownNormal, day); ok {
		return Estimate{TMax: n.TMax, TMin: n.TMin, Source: model.SourceFillStation, Found: true}
	}
	if tx, tn, basis, ok := weightedMean(target.Lat, target.Lon, target.Elev, obsNeighbors); ok {
		return Estimate{TMax: tx, TMin: tn, Source: model.SourceFillNeighbor, Basis: basis, Found: true}
	}
	if tx, tn, _, ok := weightedMean(target.Lat, target.Lon, target.Elev, normalNeighbors); ok {
		// No observed basis: the normal itself moves only when its underlying
		// observations change, which triggers a normals refresh + recompute.
		return Estimate{TMax: tx, TMin: tn, Source: model.SourceFillNeighbor, Found: true}
	}
	return Estimate{}
}

// EstimateClimate returns the value used to extrapolate a FUTURE day for a
// plot (never persisted as a station row). Preference is the bound station's
// own normal, then neighbor normals.
func EstimateClimate(
	target model.Station,
	day model.Date,
	ownNormal []Normal,
	normalNeighbors []NeighborDay,
) (tmax, tmin float64, found bool) {
	if n, ok := LookupNormal(ownNormal, day); ok {
		return n.TMax, n.TMin, true
	}
	if tx, tn, _, ok := weightedMean(target.Lat, target.Lon, target.Elev, normalNeighbors); ok {
		return tx, tn, true
	}
	return 0, 0, false
}
