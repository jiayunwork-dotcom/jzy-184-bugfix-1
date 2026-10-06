// Package engine runs plot-level incremental recomputation.
//
// # Correctness strategy
//
// The engine is I/O-free: it talks to a Port (transaction-scoped) implemented
// once against PostgreSQL (store package) and once in-memory by the tests.
// The same walk is used in production incremental updates and in the test
// oracle mode; the random-correction tests additionally rebuild the answer
// with an independent summation in phenology and require day-by-day equality.
package engine

import (
	"agriheat/internal/climate"
	"agriheat/internal/gdd"
	"agriheat/internal/model"
)

// ForecastHorizon limits how many future days are projected with climatology.
const ForecastHorizon = 366

// Tx is the transaction-scoped storage port the engine needs.
type Tx interface {
	// Domain loads.
	GetPlot(id int64) (*model.Plot, error)
	GetVariety(id int64) (*model.Variety, error)
	GetStation(id int64) (model.Station, error)

	// Weather: any authoritative row (obs or fill).
	Weather(stationID int64, day model.Date) (model.WeatherDay, bool, error)
	// EnsureFill returns the current value for a missing past day, creating or
	// refreshing an imputed row. An observed row always wins and is returned
	// unchanged. found=false means no data source existed.
	EnsureFill(stationID int64, day model.Date) (w model.WeatherDay, found bool, err error)

	// Climate inputs.
	Normals(stationID int64) ([]climate.Normal, error)
	NeighborObs(stationID int64, day model.Date) ([]climate.NeighborDay, error)
	NeighborNormals(stationID int64, day model.Date) ([]climate.NeighborDay, error)

	// Plot outputs.
	// ReplacePlotDays deletes rows of plot with date >= from and rewrites them
	// with rows; it also deletes any leftover row dated after the last given
	// row (a shortened horizon).
	ReplacePlotDays(plotID int64, from model.Date, rows []model.PlotDay) error
	// ResetSnapshots removes snapshots of plot dated after keep (strictly), so
	// the walk can recreate month-end checkpoints.
	ResetSnapshots(plotID int64, keep model.Date) error
	LatestSnapshot(plotID int64, notAfter model.Date) (model.Snapshot, bool, error)
	UpsertSnapshot(s model.Snapshot) error
	ListStages(plotID int64) ([]model.StageState, error)
	SaveStages(plotID int64, states []model.StageState) error
	// InsertEvent persists one event keyed by changeID; inserted=false means
	// the (plot,stage,changeID) event already existed (idempotent dedupe).
	InsertEvent(plotID int64, st model.Stage, old, new *model.Date, reason, changeID string) (inserted bool, err error)

	Commit() error
	Rollback() error
}

// DB opens engine transactions.
type DB interface {
	BeginTx() (Tx, error)
}

// Method is the effective configured GDD convention. It is a value so tests
// can run all three methods through the same engine.
var Method = gdd.MethodSine
