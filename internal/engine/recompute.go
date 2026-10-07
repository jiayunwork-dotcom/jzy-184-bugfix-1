package engine

import (
	"fmt"
	"time"

	"agriheat/internal/climate"
	"agriheat/internal/events"
	"agriheat/internal/gdd"
	"agriheat/internal/model"
	"agriheat/internal/phenology"
)

// Request drives one plot recomputation.
type Request struct {
	PlotID int64
	AsOf   model.Date // frozen "today": last past day and forecast origin
	// Start is the earliest day whose inputs may have changed; the walk starts
	// after the newest snapshot strictly before Start. Zero means AsOf.
	Start    model.Date
	ChangeID string // unique per data change; dedupes events
	Reason   string
	// Initial suppresses events (first computation of a new plot).
	Initial bool
}

// Result reports what the recompute did.
type Result struct {
	PlotID      int64
	Stages      []model.StageState
	Events      int
	DaysWritten int
	From        model.Date
}

// Recompute runs an incremental recomputation for one plot inside a fresh
// transaction. See README §增量重算与快照 for the checkpoint scheme.
func Recompute(d DB, req Request) (*Result, error) {
	tx, err := d.BeginTx()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	res, err := Run(tx, req)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return res, nil
}

// Run executes the recomputation inside an existing transaction. The caller
// owns commit/rollback.
func Run(tx Tx, req Request) (*Result, error) {
	plot, err := tx.GetPlot(req.PlotID)
	if err != nil {
		return nil, err
	}
	variety, err := tx.GetVariety(plot.VarietyID)
	if err != nil {
		return nil, err
	}

	// Stages that have already crossed before the walk window are carried
	// forward from persisted state: their crossing day is at or before the
	// chosen snapshot, so inputs strictly after the snapshot cannot move them.
	oldStates, err := tx.ListStages(plot.ID)
	if err != nil {
		return nil, err
	}
	oldByStage := make(map[model.Stage]model.StageState, len(oldStates))
	for _, s := range oldStates {
		oldByStage[s.Stage] = s
	}

	// Choose the latest checkpoint strictly before the first potentially
	// changed day (falling back to the sowing day, cumulative zero).
	startDay := req.Start
	if startDay.IsZero() {
		startDay = req.AsOf
	}
	snap, hasSnap, err := tx.LatestSnapshot(plot.ID, startDay.AddDays(-1))
	if err != nil {
		return nil, err
	}
	// A snapshot holds the cumulative GDD AT END of its date. With none prior
	// to sowing, start from an implicit zero checkpoint on the day before
	// sowing so the sowing day itself is included in the walk.
	if !hasSnap || snap.Date.Before(plot.SowingDate) {
		snap = model.Snapshot{PlotID: plot.ID, Date: plot.SowingDate.AddDays(-1), CumGDD: 0}
	}
	start := snap.Date.AddDays(1)
	cum := snap.CumGDD

	// Seed crossings already achieved at/before the snapshot.
	crossed := make(map[model.Stage]phenology.StageResult)
	nextStage := 0
	for _, st := range model.OrderedStages {
		s, ok := oldByStage[st]
		if ok && !s.Date.IsZero() && !s.Date.After(snap.Date) {
			crossed[st] = phenology.StageResult{Stage: st, Status: s.Status, Date: s.Date, CumAt: s.CumAtDay}
			nextStage++
		} else {
			break
		}
	}

	// Sowing day is a valid month-end checkpoint only rarely; the month-end
	// loop below creates it when applicable. Nothing extra to seed here.

	// Maturity at/before the snapshot: no later input can change any stage.
	// Keep existing later checkpoints and daily rows untouched.
	if nextStage == len(model.OrderedStages) {
		return &Result{PlotID: plot.ID, Stages: buildStates(variety, crossed), From: snap.Date}, nil
	}
	if err := tx.ResetSnapshots(plot.ID, snap.Date); err != nil {
		return nil, err
	}

	// Station metadata + climatology cache.
	stCache := make(map[int64]stationPack)
	getStationPack := func(id int64) (stationPack, error) {
		if p, ok := stCache[id]; ok {
			return p, nil
		}
		st, err := tx.GetStation(id)
		if err != nil {
			return stationPack{}, err
		}
		tab, err := tx.Normals(id)
		if err != nil {
			return stationPack{}, err
		}
		p := stationPack{station: st, normals: tab}
		stCache[id] = p
		return p, nil
	}

	end := req.AsOf.AddDays(ForecastHorizon)
	if end.Before(start) {
		end = start
	}
	days := make([]model.PlotDay, 0, req.AsOf.Sub(start)+ForecastHorizon+1)

	// Checkpoint eligibility. A month-end snapshot stores a season-total
	// cumulative, so it is valid "truth" for a later incremental recompute
	// only when every day from the start of this walk up to that month-end
	// carried known data (obs or fill). When the walk begins from a real
	// snapshot, days before it are not re-walked (snapshot reuse); when it
	// begins from the synthetic sowing-eve zero it covers the whole season.
	//
	// Why this matters: the registration baseline usually runs against a
	// station with no data yet, so its walked days are tagged missing (0
	// heat). Persisting month-end checkpoints there would record cum as if
	// the month truly accumulated nothing; a later recompute reusing such a
	// checkpoint would skip the very month in which observations then arrived
	// — the "fast batch" rhythm produced wrong/empty stage dates that way.
	// spanHasMissing is set by the first missing walked day and is never
	// reset within the run, withholding that month-end and every later one.
	spanHasMissing := false
	markSpan := func(pd model.PlotDay) {
		if pd.Source == model.SourceMissing || pd.Source == model.SourceUnbound {
			spanHasMissing = true
		}
	}

	dayFn := func(day model.Date) (model.PlotDay, error) {
		pd := model.PlotDay{Date: day}
		isFuture := day.After(req.AsOf)
		stationID, bound := plot.StationAt(day)
		if !bound {
			pd.Source = model.SourceUnbound
			return pd, nil
		}
		pd.StationID = stationID
		pack, err := getStationPack(stationID)
		if err != nil {
			return pd, err
		}
		var tmax, tmin float64
		var source string
		if isFuture {
			nn, err := tx.NeighborNormals(stationID, day)
			if err != nil {
				return pd, err
			}
			if t2, n2, ok := climate.EstimateClimate(pack.station, day, pack.normals, nn); ok {
				tmax, tmin, source = t2, n2, model.SourceClimate
			} else {
				source = model.SourceMissing
			}
		} else if w, ok, err := tx.Weather(stationID, day); err != nil {
			return pd, err
		} else if ok {
			tmax, tmin, source = w.TMax, w.TMin, w.Source
		} else {
			// Past but missing: impute and persist the fill. A later real
			// report replaces it automatically (obs always wins in UpsertObs).
			w, found, err := tx.EnsureFill(stationID, day)
			if err != nil {
				return pd, err
			}
			if found {
				tmax, tmin, source = w.TMax, w.TMin, w.Source
			} else {
				source = model.SourceMissing
			}
		}
		if source != model.SourceMissing && source != model.SourceUnbound {
			pd.TMax, pd.TMin = tmax, tmin
			pd.DailyGDD = gdd.Daily(Method, tmax, tmin, variety.BaseTemp, variety.CeilingTemp)
			if pd.DailyGDD < 0 {
				pd.DailyGDD = 0
			}
		}
		pd.Source = source
		return pd, nil
	}

	record := func(day model.Date, pd model.PlotDay) {
		cum += pd.DailyGDD
		pd.CumGDD = cum
		days = append(days, pd)
		for nextStage < len(model.OrderedStages) {
			st := model.OrderedStages[nextStage]
			if cum < variety.StageRequire[st] {
				break
			}
			status := phenology.StatusForecast
			if !day.After(req.AsOf) {
				status = phenology.StatusReached
			}
			crossed[st] = phenology.StageResult{Stage: st, Status: status, Date: day, CumAt: cum}
			nextStage++
		}
		// Checkpoints only on settled past month-ends, and only while every day
		// walked so far (back to sowing or the last real checkpoint) carried
		// known data. Once a missing day has appeared, cum no longer reflects
		// true accumulated heat and neither this nor any later month-end may be
		// checkpointed; the flag is deliberately not reset per month.
		if !day.After(req.AsOf) {
			markSpan(pd)
			if isMonthEnd(day) && !spanHasMissing {
				_ = tx.UpsertSnapshot(model.Snapshot{PlotID: plot.ID, Date: day, CumGDD: cum})
			}
		}
	}

	// Past window: walk through AsOf even if maturity passed, keeping the
	// stored observed daily curve and checkpoints exact.
	if start.Before(req.AsOf.AddDays(1)) {
		for day := start; !day.After(req.AsOf); day = day.AddDays(1) {
			pd, err := dayFn(day)
			if err != nil {
				return nil, err
			}
			record(day, pd)
		}
	}
	// Future window: project with climatology up to maturity or horizon.
	if nextStage < len(model.OrderedStages) && req.AsOf.Before(end) {
		for day := req.AsOf.AddDays(1); !day.After(end); day = day.AddDays(1) {
			pd, err := dayFn(day)
			if err != nil {
				return nil, err
			}
			record(day, pd)
			if nextStage == len(model.OrderedStages) {
				break
			}
		}
	}

	if err := tx.ReplacePlotDays(plot.ID, start, days); err != nil {
		return nil, err
	}

	newStates := buildStates(variety, crossed)
	oldDate := make(map[model.Stage]model.Date, len(oldStates))
	oldKnown := make(map[model.Stage]bool, len(oldStates))
	for _, s := range oldStates {
		if !s.Date.IsZero() {
			oldDate[s.Stage] = s.Date
			oldKnown[s.Stage] = true
		}
	}
	drafts := make([]events.Draft, 0, len(newStates))
	for _, s := range newStates {
		drafts = append(drafts, events.Draft{
			Stage: s.Stage, Status: s.Status, Date: s.Date, CumAt: s.CumAtDay,
		})
	}
	changes := events.Diff(plot.ID, drafts, oldDate, oldKnown, req.Reason)

	emitted := 0
	if !req.Initial {
		for _, c := range changes {
			inserted, err := tx.InsertEvent(plot.ID, c.Stage, c.OldDate, c.NewDate, c.Reason, req.ChangeID)
			if err != nil {
				return nil, err
			}
			if inserted {
				emitted++
			}
		}
	}
	if err := tx.SaveStages(plot.ID, newStates); err != nil {
		return nil, err
	}
	return &Result{PlotID: plot.ID, Stages: newStates, Events: emitted, DaysWritten: len(days), From: start}, nil
}

type stationPack struct {
	station model.Station
	normals []climate.Normal
}

func buildStates(v *model.Variety, crossed map[model.Stage]phenology.StageResult) []model.StageState {
	out := make([]model.StageState, 0, len(model.OrderedStages))
	for _, st := range model.OrderedStages {
		s := model.StageState{Stage: st, NameCN: model.StageCN[st], Status: phenology.StatusForecast, Require: v.StageRequire[st]}
		if r, ok := crossed[st]; ok {
			s.Status = r.Status
			s.Date = r.Date
			s.CumAtDay = r.CumAt
		}
		out = append(out, s)
	}
	return out
}

// isMonthEnd reports whether day is the last day of its month.
func isMonthEnd(d model.Date) bool {
	next := d.Time().AddDate(0, 0, 1)
	return next.Month() != d.Month()
}

// ChangeID builds the deterministic event dedupe key for a data change.
func ChangeID(kind string, stationID int64, day model.Date, at time.Time) string {
	if stationID != 0 {
		return fmt.Sprintf("%s:%d:%s:%d", kind, stationID, day, at.UnixNano())
	}
	return fmt.Sprintf("%s:%s:%d", kind, day, at.UnixNano())
}
