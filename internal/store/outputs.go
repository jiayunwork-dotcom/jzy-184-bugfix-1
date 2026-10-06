package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"agriheat/internal/model"
)

// ReplacePlotDays rewrites a plot's daily rows from day `from` onward:
// rows in [from, last] are deleted and reinserted in one transaction.
func (t *Tx) ReplacePlotDays(plotID int64, from model.Date, rows []model.PlotDay) error {
	if _, err := t.tx.Exec(t.ctx, `
		DELETE FROM plot_days WHERE plot_id=$1 AND day >= $2`,
		plotID, dateArg(from)); err != nil {
		return err
	}
	for _, r := range rows {
		var station interface{}
		if r.StationID != 0 {
			station = r.StationID
		}
		if _, err := t.tx.Exec(t.ctx, `
			INSERT INTO plot_days(plot_id, day, station_id, tmax, tmin,
			                       daily_gdd, cum_gdd, source)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
			plotID, dateArg(r.Date), station, r.TMax, r.TMin,
			r.DailyGDD, r.CumGDD, r.Source); err != nil {
			return err
		}
	}
	return nil
}

// ResetSnapshots removes snapshots strictly later than keep.
func (t *Tx) ResetSnapshots(plotID int64, keep model.Date) error {
	_, err := t.tx.Exec(t.ctx, `
		DELETE FROM plot_snapshots WHERE plot_id=$1 AND day > $2`,
		plotID, dateArg(keep))
	return err
}

// LatestSnapshot returns the newest snapshot at or before notAfter.
func (t *Tx) LatestSnapshot(plotID int64, notAfter model.Date) (model.Snapshot, bool, error) {
	var s model.Snapshot
	var d pgtype.Date
	err := t.tx.QueryRow(t.ctx, `
		SELECT day, cum_gdd FROM plot_snapshots
		WHERE plot_id=$1 AND day <= $2
		ORDER BY day DESC LIMIT 1`,
		plotID, dateArg(notAfter)).Scan(&d, &s.CumGDD)
	if errors.Is(err, pgx.ErrNoRows) {
		return s, false, nil
	}
	if err != nil {
		return s, false, err
	}
	s.PlotID = plotID
	s.Date = scanDate(d)
	return s, true, nil
}

// UpsertSnapshot writes a checkpoint.
func (t *Tx) UpsertSnapshot(s model.Snapshot) error {
	_, err := t.tx.Exec(t.ctx, `
		INSERT INTO plot_snapshots(plot_id, day, cum_gdd)
		VALUES ($1,$2,$3)
		ON CONFLICT (plot_id, day) DO UPDATE SET cum_gdd=EXCLUDED.cum_gdd`,
		s.PlotID, dateArg(s.Date), s.CumGDD)
	return err
}

// ListStages loads the persisted stage states of a plot (may be empty).
func (t *Tx) ListStages(plotID int64) ([]model.StageState, error) {
	rows, err := t.tx.Query(t.ctx, `
		SELECT stage, status, day, cum_at FROM plot_stages
		WHERE plot_id=$1`, plotID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.StageState
	for rows.Next() {
		var st, status string
		var day pgtype.Date
		var cum float64
		if err := rows.Scan(&st, &status, &day, &cum); err != nil {
			return nil, err
		}
		s := model.StageState{
			Stage: model.Stage(st), NameCN: model.StageCN[model.Stage(st)],
			Status: status, CumAtDay: cum,
		}
		if day.Valid {
			s.Date = scanDate(day)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// SaveStages replaces the persisted stage vector.
func (t *Tx) SaveStages(plotID int64, states []model.StageState) error {
	for _, s := range states {
		var day interface{}
		if !s.Date.IsZero() {
			day = dateArg(s.Date)
		}
		if _, err := t.tx.Exec(t.ctx, `
			INSERT INTO plot_stages(plot_id, stage, status, day, cum_at)
			VALUES ($1,$2,$3,$4,$5)
			ON CONFLICT (plot_id, stage) DO UPDATE
			  SET status=EXCLUDED.status, day=EXCLUDED.day, cum_at=EXCLUDED.cum_at`,
			plotID, string(s.Stage), s.Status, day, s.CumAtDay); err != nil {
			return err
		}
	}
	return nil
}

// InsertEvent inserts an event idempotently keyed by changeID.
func (t *Tx) InsertEvent(plotID int64, st model.Stage, old, new *model.Date, reason, changeID string) (bool, error) {
	var oldArg, newArg interface{}
	if old != nil {
		oldArg = dateArg(*old)
	}
	if new != nil {
		newArg = dateArg(*new)
	}
	ct, err := t.tx.Exec(t.ctx, `
		INSERT INTO stage_events(plot_id, stage, old_day, new_day, reason, change_id)
		VALUES ($1,$2,$3,$4,$5,$6)
		ON CONFLICT (plot_id, stage, change_id) DO NOTHING`,
		plotID, string(st), oldArg, newArg, reason, changeID)
	if err != nil {
		return false, err
	}
	return ct.RowsAffected() == 1, nil
}

// PlotDayRow is one query row of the daily curve.
type PlotDayRow = model.PlotDay

// QueryPlotDays returns the persisted daily window [from,to].
func QueryPlotDays(ctx context.Context, db *DB, plotID int64, from, to model.Date) ([]model.PlotDay, error) {
	rows, err := db.Pool.Query(ctx, `
		SELECT day, station_id, tmax, tmin, daily_gdd, cum_gdd, source
		FROM plot_days
		WHERE plot_id=$1 AND day BETWEEN $2 AND $3
		ORDER BY day`, plotID, dateArg(from), dateArg(to))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.PlotDay
	for rows.Next() {
		var day pgtype.Date
		var station pgtype.Int8
		var tx, tn pgtype.Float8
		var r model.PlotDay
		if err := rows.Scan(&day, &station, &tx, &tn, &r.DailyGDD, &r.CumGDD, &r.Source); err != nil {
			return nil, err
		}
		r.Date = scanDate(day)
		if station.Valid {
			r.StationID = station.Int64
		}
		if tx.Valid {
			r.TMax = tx.Float64
		}
		if tn.Valid {
			r.TMin = tn.Float64
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// QueryStages returns a plot's stage states.
func QueryStages(ctx context.Context, db *DB, plotID int64) ([]model.StageState, error) {
	t, err := db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer t.Rollback()
	return t.ListStages(plotID)
}

// QueryEvents pulls events with id > afterID, ascending, up to limit rows.
func QueryEvents(ctx context.Context, db *DB, afterID int64, limit int) ([]model.StageEvent, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := db.Pool.Query(ctx, `
		SELECT id, plot_id, stage, old_day, new_day, reason, created_at
		FROM stage_events
		WHERE id > $1
		ORDER BY id ASC
		LIMIT $2`, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.StageEvent
	for rows.Next() {
		var e model.StageEvent
		var oldDay, newDay pgtype.Date
		if err := rows.Scan(&e.ID, &e.PlotID, &e.Stage, &oldDay, &newDay,
			&e.Reason, &e.CreatedAt); err != nil {
			return nil, err
		}
		e.NameCN = model.StageCN[e.Stage]
		if oldDay.Valid {
			d := scanDate(oldDay)
			e.OldDate = &d
		}
		if newDay.Valid {
			d := scanDate(newDay)
			e.NewDate = &d
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
