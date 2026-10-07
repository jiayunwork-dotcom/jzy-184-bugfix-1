package store

import (
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"agriheat/internal/climate"
	"agriheat/internal/model"
)

// ExistingObs describes the currently authoritative row for a station/day.
type ExistingObs struct {
	TMax   float64
	TMin   float64
	Source string
	Seq    int64
	Exists bool
}

// LookupObs returns the current row for a station/day.
func (t *Tx) LookupObs(stationID int64, day model.Date) (ExistingObs, error) {
	var e ExistingObs
	err := t.tx.QueryRow(t.ctx, `
		SELECT tmax, tmin, source, seq FROM weather
		WHERE station_id=$1 AND day=$2`, stationID, dateArg(day)).
		Scan(&e.TMax, &e.TMin, &e.Source, &e.Seq)
	if errors.Is(err, pgx.ErrNoRows) {
		return e, nil
	}
	if err != nil {
		return e, err
	}
	e.Exists = true
	return e, nil
}

// UpsertObs applies the "largest seq wins" rule inside a SELECT ... FOR UPDATE
// row lock, so two concurrent reports for the same station/day serialize.
//
// changed=false means the report was stale (seq <= stored seq) and ignored.
// kind is insert | correct | replace_fill.
func (t *Tx) UpsertObs(o model.Observation) (changed bool, kind string, err error) {
	// Lock the parent station row; this also serializes two reports whose
	// weather row does not exist yet (nothing to FOR UPDATE).
	if _, err := t.tx.Exec(t.ctx,
		`SELECT id FROM stations WHERE id=$1 FOR UPDATE`, o.StationID); err != nil {
		return false, "", err
	}
	cur, err := t.LookupObs(o.StationID, o.Date)
	if err != nil {
		return false, "", err
	}
	if cur.Exists && cur.Source == model.SourceObs && o.Seq <= cur.Seq {
		return false, "supersede_ignored", nil
	}
	switch {
	case !cur.Exists:
		kind = "insert"
	case cur.Source != model.SourceObs:
		kind = "replace_fill"
	default:
		kind = "correct"
	}
	if _, err := t.tx.Exec(t.ctx, `
		INSERT INTO weather(station_id, day, tmax, tmin, source, seq, updated_at)
		VALUES ($1,$2,$3,$4,'obs',$5, now())
		ON CONFLICT (station_id, day) DO UPDATE
		SET tmax=EXCLUDED.tmax, tmin=EXCLUDED.tmin,
		    source='obs', seq=EXCLUDED.seq, updated_at=now()`,
		o.StationID, dateArg(o.Date), o.TMax, o.TMin, o.Seq); err != nil {
		return false, "", err
	}
	return true, kind, nil
}

