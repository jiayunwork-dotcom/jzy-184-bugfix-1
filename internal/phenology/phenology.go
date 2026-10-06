// Package phenology turns a daily heat-unit series into cumulative values and
// growth-stage dates. It contains no I/O: the incremental engine and the
// full-recompute oracle both build a []DayRow and call the same functions,
// which is what guarantees incremental == full recompute.
package phenology

import (
	"agriheat/internal/model"
)

// DayRow is one input day of the accumulation walk.
type DayRow struct {
	Date      model.Date
	TMax      float64
	TMin      float64
	GDD       float64
	Source    string
	StationID int64
}

// CumAt returns a parallel slice of cumulative GDD values.
func CumAt(rows []DayRow) []float64 {
	out := make([]float64, len(rows))
	cum := 0.0
	for i := range rows {
		cum += rows[i].GDD
		out[i] = cum
	}
	return out
}

const (
	// StatusReached means the stage requirement was met on/before asOf.
	StatusReached = "reached"
	// StatusForecast means it is projected from future climatological days.
	StatusForecast = "forecast"
)

// StageResult is one stage's outcome.
type StageResult struct {
	Stage  model.Stage
	Status string
	Date   model.Date
	CumAt  float64
}

// Stages walks cumulative GDD across observed/filled and forecast rows and
// locates each stage's crossing day.
//
//	rows: daily rows ordered ascending; prefix rows (up to and including asOf)
//	      carry obs/fill sources, later rows carry source "climate".
//	asOf: last past date present in rows; a crossing on/before it is "reached"
//	      with the actual crossing date, later crossings are "forecast".
//	requirements: strictly increasing cumulative GDD needs per stage.
//
// A stage not reachable within the provided rows yields Date zero and
// StatusForecast (caller treats zero date as "unknown/beyond horizon").
func Stages(rows []DayRow, asOf model.Date, requirements map[model.Stage]float64) []StageResult {
	cum := CumAt(rows)
	out := make([]StageResult, 0, len(model.OrderedStages))
	si := 0
	for i := range rows {
		for si < len(model.OrderedStages) {
			st := model.OrderedStages[si]
			if cum[i] < requirements[st] {
				break
			}
			status := StatusForecast
			if !rows[i].Date.After(asOf) {
				status = StatusReached
			}
			out = append(out, StageResult{
				Stage:  st,
				Status: status,
				Date:   rows[i].Date,
				CumAt:  cum[i],
			})
			si++
		}
		if si == len(model.OrderedStages) {
			break
		}
	}
	// Unreached stages stay absent; the engine fills placeholders.
	return out
}
