package climate

import (
	"math"
	"testing"
	"time"

	"agriheat/internal/model"
)

func makeObs(stationID int64, y, m, d, tx, tn float64) model.WeatherDay {
	return model.WeatherDay{
		StationID: int64(stationID),
		Date:      mustDate(int(y), int(m), int(d)),
		TMax:      tx,
		TMin:      tn,
		Source:    model.SourceObs,
	}
}

func mustDate(y, m, d int) model.Date {
	return model.NewDate(y, time.Month(m), d)
}

// TestStationNormalFirst: same-station climatology wins over neighbors and is
// tagged fill_station.
func TestStationNormalFirst(t *testing.T) {
	target := model.Station{ID: 1, Lat: 30, Lon: 120, Elev: 100}
	day := mustDate(2026, 5, 20)
	// 4 prior-year observations near the same doy.
	obs := []model.WeatherDay{
		makeObs(1, 2022, 5, 20, 26, 14),
		makeObs(1, 2023, 5, 19, 28, 16),
		makeObs(1, 2024, 5, 21, 24, 12),
		makeObs(1, 2025, 5, 20, 26, 14),
	}
	tab := ComputeNormals(obs)
	est := EstimateFill(target, day, tab, nil, nil)
	if !est.Found || est.Source != model.SourceFillStation {
		t.Fatalf("want fill_station, got %+v", est)
	}
	wantMax := (26 + 28 + 24 + 26) / 4.0
	if math.Abs(est.TMax-wantMax) > 1e-9 {
		t.Fatalf("normal tmax %v want %v", est.TMax, wantMax)
	}
}

// TestNeighborElevLapse: a neighbor 100 m lower and co-located should yield a
// target temperature raised by 0.65 °C (lapse correction).
func TestNeighborElevLapse(t *testing.T) {
	target := model.Station{ID: 1, Lat: 30, Lon: 120, Elev: 200}
	day := mustDate(2026, 5, 20)
	neighbors := []NeighborDay{{
		StationID: 2, Date: day, TMax: 20, TMin: 10,
		Elev: 100, Lat: 30, Lon: 120, Observed: true,
	}}
	est := EstimateFill(target, day, nil, neighbors, nil)
	if !est.Found || est.Source != model.SourceFillNeighbor {
		t.Fatalf("want fill_neighbor, got %+v", est)
	}
	if math.Abs(est.TMax-20.65) > 1e-9 {
		t.Fatalf("lapse tmax %v want 20.65", est.TMax)
	}
	if len(est.Basis) != 1 || est.Basis[0].StationID != 2 {
		t.Fatalf("basis must cite observed neighbor: %+v", est.Basis)
	}
}

// TestNeighborTooFarIgnored: beyond 200 km there is no fill.
func TestNeighborTooFarIgnored(t *testing.T) {
	target := model.Station{ID: 1, Lat: 30, Lon: 120, Elev: 100}
	day := mustDate(2026, 5, 20)
	neighbors := []NeighborDay{{
		StationID: 2, Date: day, TMax: 20, TMin: 10,
		Elev: 100, Lat: 35, Lon: 120, Observed: true, // ~555 km north
	}}
	est := EstimateFill(target, day, nil, neighbors, nil)
	if est.Found {
		t.Fatalf("far neighbor must not fill, got %+v", est)
	}
}

// TestIDWWeights: a closer neighbor dominates the blend.
func TestIDWWeights(t *testing.T) {
	target := model.Station{ID: 1, Lat: 30, Lon: 120, Elev: 100}
	day := mustDate(2026, 5, 20)
	cands := []NeighborDay{
		{StationID: 2, Date: day, TMax: 10, TMin: 0, Elev: 100, Lat: 30.5, Lon: 120, Observed: true},  // ~55 km
		{StationID: 3, Date: day, TMax: 30, TMin: 20, Elev: 100, Lat: 31.5, Lon: 120, Observed: true}, // ~166 km
	}
	est := EstimateFill(target, day, nil, cands, nil)
	if !est.Found {
		t.Fatal("expected a fill")
	}
	// The close (cold) station must pull the blend well below the arithmetic mean.
	if est.TMax >= 20 {
		t.Fatalf("IDW did not favor the close station: tmax=%v", est.TMax)
	}
}

// TestFilledRowsExcludedFromNormal: an imputed value must never feed the
// climatology (otherwise fills bias the normals).
func TestFilledRowsExcludedFromNormal(t *testing.T) {
	obs := []model.WeatherDay{
		makeObs(1, 2023, 5, 20, 26, 14),
		makeObs(1, 2024, 5, 20, 28, 16),
		makeObs(1, 2025, 5, 20, 26, 14),
	}
	// Inject an extreme fill that, if included, would dominate the average.
	fill := model.WeatherDay{
		StationID: 1, Date: mustDate(2026, 5, 20), TMax: 80, TMin: 60,
		Source: model.SourceFillStation,
	}
	tab := ComputeNormals(append(obs, fill))
	n, ok := LookupNormal(tab, mustDate(2026, 5, 20))
	if !ok {
		t.Fatal("expected a normal from observed samples")
	}
	if n.TMax >= 30 || n.Samples != 3 {
		t.Fatalf("fill leaked into normal: %+v", n)
	}
}

// TestFutureClimateUsesNormal: EstimateClimate prefers the own normal.
func TestFutureClimateUsesNormal(t *testing.T) {
	target := model.Station{ID: 1, Lat: 30, Lon: 120, Elev: 100}
	day := mustDate(2026, 7, 20)
	obs := []model.WeatherDay{
		makeObs(1, 2023, 7, 20, 34, 24),
		makeObs(1, 2024, 7, 20, 36, 26),
		makeObs(1, 2025, 7, 20, 32, 22),
	}
	tab := ComputeNormals(obs)
	tx, tn, ok := EstimateClimate(target, day, tab, nil)
	if !ok {
		t.Fatal("expected future climate")
	}
	if math.Abs(tx-34) > 1e-9 || math.Abs(tn-24) > 1e-9 {
		t.Fatalf("climate %v/%v want 34/24", tx, tn)
	}
}
