package engine_test

import (
	"fmt"
	"sync"
	"time"

	"agriheat/internal/climate"
	"agriheat/internal/engine"
	"agriheat/internal/model"
)

// memWorld is an in-memory implementation of the engine storage port used by
// the tests. It mirrors the PostgreSQL semantics that matter for correctness:
// largest-seq-wins observations, imputed rows with basis records, month-end
// snapshots replaced on re-walk, idempotent events keyed by change id, and a
// durable task queue that survives simulated restarts.
type memWorld struct {
	mu sync.Mutex

	stations  map[int64]model.Station
	varieties map[int64]*model.Variety
	plots     map[int64]*model.Plot

	// weather[station][date] = authoritative row
	weather map[int64]map[model.Date]model.WeatherDay
	basis   map[[2]interface{}][]model.WeatherBasis

	snapshots map[int64]map[model.Date]float64
	plotDays  map[int64]map[model.Date]model.PlotDay
	stages    map[int64]map[model.Stage]model.StageState
	events    []memEvent

	tasks   []memTask
	taskID  int64
	eventID int64

	asOf model.Date
}

type memEvent struct {
	id       int64
	plotID   int64
	stage    model.Stage
	old, new *model.Date
	reason   string
	changeID string
}

type memTask struct {
	id       int64
	plotID   int64
	asOf     model.Date
	start    model.Date
	changeID string
	reason   string
	initial  bool
	status   string // pending | processing | done | failed
	attempts int
}

func newMemWorld(asOf model.Date) *memWorld {
	return &memWorld{
		stations:  map[int64]model.Station{},
		varieties: map[int64]*model.Variety{},
		plots:     map[int64]*model.Plot{},
		weather:   map[int64]map[model.Date]model.WeatherDay{},
		basis:     map[[2]interface{}][]model.WeatherBasis{},
		snapshots: map[int64]map[model.Date]float64{},
		plotDays:  map[int64]map[model.Date]model.PlotDay{},
		stages:    map[int64]map[model.Stage]model.StageState{},
		asOf:      asOf,
	}
}

// ---- world setup helpers -------------------------------------------------

func (w *memWorld) addStation(id int64, code string, lat, lon, elev float64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.stations[id] = model.Station{ID: id, Code: code, Lat: lat, Lon: lon, Elev: elev}
}

func (w *memWorld) addVariety(id int64, base, ceil float64, req []float64) {
	v := &model.Variety{ID: id, Name: fmt.Sprintf("v%d", id), BaseTemp: base, CeilingTemp: ceil,
		StageRequire: map[model.Stage]float64{}}
	for i, st := range model.OrderedStages {
		v.StageRequire[st] = req[i]
	}
	w.varieties[id] = v
}

func (w *memWorld) addPlot(id, varietyID, stationID int64, sowing model.Date) {
	w.plots[id] = &model.Plot{
		ID: id, Code: fmt.Sprintf("p%d", id), VarietyID: varietyID, SowingDate: sowing,
		Bindings: []model.Binding{{PlotID: id, StationID: stationID, EffectiveFrom: sowing}},
	}
}

func (w *memWorld) rebind(plotID, stationID int64, from model.Date) {
	p := w.plots[plotID]
	for i := range p.Bindings {
		if p.Bindings[i].EffectiveTo.IsZero() {
			p.Bindings[i].EffectiveTo = from
		}
	}
	p.Bindings = append(p.Bindings, model.Binding{PlotID: plotID, StationID: stationID, EffectiveFrom: from})
}

// putObs writes an authoritative observed row directly (test setup).
func (w *memWorld) putObs(o model.Observation) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.weather[o.StationID] == nil {
		w.weather[o.StationID] = map[model.Date]model.WeatherDay{}
	}
	w.weather[o.StationID][o.Date] = model.WeatherDay{
		StationID: o.StationID, Date: o.Date, TMax: o.TMax, TMin: o.TMin,
		Source: model.SourceObs, Seq: o.Seq,
	}
}

