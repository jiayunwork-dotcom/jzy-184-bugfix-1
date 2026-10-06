// Package service orchestrates writes: validating batches, applying the
// largest-seq-wins observation rule, refreshing climatology and imputed rows,
// and enqueuing durable recompute tasks for every affected plot.
package service

import (
	"context"
	"fmt"
	"sort"
	"time"

	"agriheat/internal/model"
	"agriheat/internal/store"
	"agriheat/internal/validate"
)

// Clock yields the "current" date; injectable for deterministic tests.
type Clock func() model.Date

// Service wires the store together for write operations.
type Service struct {
	DB    *store.DB
	Now   Clock
	NowTS func() time.Time
}

// New constructs a Service using wall-clock time.
func New(db *store.DB) *Service {
	return &Service{
		DB:    db,
		Now:   model.Today,
		NowTS: func() time.Time { return time.Now().UTC() },
	}
}

// ObservationIn is one row of a batch ingest request.
type ObservationIn struct {
	StationCode string  `json:"station_code"`
	StationID   int64   `json:"station_id"`
	Date        string  `json:"date"`
	TMax        float64 `json:"tmax"`
	TMin        float64 `json:"tmin"`
	Seq         int64   `json:"seq"`
}

// RowResult reports the per-row outcome.
type RowResult struct {
	Index   int    `json:"index"`
	OK      bool   `json:"ok"`
	Reason  string `json:"reason,omitempty"`
	Applied bool   `json:"applied,omitempty"` // true if it changed stored data
	Kind    string `json:"kind,omitempty"`    // insert | correct | replace_fill | supersede_ignored
}

// BatchResult is the batch response.
type BatchResult struct {
	Accepted int         `json:"accepted"`
	Ignored  int         `json:"ignored"`
	Enqueued int         `json:"enqueued_plots"`
	Results  []RowResult `json:"results"`
	ChangeID string      `json:"change_id"`
}

// IngestObservations validates and applies a batch. Illegal rows never block
// legal ones; each row gets a result with a reason.
func (s *Service) IngestObservations(ctx context.Context, in []ObservationIn) (*BatchResult, error) {
	res := &BatchResult{Results: make([]RowResult, len(in))}
	type parsed struct {
		idx int
		obs model.Observation
	}
	valid := make([]parsed, 0, len(in))
	// Collapse duplicate (station,date) rows inside one batch to the largest
	// seq; smaller-seq late rows are reported as supersede_ignored and must
	// never overwrite newer data.
	dupKey := map[string]int{}

	// Phase 1: resolve codes and validate; never touch the DB for bad rows.
	for i, r := range in {
		res.Results[i].Index = i
		if errs := validate.CheckTemp(r.TMax, r.TMin); len(errs) > 0 {
			res.Results[i].Reason = errs[0].Error()
			res.Ignored++
			continue
		}
		day, err := model.ParseDate(r.Date)
		if err != nil {
			res.Results[i].Reason = "date: must be YYYY-MM-DD"
			res.Ignored++
			continue
		}
		if day.After(s.Now()) {
			res.Results[i].Reason = "date: future observations are not accepted"
			res.Ignored++
			continue
		}
		sid := r.StationID
		if sid == 0 && r.StationCode != "" {
			if id, err := s.resolveStation(ctx, r.StationCode); err != nil {
				res.Results[i].Reason = err.Error()
				res.Ignored++
				continue
			} else {
				sid = id
			}
		}
		if sid == 0 {
			res.Results[i].Reason = "station_id or station_code is required"
			res.Ignored++
			continue
		}
		if r.Seq < 0 {
			res.Results[i].Reason = "seq: must be >= 0"
			res.Ignored++
			continue
		}
		key := fmt.Sprintf("%d|%s", sid, day)
		if prev, ok := dupKey[key]; ok {
			if valid[prev].obs.Seq >= r.Seq {
				res.Results[i].OK = true
				res.Results[i].Kind = "supersede_ignored"
				res.Ignored++
				continue
			}
			// newer row wins: mark the earlier one ignored
			res.Results[valid[prev].idx].OK = true
			res.Results[valid[prev].idx].Kind = "supersede_ignored"
			res.Results[valid[prev].idx].Applied = false
			valid[prev] = parsed{idx: i, obs: model.Observation{
				StationID: sid, Date: day, TMax: r.TMax, TMin: r.TMin, Seq: r.Seq,
			}}
			continue
		}
		idx := len(valid)
		dupKey[key] = idx
		valid = append(valid, parsed{idx: i, obs: model.Observation{
			StationID: sid, Date: day, TMax: r.TMax, TMin: r.TMin, Seq: r.Seq,
		}})
	}

	if len(valid) == 0 {
		return res, nil
	}

	// A single change id for the whole batch: one data-change event per stage,
	// no duplicates even when several rows of the batch touch one plot.
	changeID := fmt.Sprintf("ingest:%d", s.NowTS().UnixNano())
	asOf := s.Now()
	// Earliest affected day per station (observation changes and cascaded
	// fill invalidations).
	earliestAtStation := map[int64]model.Date{}
	changedStations := map[int64]struct{}{}

	// Phase 2: serialize the whole batch in one transaction; row locks inside
	// UpsertObs make concurrent batches safe.
	t, err := s.DB.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer t.Rollback()

	for _, p := range valid {
		changed, kind, err := t.UpsertObs(p.obs)
		if err != nil {
			res.Results[p.idx].Reason = "db: " + err.Error()
			res.Ignored++
			continue
		}
		rr := res.Results[p.idx]
		rr.OK = true
		rr.Kind = kind
		if changed {
			rr.Applied = true
			res.Accepted++
			changedStations[p.obs.StationID] = struct{}{}
			if d, ok := earliestAtStation[p.obs.StationID]; !ok || p.obs.Date.Before(d) {
				earliestAtStation[p.obs.StationID] = p.obs.Date
			}
		} else {
			res.Ignored++
		}
		res.Results[p.idx] = rr
	}

	// Refresh climatology and invalidate imputed rows once per changed station
	// (not once per row): a batch of thousands of rows for one station must
	// not rebuild its normals thousands of times. Each invalidation may also
	// cascade to neighbor fills at other stations.
	stationIDs := make([]int64, 0, len(changedStations))
	for sid := range changedStations {
		stationIDs = append(stationIDs, sid)
	}
	sort.Slice(stationIDs, func(i, j int) bool { return stationIDs[i] < stationIDs[j] })
	for _, sid := range stationIDs {
		if err := t.RefreshNormals(sid); err != nil {
			return nil, err
		}
		// The station's own normal fills shift for the whole doy table; drop
		// them all (InvalidateFills computes the earliest removed date).
		cascade, err := t.InvalidateFills(sid, earliestAtStation[sid])
		if err != nil {
			return nil, err
		}
		for other, fd := range cascade {
			if d, ok := earliestAtStation[other]; !ok || fd.Before(d) {
				earliestAtStation[other] = fd
			}
		}
	}

	// Enqueue one task per affected plot (deduped in DB by change id). Start
	// is the earliest changed day of any station touching the plot, so an old
	// correction rolls back to a checkpoint old enough.
	plotStart := map[int64]model.Date{}
	for sid, fd := range earliestAtStation {
		ids, err := t.PlotsOnStation(sid, model.Date{})
		if err != nil {
			return nil, err
		}
		for _, id := range ids {
			if d, ok := plotStart[id]; !ok || fd.Before(d) {
				plotStart[id] = fd
			}
		}
	}
	plotIDs := make([]int64, 0, len(plotStart))
	for id := range plotStart {
		plotIDs = append(plotIDs, id)
	}
	sort.Slice(plotIDs, func(i, j int) bool { return plotIDs[i] < plotIDs[j] })
	for _, pid := range plotIDs {
		if err := t.EnqueueOrMergeTask(store.Task{
			PlotID: pid, AsOf: asOf, Start: plotStart[pid],
			ChangeID: changeID,
			Reason:   "batch observation ingest",
		}); err != nil {
			return nil, err
		}
	}
	res.Enqueued = len(plotIDs)
	res.ChangeID = changeID
	if err := t.Commit(); err != nil {
		return nil, err
	}
	return res, nil
}

