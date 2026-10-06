package engine_test

import (
	"math"
	"math/rand"
	"sort"
	"sync"
	"testing"
	"time"

	"agriheat/internal/climate"
	"agriheat/internal/engine"
	"agriheat/internal/gdd"
	"agriheat/internal/model"
)

// oracle recomputes a plot "from the sowing day with final data only" and
// returns the expected daily rows plus stage dates, using an independent
// implementation from the engine (no snapshots, no incremental state).
//
// Past days use observations/fills exactly as the engine's EnsureFill would
// produce them (deterministic); future days use climatology extrapolation.
type oracleDay struct {
	date   model.Date
	tmax   float64
	tmin   float64
	daily  float64
	cum    float64
	source string
	sid    int64
}

type oracleResult struct {
	days   map[model.Date]oracleDay
	stages map[model.Stage]model.Date
}

func runOracle(w *memWorld, plotID int64, asOf model.Date) oracleResult {
	w.mu.Lock()
	defer w.mu.Unlock()

	plot := w.plots[plotID]
	v := w.varieties[plot.VarietyID]
	end := asOf.AddDays(engine.ForecastHorizon)

	res := oracleResult{days: map[model.Date]oracleDay{}, stages: map[model.Stage]model.Date{}}
	cum := 0.0
	next := 0
	for day := plot.SowingDate; !day.After(end); day = day.AddDays(1) {
		sid, bound := plot.StationAt(day)
		od := oracleDay{date: day, sid: sid}
		if !bound {
			od.source = model.SourceUnbound
		} else {
			isFuture := day.After(asOf)
			var tmax, tmin float64
			source := model.SourceMissing
			if !isFuture {
				if m := w.weather[sid]; m != nil {
					if r, ok := m[day]; ok {
						tmax, tmin, source = r.TMax, r.TMin, r.Source
					}
				}
				if source == model.SourceMissing {
					// Mirror EnsureFill precedence without persisting.
					target := w.stations[sid]
					if n, ok := climate.LookupNormal(w.normalsLocked(sid), day); ok {
						tmax, tmin, source = n.TMax, n.TMin, model.SourceFillStation
					} else {
						obsN := neighborObsLocked(w, sid, day, true)
						normN := neighborObsLocked(w, sid, day, false)
						if tx, tn, _, ok := weightedLocked(w, target, obsN); ok {
							tmax, tmin, source = tx, tn, model.SourceFillNeighbor
						} else if tx, tn, _, ok := weightedLocked(w, target, normN); ok {
							tmax, tmin, source = tx, tn, model.SourceFillNeighbor
						}
					}
				}
			} else {
				target := w.stations[sid]
				if n, ok := climate.LookupNormal(w.normalsLocked(sid), day); ok {
					tmax, tmin, source = n.TMax, n.TMin, model.SourceClimate
				} else {
					normN := neighborObsLocked(w, sid, day, false)
					if tx, tn, _, ok := weightedLocked(w, target, normN); ok {
						tmax, tmin, source = tx, tn, model.SourceClimate
					}
				}
			}
			if source != model.SourceMissing {
				od.tmax, od.tmin = tmax, tmin
				od.daily = gdd.Daily(gdd.MethodSine, tmax, tmin, v.BaseTemp, v.CeilingTemp)
				if od.daily < 0 {
					od.daily = 0
				}
			}
			od.source = source
		}
		cum += od.daily
		od.cum = cum
		res.days[day] = od
		for next < len(model.OrderedStages) {
			st := model.OrderedStages[next]
			if cum < v.StageRequire[st] {
				break
			}
			res.stages[st] = day
			next++
		}
		// Past days always computed; future projection stops at maturity.
		if next == len(model.OrderedStages) && day.After(asOf) {
			break
		}
	}
	return res
}

// neighborObsLocked / weightedLocked reproduce climate package blending using
// world state under the caller-held lock.
func neighborObsLocked(w *memWorld, stationID int64, day model.Date, observed bool) []climate.NeighborDay {
	var out []climate.NeighborDay
	for sid := range w.stations {
		if sid == stationID {
			continue
		}
		s := w.stations[sid]
		if observed {
			if r, ok := w.weather[sid][day]; ok && r.Source == model.SourceObs {
				out = append(out, climate.NeighborDay{StationID: sid, Date: day, TMax: r.TMax, TMin: r.TMin, Elev: s.Elev, Lat: s.Lat, Lon: s.Lon, Observed: true})
			}
		} else {
			if n, ok := climate.LookupNormal(w.normalsLocked(sid), day); ok {
				out = append(out, climate.NeighborDay{StationID: sid, Date: day, TMax: n.TMax, TMin: n.TMin, Elev: s.Elev, Lat: s.Lat, Lon: s.Lon})
			}
		}
	}
	return out
}