// upsertObs mirrors store.UpsertObs: largest seq wins, stale late rows ignored.
// Returns (changed, kind). changed=true means authoritative data moved.
func (w *memWorld) upsertObs(o model.Observation) (bool, string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	m := w.weather[o.StationID]
	if m == nil {
		m = map[model.Date]model.WeatherDay{}
		w.weather[o.StationID] = m
	}
	if cur, ok := m[o.Date]; ok && cur.Source == model.SourceObs && o.Seq <= cur.Seq {
		return false, "supersede_ignored"
	}
	kind := "insert"
	if cur, ok := m[o.Date]; ok {
		if cur.Source == model.SourceObs {
			kind = "correct"
		} else {
			kind = "replace_fill"
		}
	}
	m[o.Date] = model.WeatherDay{
		StationID: o.StationID, Date: o.Date, TMax: o.TMax, TMin: o.TMin,
		Source: model.SourceObs, Seq: o.Seq,
	}
	w.refreshNormalsLocked(o.StationID)
	w.invalidateFillsLocked(o.StationID, o.Date)
	return true, kind
}

// refreshNormalsLocked recomputes imputed rows lazily; the normal table is
// derived on demand via observedRowsLocked, so nothing to precompute.
func (w *memWorld) refreshNormalsLocked(int64) {}

func (w *memWorld) observedRowsLocked(stationID int64) []model.WeatherDay {
	var out []model.WeatherDay
	for _, row := range w.weather[stationID] {
		if row.Source == model.SourceObs {
			out = append(out, row)
		}
	}
	return out
}

// invalidateFillsLocked drops the station's own fills and neighbor fills
// citing the changed row. Returns affected station -> earliest day.
func (w *memWorld) invalidateFillsLocked(stationID int64, day model.Date) map[int64]model.Date {
	affected := map[int64]model.Date{}
	for d, row := range w.weather[stationID] {
		if row.Source != model.SourceObs {
			delete(w.weather[stationID], d)
			if e, ok := affected[stationID]; !ok || d.Before(e) {
				affected[stationID] = d
			}
		}
	}
	delete(w.basis, key(stationID, day))
	for sid, days := range w.weather {
		for fd, row := range days {
			if row.Source == model.SourceObs {
				continue
			}
			for _, b := range row.Basis {
				if b.StationID == stationID && b.Date.Equal(day) {
					if e, ok := affected[sid]; !ok || fd.Before(e) {
						affected[sid] = fd
					}
				}
			}
		}
	}
	for sid, fd := range affected {
		if sid == stationID {
			continue
		}
		if row, ok := w.weather[sid][fd]; ok && row.Source != model.SourceObs {
			delete(w.weather[sid], fd)
		}
	}
	return affected
}

func key(stationID int64, d model.Date) [2]interface{} {
	return [2]interface{}{stationID, d.String()}
}

// normalsLocked builds the station climatology table.
func (w *memWorld) normalsLocked(stationID int64) []climate.Normal {
	return climate.ComputeNormals(w.observedRowsLocked(stationID))
}

// ---- engine.DB / engine.Tx ----------------------------------------------

type memDB struct{ w *memWorld }
type memTx struct {
	w    *memWorld
	done bool
}

func (w *memWorld) db() engine.DB { return memDB{w} }

func (d memDB) BeginTx() (engine.Tx, error) {
	// Tests are single-connection: the "transaction" sees the shared world and
	// Commit is a no-op. Rollback is implemented in the restart test by
	// discarding the engine result; the durable queue model is explicit there.
	d.w.mu.Lock()
	return &memTx{w: d.w}, nil
}

func (t *memTx) finish() {
	if !t.done {
		t.done = true
		t.w.mu.Unlock()
	}
}

func (t *memTx) Rollback() error { t.finish(); return nil }
func (t *memTx) Commit() error   { t.finish(); return nil }

func (t memTx) GetPlot(id int64) (*model.Plot, error) {
	p, ok := t.w.plots[id]
	if !ok {
		return nil, fmt.Errorf("plot %d not found", id)
	}
	cp := *p
	cp.Bindings = append([]model.Binding(nil), p.Bindings...)
	return &cp, nil
}