func (s *Service) resolveStation(ctx context.Context, code string) (int64, error) {
	var id int64
	err := s.DB.Pool.QueryRow(ctx,
		`SELECT id FROM stations WHERE code=$1`, code).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("station_code %q: %w", code, store.ErrNotFound)
	}
	return id, nil
}

// RegisterPlot creates a plot, its initial binding, and enqueues its initial
// recomputation.
func (s *Service) RegisterPlot(ctx context.Context, p *model.Plot, stationID int64) (int64, error) {
	t, err := s.DB.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer t.Rollback()
	id, err := t.CreatePlot(p, stationID)
	if err != nil {
		return 0, err
	}
	changeID := fmt.Sprintf("plot:%d:%d", id, s.NowTS().UnixNano())
	if err := t.EnqueueTask(store.Task{
		PlotID: id, AsOf: s.Now(), ChangeID: changeID,
		Reason: "plot registered", IsInitial: true,
	}); err != nil {
		return 0, err
	}
	if err := t.Commit(); err != nil {
		return 0, err
	}
	return id, nil
}

// RegisterVariety validates and creates a variety.
func (s *Service) RegisterVariety(ctx context.Context, v *model.Variety) (int64, error) {
	if errs := validate.CheckVarietyParams(v.BaseTemp, v.CeilingTemp, v.StageRequire); len(errs) > 0 {
		return 0, errs
	}
	t, err := s.DB.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer t.Rollback()
	id, err := t.CreateVariety(v)
	if err != nil {
		return 0, err
	}
	return id, t.Commit()
}

// RegisterStation creates a station.
func (s *Service) RegisterStation(ctx context.Context, st *model.Station) (int64, error) {
	t, err := s.DB.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer t.Rollback()
	id, err := t.CreateStation(st)
	if err != nil {
		return 0, err
	}
	return id, t.Commit()
}

// Rebind moves a plot to another station effective fromDay and enqueues a
// recompute. Re-binding to a nonexistent station fails validation.
func (s *Service) Rebind(ctx context.Context, plotID, stationID int64, fromDay model.Date) error {
	t, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer t.Rollback()
	if err := t.Rebind(plotID, stationID, fromDay); err != nil {
		return err
	}
	changeID := fmt.Sprintf("rebind:%d:%s:%d", plotID, fromDay, s.NowTS().UnixNano())
	if err := t.EnqueueOrMergeTask(store.Task{
		PlotID: plotID, AsOf: s.Now(), Start: fromDay, ChangeID: changeID,
		Reason: fmt.Sprintf("rebind plot to station %d from %s", stationID, fromDay),
	}); err != nil {
		return err
	}
	return t.Commit()
}