func weightedLocked(w *memWorld, target model.Station, cands []climate.NeighborDay) (float64, float64, []model.WeatherBasis, bool) {
	var wsum, sx, sn float64
	var basis []model.WeatherBasis
	for _, c := range cands {
		d := haversine(target.Lat, target.Lon, c.Lat, c.Lon)
		if d > climate.MaxNeighborKm {
			continue
		}
		wt := 1.0 / ((d + 0.5) * (d + 0.5))
		sx += wt * (c.TMax + climate.LapseRate*(target.Elev-c.Elev))
		sn += wt * (c.TMin + climate.LapseRate*(target.Elev-c.Elev))
		wsum += wt
		if c.Observed {
			basis = append(basis, model.WeatherBasis{StationID: c.StationID, Date: c.Date})
		}
	}
	if wsum == 0 {
		return 0, 0, nil, false
	}
	return sx / wsum, sn / wsum, basis, true
}

func haversine(lat1, lon1, lat2, lon2 float64) float64 {
	const r = 6371.0
	rad := func(x float64) float64 { return x * math.Pi / 180 }
	dla := rad(lat2 - lat1)
	dlo := rad(lon2 - lon1)
	a := math.Sin(dla/2)*math.Sin(dla/2) +
		math.Cos(rad(lat1))*math.Cos(rad(lat2))*math.Sin(dlo/2)*math.Sin(dlo/2)
	return 2 * r * math.Asin(math.Sqrt(a))
}

// assertEqOracle compares the engine's persisted plot rows and stages with
// the independent oracle, day by day.
func assertEqOracle(t *testing.T, w *memWorld, plotID int64, asOf model.Date, tag string) {
	t.Helper()
	exp := runOracle(w, plotID, asOf)
	w.mu.Lock()
	defer w.mu.Unlock()

	got := w.plotDays[plotID]
	for d, e := range exp.days {
		g, ok := got[d]
		if !ok {
			t.Fatalf("[%s] missing engine row %s", tag, d)
		}
		if math.Abs(g.CumGDD-e.cum) > 1e-8 {
			t.Fatalf("[%s] cum mismatch %s: engine=%v oracle=%v", tag, d, g.CumGDD, e.cum)
		}
		if math.Abs(g.DailyGDD-e.daily) > 1e-9 {
			t.Fatalf("[%s] daily mismatch %s: engine=%v oracle=%v", tag, d, g.DailyGDD, e.daily)
		}
		if g.Source != e.source {
			t.Fatalf("[%s] source mismatch %s: engine=%s oracle=%s", tag, d, g.Source, e.source)
		}
		if g.StationID != e.sid {
			t.Fatalf("[%s] station mismatch %s: engine=%d oracle=%d", tag, d, g.StationID, e.sid)
		}
	}
	for d := range got {
		if _, ok := exp.days[d]; !ok {
			t.Fatalf("[%s] stale engine row beyond horizon %s", tag, d)
		}
	}
	stageMap := w.stages[plotID]
	for _, st := range model.OrderedStages {
		g := stageMap[st]
		ed, expected := exp.stages[st]
		if !expected {
			if !g.Date.IsZero() {
				t.Fatalf("[%s] stage %s unexpectedly set to %s", tag, st, g.Date)
			}
			continue
		}
		if g.Date.IsZero() || !g.Date.Equal(ed) {
			t.Fatalf("[%s] stage %s: engine=%v oracle=%s", tag, st, g.Date, ed)
		}
		if !ed.After(asOf) && g.Status != "reached" {
			t.Fatalf("[%s] stage %s at %s should be reached, got %s", tag, st, ed, g.Status)
		}
		if ed.After(asOf) && g.Status != "forecast" {
			t.Fatalf("[%s] stage %s at %s should be forecast, got %s", tag, st, ed, g.Status)
		}
	}
}

// ---- tests ---------------------------------------------------------------

func d(y int, m time.Month, day int) model.Date { return model.NewDate(y, m, day) }

