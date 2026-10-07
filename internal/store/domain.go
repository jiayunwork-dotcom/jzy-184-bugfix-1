package store

import (
	"errors"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"agriheat/internal/model"
)

// ErrNotFound is returned for missing rows.
var ErrNotFound = errors.New("not found")

func scanDate(v pgtype.Date) model.Date {
	if !v.Valid {
		return model.Date{}
	}
	return model.DateFromTime(v.Time)
}

func dateArg(d model.Date) pgtype.Date {
	return pgtype.Date{Time: d.Time(), Valid: !d.IsZero()}
}

// GetStation loads a station.
func (t *Tx) GetStation(id int64) (model.Station, error) {
	var s model.Station
	err := t.tx.QueryRow(t.ctx,
		`SELECT id, code, name, lat, lon, elev FROM stations WHERE id=$1`, id).
		Scan(&s.ID, &s.Code, &s.Name, &s.Lat, &s.Lon, &s.Elev)
	if errors.Is(err, pgx.ErrNoRows) {
		return s, ErrNotFound
	}
	return s, err
}

// GetVariety loads a variety with its stage requirement map.
func (t *Tx) GetVariety(id int64) (*model.Variety, error) {
	var v model.Variety
	req := map[string]float64{}
	err := t.tx.QueryRow(t.ctx,
		`SELECT id, name, base_temp, ceiling_temp, stage_require
		 FROM varieties WHERE id=$1`, id).
		Scan(&v.ID, &v.Name, &v.BaseTemp, &v.CeilingTemp, &req)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	v.StageRequire = make(map[model.Stage]float64, len(req))
	for k, val := range req {
		v.StageRequire[model.Stage(k)] = val
	}
	return &v, nil
}

// GetPlot loads a plot and its binding intervals ordered by effective date.
func (t *Tx) GetPlot(id int64) (*model.Plot, error) {
	p := &model.Plot{ID: id}
	err := t.tx.QueryRow(t.ctx,
		`SELECT code, name, variety_id, sowing_date FROM plots WHERE id=$1`, id).
		Scan(&p.Code, &p.Name, &p.VarietyID, &p.SowingDate)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	rows, err := t.tx.Query(t.ctx,
		`SELECT station_id, effective_from, effective_to
		 FROM plot_bindings WHERE plot_id=$1 ORDER BY effective_from`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var b model.Binding
		var from, to pgtype.Date
		b.PlotID = id
		if err := rows.Scan(&b.StationID, &from, &to); err != nil {
			return nil, err
		}
		b.EffectiveFrom = scanDate(from)
		if to.Valid {
			b.EffectiveTo = scanDate(to)
		}
		p.Bindings = append(p.Bindings, b)
	}
	return p, rows.Err()
}

// CreateStation inserts a station and returns its id.
func (t *Tx) CreateStation(s *model.Station) (int64, error) {
	var id int64
	err := t.tx.QueryRow(t.ctx, `
		INSERT INTO stations(code, name, lat, lon, elev)
		VALUES ($1,$2,$3,$4,$5) RETURNING id`,
		s.Code, s.Name, s.Lat, s.Lon, s.Elev).Scan(&id)
	return id, err
}

// CreateVariety inserts a variety and returns its id.
func (t *Tx) CreateVariety(v *model.Variety) (int64, error) {
	req := map[string]float64{}
	for k, val := range v.StageRequire {
		req[string(k)] = val
	}
	var id int64
	err := t.tx.QueryRow(t.ctx, `
		INSERT INTO varieties(name, base_temp, ceiling_temp, stage_require)
		VALUES ($1,$2,$3,$4) RETURNING id`,
		v.Name, v.BaseTemp, v.CeilingTemp, req).Scan(&id)
	return id, err
}

// CreatePlot inserts a plot and its initial binding.
func (t *Tx) CreatePlot(p *model.Plot, initialStation int64) (int64, error) {
	var id int64
	err := t.tx.QueryRow(t.ctx, `
		INSERT INTO plots(code, name, variety_id, sowing_date)
		VALUES ($1,$2,$3,$4) RETURNING id`,
		p.Code, p.Name, p.VarietyID, p.SowingDate).Scan(&id)
	if err != nil {
		return 0, err
	}
	// Validate the initial station FK explicitly for a friendly error.
	if _, err := t.GetStation(initialStation); err != nil {
		return 0, fmt.Errorf("initial station: %w", err)
	}
	_, err = t.tx.Exec(t.ctx, `
		INSERT INTO plot_bindings(plot_id, station_id, effective_from)
		VALUES ($1,$2,$3)`, id, initialStation, p.SowingDate)
	if err != nil {
		return 0, err
	}
	return id, nil
}

// Rebind closes the current open interval at fromDay and opens a new binding
// to stationID effective fromDay, in the caller's transaction.
func (t *Tx) Rebind(plotID, stationID int64, fromDay model.Date) error {
	if _, err := t.GetStation(stationID); err != nil {
		return fmt.Errorf("rebind target: %w", err)
	}
	ct, err := t.tx.Exec(t.ctx, `
		UPDATE plot_bindings SET effective_to = $2
		WHERE plot_id=$1 AND effective_to IS NULL`, plotID, dateArg(fromDay))
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return fmt.Errorf("plot %d has no current binding to close", plotID)
	}
	_, err = t.tx.Exec(t.ctx, `
		INSERT INTO plot_bindings(plot_id, station_id, effective_from)
		VALUES ($1,$2,$3)`, plotID, stationID, dateArg(fromDay))
	return err
}

// LockStation locks a station row in the caller's transaction. Write paths use
// the same parent-row lock order as UpsertObs (station before affected plots),
// which lets plot registration establish its initial baseline concurrently
// with observation ingestion without reading an uncommitted/unstable state.
func (t *Tx) LockStation(id int64) error {
	_, err := t.tx.Exec(t.ctx, `SELECT id FROM stations WHERE id=$1 FOR UPDATE`, id)
	return err
}

// LockPlots locks the supplied plot rows in id order. Write transactions that
// enqueue recomputes for overlapping plot sets take these in the same fixed
// order after their station-row locks, which prevents deadlocks and makes
// committed task-id order match the write order.
func (t *Tx) LockPlots(ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	sorted := append([]int64(nil), ids...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	_, err := t.tx.Exec(t.ctx,
		`SELECT id FROM plots WHERE id = ANY($1) ORDER BY id FOR UPDATE`, sorted)
	return err
}

// PlotsOnStation returns ids of plots bound to stationID in an interval that
// intersects [sinceDay, +∞).
func (t *Tx) PlotsOnStation(stationID int64, sinceDay model.Date) ([]int64, error) {
	rows, err := t.tx.Query(t.ctx, `
		SELECT DISTINCT plot_id FROM plot_bindings
		WHERE station_id=$1
		  AND (effective_to IS NULL OR effective_to > $2)`,
		stationID, dateArg(sinceDay))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// AllPlotIDs returns every plot id.
func (t *Tx) AllPlotIDs() ([]int64, error) {
	rows, err := t.tx.Query(t.ctx, `SELECT id FROM plots ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