func (t memTx) GetVariety(id int64) (*model.Variety, error) {
	v, ok := t.w.varieties[id]
	if !ok {
		return nil, fmt.Errorf("variety %d not found", id)
	}
	cp := *v
	return &cp, nil
}

func (t memTx) GetStation(id int64) (model.Station, error) {
	s, ok := t.w.stations[id]
	if !ok {
		return s, fmt.Errorf("station %d not found", id)
	}
	return s, nil
}

func (t memTx) Weather(stationID int64, day model.Date) (model.WeatherDay, bool, error) {
	if m := t.w.weather[stationID]; m != nil {
		if r, ok := m[day]; ok {
			return r, true, nil
		}
	}
	return model.WeatherDay{}, false, nil
}

func (t memTx) Normals(stationID int64) ([]climate.Normal, error) {
	return t.w.normalsLocked(stationID), nil
}

func (t memTx) NeighborObs(stationID int64, day model.Date) ([]climate.NeighborDay, error) {
	var out []climate.NeighborDay
	for sid, m := range t.w.weather {
		if sid == stationID {
			continue
		}
		if r, ok := m[day]; ok && r.Source == model.SourceObs {
			s := t.w.stations[sid]
			out = append(out, climate.NeighborDay{
				StationID: sid, Date: day, TMax: r.TMax, TMin: r.TMin,
				Elev: s.Elev, Lat: s.Lat, Lon: s.Lon, Observed: true,
			})
		}
	}
	return out, nil
}

func (t memTx) NeighborNormals(stationID int64, day model.Date) ([]climate.NeighborDay, error) {
	var out []climate.NeighborDay
	for sid := range t.w.stations {
		if sid == stationID {
			continue
		}
		tab := t.w.normalsLocked(sid)
		if n, ok := climate.LookupNormal(tab, day); ok {
			s := t.w.stations[sid]
			out = append(out, climate.NeighborDay{
				StationID: sid, Date: day, TMax: n.TMax, TMin: n.TMin,
				Elev: s.Elev, Lat: s.Lat, Lon: s.Lon, Observed: false,
			})
		}
	}
	return out, nil
}

func (t memTx) EnsureFill(stationID int64, day model.Date) (model.WeatherDay, bool, error) {
	if m := t.w.weather[stationID]; m != nil {
		if r, ok := m[day]; ok {
			return r, true, nil
		}
	}
	target := t.w.stations[stationID]
	own := t.w.normalsLocked(stationID)
	obsN, _ := t.NeighborObs(stationID, day)
	normN, _ := t.NeighborNormals(stationID, day)
	est := climate.EstimateFill(target, day, own, obsN, normN)
	if !est.Found {
		return model.WeatherDay{}, false, nil
	}
	w := model.WeatherDay{StationID: stationID, Date: day, TMax: est.TMax, TMin: est.TMin, Source: est.Source, Basis: est.Basis}
	if t.w.weather[stationID] == nil {
		t.w.weather[stationID] = map[model.Date]model.WeatherDay{}
	}
	t.w.weather[stationID][day] = w
	t.w.basis[key(stationID, day)] = est.Basis
	return w, true, nil
}

func (t memTx) ReplacePlotDays(plotID int64, from model.Date, rows []model.PlotDay) error {
	m := t.w.plotDays[plotID]
	if m == nil {
		m = map[model.Date]model.PlotDay{}
		t.w.plotDays[plotID] = m
	}
	for d := range m {
		if !d.Before(from) {
			delete(m, d)
		}
	}
	for _, r := range rows {
		m[r.Date] = r
	}
	return nil
}

func (t memTx) ResetSnapshots(plotID int64, keep model.Date) error {
	for d := range t.w.snapshots[plotID] {
		if d.After(keep) {
			delete(t.w.snapshots[plotID], d)
		}
	}
	return nil
}