func TestExampleReaches12AndStages(t *testing.T) {
	asOf := d(2026, 4, 10)
	w := newMemWorld(asOf)
	w.addStation(1, "S1", 30, 120, 50)
	// Requirements: emergence 12, then increasing.
	w.addVariety(1, 10, 35, []float64{12, 100, 300, 500, 800})
	sowing := d(2026, 4, 1)
	w.addPlot(1, 1, 1, sowing)

	// Every day from sowing to asOf: TMax 30, TMin 14 -> 12 GDD/day.
	for day := sowing; !day.After(asOf); day = day.AddDays(1) {
		w.upsertObs(model.Observation{StationID: 1, Date: day, TMax: 30, TMin: 14, Seq: 1})
	}
	w.enqueue(memTask{plotID: 1, asOf: asOf, start: sowing, changeID: "c0", reason: "seed", initial: true})
	if err := w.drain(); err != nil {
		t.Fatal(err)
	}
	// First day cum == 12 -> emergence reached on the sowing day.
	st := w.stages[1][model.StageEmergence]
	if st.Date.IsZero() || !st.Date.Equal(sowing) || st.Status != "reached" {
		t.Fatalf("emergence = %+v, want reached %s", st, sowing)
	}
	assertEqOracle(t, w, 1, asOf, "12/day")
}

func TestFillReplacedByRealObs(t *testing.T) {
	asOf := d(2026, 6, 30)
	w := newMemWorld(asOf)
	// Station 1 has a rich history so same-station climatology exists.
	w.addStation(1, "S1", 30, 120, 50)
	w.addVariety(1, 10, 35, []float64{50, 200, 500, 800, 1200})
	sowing := d(2026, 5, 1)
	w.addPlot(1, 1, 1, sowing)

	// Same-station climatology history: five prior years of May/June data
	// (well above the 3-sample window threshold).
	for yr := 2021; yr <= 2025; yr++ {
		day0 := d(yr, 5, 1)
		for k := 0; k < 60; k++ {
			day := day0.AddDays(k)
			w.putObs(model.Observation{StationID: 1, Date: day, TMax: 26, TMin: 14, Seq: 1})
		}
	}
	// Current year: observations for May except a single gap day.
	gap := d(2026, 5, 20)
	for k := 0; k < 30; k++ {
		day := sowing.AddDays(k)
		if day.Equal(gap) {
			continue
		}
		w.putObs(model.Observation{StationID: 1, Date: day, TMax: 27, TMin: 15, Seq: 1})
	}
	w.enqueue(memTask{plotID: 1, asOf: asOf, start: sowing, changeID: "init", initial: true})
	if err := w.drain(); err != nil {
		t.Fatal(err)
	}
	// The gap must have been imputed from the station climatology.
	row := w.weather[1][gap]
	if !model.IsFilled(row.Source) {
		t.Fatalf("expected a fill at %s, got source=%q row=%+v", gap, row.Source, row)
	}
	// Real data arrives later with a bigger seq: fill must be replaced.
	changed, kind := w.upsertObs(model.Observation{StationID: 1, Date: gap, TMax: 33, TMin: 21, Seq: 9})
	if !changed || kind != "replace_fill" {
		t.Fatalf("expected replace_fill, got changed=%v kind=%s", changed, kind)
	}
	if got := w.weather[1][gap]; got.Source != model.SourceObs {
		t.Fatalf("fill not replaced: %+v", got)
	}
	w.enqueue(memTask{plotID: 1, asOf: asOf, start: gap, changeID: "real", reason: "real arrives"})
	if err := w.drain(); err != nil {
		t.Fatal(err)
	}
	assertEqOracle(t, w, 1, asOf, "fill replaced")
}