// InvalidateFills removes imputed rows that depended on stationID's data and
// returns, per affected station, the earliest date whose fill was removed.
//
//   - stationID: all own-station climatology fills are dropped (a new archive
//     year shifts every day-of-year normal); the earliest is returned.
//   - other stations: only neighbor-based fills citing the changed observed
//     row (stationID, day) are dropped, at exactly that date.
func (t *Tx) InvalidateFills(stationID int64, day model.Date) (map[int64]model.Date, error) {
	affected := map[int64]model.Date{}

	var earliest pgtype.Date
	err := t.tx.QueryRow(t.ctx, `
		SELECT min(day) FROM weather WHERE source <> 'obs' AND station_id=$1`,
		stationID).Scan(&earliest)
	if err != nil {
		return nil, err
	}
	if _, err := t.tx.Exec(t.ctx, `
		DELETE FROM weather_basis
		WHERE (station_id, day) IN (
			SELECT station_id, day FROM weather WHERE source <> 'obs' AND station_id=$1
		)`, stationID); err != nil {
		return nil, err
	}
	if _, err := t.tx.Exec(t.ctx, `
		DELETE FROM weather WHERE source <> 'obs' AND station_id=$1`, stationID); err != nil {
		return nil, err
	}
	if earliest.Valid {
		affected[stationID] = scanDate(earliest)
	}

	// Other stations' neighbor-based fills citing ANY observed row of this
	// station: a batch may have corrected several days at once, so we cannot
	// restrict to the single `day` argument here.
	rows, err := t.tx.Query(t.ctx, `
		SELECT station_id, day FROM weather_basis
		WHERE basis_station_id=$1`,
		stationID)
	if err != nil {
		return nil, err
	}
	type sd struct {
		s int64
		d pgtype.Date
	}
	var targets []sd
	for rows.Next() {
		var x sd
		if err := rows.Scan(&x.s, &x.d); err != nil {
			rows.Close()
			return nil, err
		}
		targets = append(targets, x)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, x := range targets {
		if _, err := t.tx.Exec(t.ctx, `
			DELETE FROM weather_basis WHERE station_id=$1 AND day=$2`,
			x.s, x.d); err != nil {
			return nil, err
		}
		if _, err := t.tx.Exec(t.ctx, `
			DELETE FROM weather WHERE station_id=$1 AND day=$2 AND source <> 'obs'`,
			x.s, x.d); err != nil {
			return nil, err
		}
		fd := scanDate(x.d)
		if d, ok := affected[x.s]; !ok || fd.Before(d) {
			affected[x.s] = fd
		}
	}
	return affected, nil
}

// RefreshNormals rebuilds the climatology table of one station from its
// observed rows only.
func (t *Tx) RefreshNormals(stationID int64) error {
	rows, err := t.tx.Query(t.ctx, `
		SELECT day, tmax, tmin FROM weather
		WHERE station_id=$1 AND source='obs' ORDER BY day`, stationID)
	if err != nil {
		return err
	}
	var obs []model.WeatherDay
	for rows.Next() {
		var d pgtype.Date
		var w model.WeatherDay
		w.StationID = stationID
		w.Source = model.SourceObs
		if err := rows.Scan(&d, &w.TMax, &w.TMin); err != nil {
			rows.Close()
			return err
		}
		w.Date = scanDate(d)
		obs = append(obs, w)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	table := climate.ComputeNormals(obs)
	if _, err := t.tx.Exec(t.ctx,
		`DELETE FROM normals WHERE station_id=$1`, stationID); err != nil {
		return err
	}
	for i, n := range table {
		if n.Samples == 0 {
			continue
		}
		if _, err := t.tx.Exec(t.ctx, `
			INSERT INTO normals(station_id, doy, tmax, tmin, samples)
			VALUES ($1,$2,$3,$4,$5)`,
			stationID, i+1, n.TMax, n.TMin, n.Samples); err != nil {
			return err
		}
	}
	return nil
}

// Weather returns the authoritative row for a station/day.
func (t *Tx) Weather(stationID int64, day model.Date) (model.WeatherDay, bool, error) {
	var w model.WeatherDay
	var d pgtype.Date
	w.StationID = stationID
	err := t.tx.QueryRow(t.ctx, `
		SELECT day, tmax, tmin, source, seq FROM weather
		WHERE station_id=$1 AND day=$2`,
		stationID, dateArg(day)).
		Scan(&d, &w.TMax, &w.TMin, &w.Source, &w.Seq)
	if errors.Is(err, pgx.ErrNoRows) {
		return w, false, nil
	}
	if err != nil {
		return w, false, err
	}
	w.Date = scanDate(d)
	return w, true, nil
}

// Normals loads a station's climatology table (length 366).
func (t *Tx) Normals(stationID int64) ([]climate.Normal, error) {
	out := make([]climate.Normal, 366)
	rows, err := t.tx.Query(t.ctx, `
		SELECT doy, tmax, tmin, samples FROM normals WHERE station_id=$1`, stationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var doy int
		var n climate.Normal
		if err := rows.Scan(&doy, &n.TMax, &n.TMin, &n.Samples); err != nil {
			return nil, err
		}
		if doy >= 1 && doy <= 366 {
			out[doy-1] = n
		}
	}
	return out, rows.Err()
}

func (t *Tx) allStations() (map[int64]model.Station, error) {
	rows, err := t.tx.Query(t.ctx,
		`SELECT id, code, name, lat, lon, elev FROM stations`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[int64]model.Station{}
	for rows.Next() {
		var s model.Station
		if err := rows.Scan(&s.ID, &s.Code, &s.Name, &s.Lat, &s.Lon, &s.Elev); err != nil {
			return nil, err
		}
		m[s.ID] = s
	}
	return m, rows.Err()
}

// NeighborObs returns same-day observed values at other stations.
func (t *Tx) NeighborObs(stationID int64, day model.Date) ([]climate.NeighborDay, error) {
	stations, err := t.allStations()
	if err != nil {
		return nil, err
	}
	rows, err := t.tx.Query(t.ctx, `
		SELECT w.station_id, w.tmax, w.tmin
		FROM weather w
		WHERE w.day=$1 AND w.source='obs' AND w.station_id <> $2`,
		dateArg(day), stationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []climate.NeighborDay
	for rows.Next() {
		var sid int64
		var tx, tn float64
		if err := rows.Scan(&sid, &tx, &tn); err != nil {
			return nil, err
		}
		s := stations[sid]
		out = append(out, climate.NeighborDay{
			StationID: sid, Date: day, TMax: tx, TMin: tn,
			Elev: s.Elev, Lat: s.Lat, Lon: s.Lon, Observed: true,
		})
	}
	return out, rows.Err()
}

// NeighborNormals returns climatology candidates of other stations for day.
func (t *Tx) NeighborNormals(stationID int64, day model.Date) ([]climate.NeighborDay, error) {
	stations, err := t.allStations()
	if err != nil {
		return nil, err
	}
	doy := day.Time().YearDay()
	rows, err := t.tx.Query(t.ctx, `
		SELECT station_id, tmax, tmin FROM normals
		WHERE doy=$1 AND station_id <> $2`, doy, stationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []climate.NeighborDay
	for rows.Next() {
		var sid int64
		var tx, tn float64
		if err := rows.Scan(&sid, &tx, &tn); err != nil {
			return nil, err
		}
		s := stations[sid]
		out = append(out, climate.NeighborDay{
			StationID: sid, Date: day, TMax: tx, TMin: tn,
			Elev: s.Elev, Lat: s.Lat, Lon: s.Lon, Observed: false,
		})
	}
	return out, rows.Err()
}

// EnsureFill returns the value for a missing past day, creating an imputed
// row from station climatology or lapse-corrected IDW neighbors. An observed
// row always wins and is returned as-is.
func (t *Tx) EnsureFill(stationID int64, day model.Date) (model.WeatherDay, bool, error) {
	if w, ok, err := t.Weather(stationID, day); err != nil {
		return w, false, err
	} else if ok {
		return w, true, nil
	}
	target, err := t.GetStation(stationID)
	if err != nil {
		return model.WeatherDay{}, false, err
	}
	own, err := t.Normals(stationID)
	if err != nil {
		return model.WeatherDay{}, false, err
	}
	obsN, err := t.NeighborObs(stationID, day)
	if err != nil {
		return model.WeatherDay{}, false, err
	}
	normN, err := t.NeighborNormals(stationID, day)
	if err != nil {
		return model.WeatherDay{}, false, err
	}
	est := climate.EstimateFill(target, day, own, obsN, normN)
	if !est.Found {
		return model.WeatherDay{}, false, nil
	}
	if _, err := t.tx.Exec(t.ctx, `
		INSERT INTO weather(station_id, day, tmax, tmin, source, seq, updated_at)
		VALUES ($1,$2,$3,$4,$5,0,now())`,
		stationID, dateArg(day), est.TMax, est.TMin, est.Source); err != nil {
		return model.WeatherDay{}, false, err
	}
	for _, b := range est.Basis {
		if _, err := t.tx.Exec(t.ctx, `
			INSERT INTO weather_basis(station_id, day, basis_station_id, basis_day)
			VALUES ($1,$2,$3,$4) ON CONFLICT DO NOTHING`,
			stationID, dateArg(day), b.StationID, dateArg(b.Date)); err != nil {
			return model.WeatherDay{}, false, err
		}
	}
	w := model.WeatherDay{
		StationID: stationID, Date: day,
		TMax: est.TMax, TMin: est.TMin, Source: est.Source, Basis: est.Basis,
	}
	return w, true, nil
}