func (t memTx) LatestSnapshot(plotID int64, notAfter model.Date) (model.Snapshot, bool, error) {
	var best model.Date
	found := false
	for d := range t.w.snapshots[plotID] {
		if !d.After(notAfter) && (!found || d.After(best)) {
			best, found = d, true
		}
	}
	if !found {
		return model.Snapshot{}, false, nil
	}
	return model.Snapshot{PlotID: plotID, Date: best, CumGDD: t.w.snapshots[plotID][best]}, true, nil
}

func (t memTx) UpsertSnapshot(s model.Snapshot) error {
	if t.w.snapshots[s.PlotID] == nil {
		t.w.snapshots[s.PlotID] = map[model.Date]float64{}
	}
	t.w.snapshots[s.PlotID][s.Date] = s.CumGDD
	return nil
}

func (t memTx) ListStages(plotID int64) ([]model.StageState, error) {
	var out []model.StageState
	for _, st := range model.OrderedStages {
		if s, ok := t.w.stages[plotID][st]; ok {
			out = append(out, s)
		}
	}
	return out, nil
}

func (t memTx) SaveStages(plotID int64, states []model.StageState) error {
	if t.w.stages[plotID] == nil {
		t.w.stages[plotID] = map[model.Stage]model.StageState{}
	}
	for _, s := range states {
		t.w.stages[plotID][s.Stage] = s
	}
	return nil
}

func (t memTx) InsertEvent(plotID int64, st model.Stage, old, new *model.Date, reason, changeID string) (bool, error) {
	for _, e := range t.w.events {
		if e.plotID == plotID && e.stage == st && e.changeID == changeID {
			return false, nil
		}
	}
	t.w.eventID++
	t.w.events = append(t.w.events, memEvent{
		id: t.w.eventID, plotID: plotID, stage: st, old: old, new: new,
		reason: reason, changeID: changeID,
	})
	return true, nil
}

// ---- task queue helpers (used by restart/ordering tests) ----------------

func (w *memWorld) enqueueLocked(k memTask) {
	for _, t := range w.tasks {
		if t.plotID == k.plotID && t.changeID == k.changeID &&
			(t.status == "pending" || t.status == "processing") {
			return
		}
	}
	w.taskID++
	k.id = w.taskID
	k.status = "pending"
	w.tasks = append(w.tasks, k)
}

func (w *memWorld) enqueue(k memTask) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.enqueueLocked(k)
}

func (w *memWorld) pending() []memTask {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []memTask
	for _, t := range w.tasks {
		if t.status == "pending" {
			out = append(out, t)
		}
	}
	return out
}

func (w *memWorld) recoverProcessing() {
	w.mu.Lock()
	defer w.mu.Unlock()
	for i := range w.tasks {
		if w.tasks[i].status == "processing" {
			w.tasks[i].status = "pending"
		}
	}
}

// runOne claims and processes the next pending task, optionally crashing after
// the claim (task left processing) to simulate restart.
func (w *memWorld) runOne(crash bool) (ran bool, err error) {
	w.mu.Lock()
	idx := -1
	for i := range w.tasks {
		if w.tasks[i].status == "pending" {
			idx = i
			break
		}
	}
	if idx < 0 {
		w.mu.Unlock()
		return false, nil
	}
	w.tasks[idx].status = "processing"
	task := w.tasks[idx]
	w.mu.Unlock()

	if crash {
		// Simulate process death with the task stuck in "processing".
		return true, nil
	}

	_, err = engine.Recompute(w.db(), engine.Request{
		PlotID: task.plotID, AsOf: task.asOf, Start: task.start,
		ChangeID: task.changeID, Reason: task.reason, Initial: task.initial,
	})
	w.mu.Lock()
	defer w.mu.Unlock()
	if err != nil {
		w.tasks[idx].status = "pending"
		return true, err
	}
	w.tasks[idx].status = "done"
	return true, nil
}

func (w *memWorld) drain() error {
	for {
		ran, err := w.runOne(false)
		if err != nil {
			return err
		}
		if !ran {
			return nil
		}
	}
}

var _ = time.Now