func TestRebindSwitchesStation(t *testing.T) {
	asOf := d(2026, 7, 1)
	w := newMemWorld(asOf)
	w.addStation(1, "A", 30, 120, 50)
	w.addStation(2, "B", 30.5, 120.5, 800)
	w.addVariety(1, 10, 35, []float64{50, 200, 500, 800, 1200})
	sowing := d(2026, 5, 1)
	w.addPlot(1, 1, 1, sowing)
	for day := sowing; !day.After(asOf); day = day.AddDays(1) {
		w.putObs(model.Observation{StationID: 1, Date: day, TMax: 24, TMin: 12, Seq: 1})
		w.putObs(model.Observation{StationID: 2, Date: day, TMax: 32, TMin: 22, Seq: 1})
	}
	w.enqueue(memTask{plotID: 1, asOf: asOf, start: sowing, changeID: "init", initial: true})
	if err := w.drain(); err != nil {
		t.Fatal(err)
	}
	before := w.plotDays[1][d(2026, 6, 15)]

	rebindDay := d(2026, 6, 1)
	w.rebind(1, 2, rebindDay)
	w.enqueue(memTask{plotID: 1, asOf: asOf, start: rebindDay, changeID: "rb", reason: "rebind"})
	if err := w.drain(); err != nil {
		t.Fatal(err)
	}
	after := w.plotDays[1][d(2026, 6, 15)]
	if after.StationID != 2 {
		t.Fatalf("post-rebind day still on station %d", after.StationID)
	}
	if after.DailyGDD <= before.DailyGDD {
		t.Fatalf("hotter neighbor station should raise GDD: %v <= %v", after.DailyGDD, before.DailyGDD)
	}
	if w.plotDays[1][d(2026, 5, 15)].StationID != 1 {
		t.Fatal("pre-rebind day must stay on station 1")
	}
	assertEqOracle(t, w, 1, asOf, "rebind")
}

func TestOutOfOrderAndDuplicateReports(t *testing.T) {
	asOf := d(2026, 5, 5)
	w := newMemWorld(asOf)
	w.addStation(1, "S", 30, 120, 50)
	w.addVariety(1, 10, 35, []float64{20, 100, 300, 500, 800})
	sowing := d(2026, 4, 20)
	w.addPlot(1, 1, 1, sowing)

	day := d(2026, 4, 25)
	// seq 5 arrives first.
	c, k := w.upsertObs(model.Observation{StationID: 1, Date: day, TMax: 25, TMin: 13, Seq: 5})
	if !c || k != "insert" {
		t.Fatalf("first report: %v %s", c, k)
	}
	// seq 3 arrives late: must NOT overwrite.
	c, k = w.upsertObs(model.Observation{StationID: 1, Date: day, TMax: 40, TMin: 30, Seq: 3})
	if c || k != "supersede_ignored" {
		t.Fatalf("stale report applied: %v %s", c, k)
	}
	if r := w.weather[1][day]; r.TMax != 25 {
		t.Fatalf("stale seq overwrote data: %+v", r)
	}
	// seq 5 duplicate also ignored.
	if c, _ := w.upsertObs(model.Observation{StationID: 1, Date: day, TMax: 25, TMin: 13, Seq: 5}); c {
		t.Fatal("duplicate seq applied")
	}
	// seq 8 correction wins.
	c, k = w.upsertObs(model.Observation{StationID: 1, Date: day, TMax: 28, TMin: 16, Seq: 8})
	if !c || k != "correct" {
		t.Fatalf("correction: %v %s", c, k)
	}
	if r := w.weather[1][day]; r.TMax != 28 {
		t.Fatalf("correction not applied: %+v", r)
	}
}

func TestSameChangeNoDuplicateEvents(t *testing.T) {
	asOf := d(2026, 5, 20)
	w := newMemWorld(asOf)
	w.addStation(1, "S", 30, 120, 50)
	w.addVariety(1, 10, 35, []float64{20, 100, 300, 500, 800})
	sowing := d(2026, 5, 1)
	w.addPlot(1, 1, 1, sowing)

	// Initial run: forecast emergence a few days out with climate missing ->
	// then feed observations day by day and force a stage move.
	day := sowing
	w.upsertObs(model.Observation{StationID: 1, Date: day, TMax: 30, TMin: 14, Seq: 1})
	w.enqueue(memTask{plotID: 1, asOf: asOf, start: sowing, changeID: "init", initial: true})
	if err := w.drain(); err != nil {
		t.Fatal(err)
	}
	// Add more warm days triggering jointing; run the SAME change id twice.
	for k := 1; k <= 10; k++ {
		w.upsertObs(model.Observation{StationID: 1, Date: sowing.AddDays(k), TMax: 30, TMin: 14, Seq: 1})
	}
	for i := 0; i < 2; i++ {
		// Reusing one change id must not duplicate events.
		_, err := engine.Recompute(w.db(), engine.Request{
			PlotID: 1, AsOf: asOf, Start: sowing, ChangeID: "same", Reason: "x",
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	count := map[model.Stage]int{}
	for _, e := range w.events {
		if e.changeID == "same" {
			count[e.stage]++
		}
	}
	for st, n := range count {
		if n > 1 {
			t.Fatalf("duplicate event for %s: %d", st, n)
		}
	}
}

// TestRandomCorrections is the centerpiece: many random乱序/补传/更正 sequences
// must leave the incremental engine day-by-day identical to a full recompute
// from sowing with final data.
func TestRandomCorrections(t *testing.T) {
	rng := rand.New(rand.NewSource(20261001))
	for iter := 0; iter < 40; iter++ {
		asOf := d(2026, 6, 30)
		w := newMemWorld(asOf)
		w.addStation(1, "S", 30, 120, 50)
		w.addVariety(1, 10, 35, []float64{60, 250, 600, 900, 1400})
		sowing := d(2026, 4, 1)
		w.addPlot(1, 1, 1, sowing)

		nDays := asOf.Sub(sowing) + 1
		seqAt := make([]int64, nDays)
		// Pre-create initial run.
		w.enqueue(memTask{plotID: 1, asOf: sowing, start: sowing, changeID: "init", initial: true})
		if err := w.drain(); err != nil {
			t.Fatal(err)
		}

		curAsOf := sowing
		for step := 0; step < 120; step++ {
			idx := rng.Intn(nDays)
			day := sowing.AddDays(idx)
			// Random out-of-order seq: sometimes smaller (ignored), sometimes bigger.
			var seq int64
			if rng.Intn(3) == 0 {
				seq = seqAt[idx] - int64(1+rng.Intn(3))
				if seq < 0 {
					seq = 0
				}
			} else {
				seqAt[idx] += int64(1 + rng.Intn(4))
				seq = seqAt[idx]
			}
			tx := 18 + rng.Float64()*20
			tn := tx - 4 - rng.Float64()*10
			o := model.Observation{StationID: 1, Date: day, TMax: tx, TMin: tn, Seq: seq}
			changed, _ := w.upsertObs(o)
			// Occasionally advance "today".
			if rng.Intn(4) == 0 && curAsOf.Before(asOf) {
				curAsOf = curAsOf.AddDays(1 + rng.Intn(5))
				if curAsOf.After(asOf) {
					curAsOf = asOf
				}
			}
			if changed {
				w.enqueue(memTask{plotID: 1, asOf: curAsOf, start: day,
					changeID: randomChangeID(rng, step, iter), reason: "random"})
				if rng.Intn(2) == 0 {
					if err := w.drain(); err != nil {
						t.Fatal(err)
					}
				}
			}
		}
		if err := w.drain(); err != nil {
			t.Fatal(err)
		}
		// Simulate the end-of-day timer: roll every plot forward to the final
		// asOf so newly arrived observations replace earlier climate rows.
		w.enqueue(memTask{plotID: 1, asOf: asOf, start: sowing,
			changeID: randomChangeID(rng, 999_000, iter), reason: "roll-forward"})
		if err := w.drain(); err != nil {
			t.Fatal(err)
		}
		assertEqOracle(t, w, 1, asOf, "random")
	}
}

func randomChangeID(rng *rand.Rand, step, iter int) string {
	return "r" + itoa(iter) + "-" + itoa(step) + "-" + itoa(rng.Intn(1<<20))
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [24]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

func TestConcurrentReportsKeepLargestSeq(t *testing.T) {
	asOf := d(2026, 5, 5)
	w := newMemWorld(asOf)
	w.addStation(1, "S", 30, 120, 50)
	w.addVariety(1, 10, 35, []float64{20, 100, 300, 500, 800})
	sowing := d(2026, 4, 20)
	w.addPlot(1, 1, 1, sowing)

	day := d(2026, 4, 25)
	var wg sync.WaitGroup
	for seq := int64(1); seq <= 100; seq++ {
		wg.Add(1)
		go func(seq int64) {
			defer wg.Done()
			// Larger seq carries a distinctive tmax; stale seqs must lose.
			tmax := 15.0 + float64(seq)
			w.upsertObs(model.Observation{StationID: 1, Date: day, TMax: tmax, TMin: 10, Seq: seq})
		}(seq)
	}
	wg.Wait()
	row := w.weather[1][day]
	if row.Seq != 100 || row.TMax != 115 {
		t.Fatalf("concurrent reports: final row seq=%d tmax=%v, want 100/115", row.Seq, row.TMax)
	}
}

func TestTaskMergeComputesOnce(t *testing.T) {
	asOf := d(2026, 5, 30)
	w := newMemWorld(asOf)
	w.addStation(1, "S", 30, 120, 50)
	w.addVariety(1, 10, 35, []float64{60, 250, 600, 900, 1400})
	sowing := d(2026, 5, 1)
	w.addPlot(1, 1, 1, sowing)

	for k := 0; k < 20; k++ {
		day := sowing.AddDays(k)
		w.upsertObs(model.Observation{StationID: 1, Date: day, TMax: 30, TMin: 14, Seq: 1})
	}
	// Two batches arrive "concurrently": both enqueue before the worker runs.
	w.mergeOrEnqueue(memTask{plotID: 1, asOf: asOf, start: sowing, changeID: "b1", reason: "batch1"})
	w.mergeOrEnqueue(memTask{plotID: 1, asOf: asOf, start: sowing.AddDays(10), changeID: "b2", reason: "batch2"})
	if n := len(w.pending()); n != 1 {
		t.Fatalf("expected 1 merged pending task, got %d", n)
	}
	if err := w.drain(); err != nil {
		t.Fatal(err)
	}
	assertEqOracle(t, w, 1, asOf, "merge")
}

func TestRestartResumesConsistently(t *testing.T) {
	asOf := d(2026, 6, 15)
	w := newMemWorld(asOf)
	w.addStation(1, "S", 30, 120, 50)
	w.addVariety(1, 10, 35, []float64{60, 250, 600, 900, 1400})
	sowing := d(2026, 4, 1)
	w.addPlot(1, 1, 1, sowing)
	for day := sowing; !day.After(asOf); day = day.AddDays(1) {
		w.upsertObs(model.Observation{StationID: 1, Date: day, TMax: 26 + float64(day.Day()%5), TMin: 12, Seq: 1})
	}
	w.enqueue(memTask{plotID: 1, asOf: asOf, start: sowing, changeID: "init", initial: true})

	// Crash twice mid-claim; processing tasks must become pending on restart.
	for i := 0; i < 2; i++ {
		ran, err := w.runOne(true)
		if err != nil || !ran {
			t.Fatalf("crash run: ran=%v err=%v", ran, err)
		}
		w.recoverProcessing()
	}
	if n := len(w.pending()); n != 1 {
		t.Fatalf("expected recovered pending task, got %d", n)
	}
	if err := w.drain(); err != nil {
		t.Fatal(err)
	}
	assertEqOracle(t, w, 1, asOf, "restart")
}

func TestSnapshotReuseReducesRewrittenWindow(t *testing.T) {
	asOf := d(2026, 7, 31)
	w := newMemWorld(asOf)
	w.addStation(1, "S", 30, 120, 50)
	w.addVariety(1, 10, 35, []float64{6000, 9000, 12000, 15000, 20000}) // rarely reached
	sowing := d(2026, 1, 1)
	w.addPlot(1, 1, 1, sowing)
	for day := sowing; !day.After(asOf); day = day.AddDays(1) {
		w.upsertObs(model.Observation{StationID: 1, Date: day, TMax: 25, TMin: 12, Seq: 1})
	}
	w.enqueue(memTask{plotID: 1, asOf: asOf, start: sowing, changeID: "init", initial: true})
	if err := w.drain(); err != nil {
		t.Fatal(err)
	}
	// Month-end snapshots must exist.
	snaps := []model.Date{}
	for d0 := range w.snapshots[1] {
		snaps = append(snaps, d0)
	}
	sort.Slice(snaps, func(i, j int) bool { return snaps[i].Before(snaps[j]) })
	if len(snaps) < 6 {
		t.Fatalf("expected month-end snapshots, got %d: %v", len(snaps), snaps)
	}

	// Correct a July day: recompute must start after the June snapshot.
	day := d(2026, 7, 10)
	w.upsertObs(model.Observation{StationID: 1, Date: day, TMax: 38, TMin: 24, Seq: 5})
	res, err := engine.Recompute(w.db(), engine.Request{
		PlotID: 1, AsOf: asOf, Start: day, ChangeID: "july", Reason: "july correction",
	})
	if err != nil {
		t.Fatal(err)
	}
	july1 := d(2026, 7, 1)
	if res.From.Before(july1) {
		t.Fatalf("recompute rewrote from %s, expected on/after %s (snapshot reuse failed)", res.From, july1)
	}
	assertEqOracle(t, w, 1, asOf, "snapshot reuse")
}
