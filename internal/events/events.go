// Package events computes stage-prediction change events deterministically.
// A recompute produces a complete new stage vector; Diff compares it against
// the persisted vector. Because Diff is pure and keyed by (plot, stage,
// trigger change id), re-running the same change yields no duplicate events.
package events

import (
	"agriheat/internal/model"
	"agriheat/internal/phenology"
)

// Draft is a stage result that may carry no date yet (not reached inside the
// projection horizon).
type Draft struct {
	Stage  model.Stage
	Status string
	Date   model.Date
	CumAt  float64
}

// FromResults converts phenology results and fills unreached stages.
func FromResults(rs []phenology.StageResult) []Draft {
	m := make(map[model.Stage]phenology.StageResult, len(rs))
	for _, r := range rs {
		m[r.Stage] = r
	}
	out := make([]Draft, 0, len(model.OrderedStages))
	for _, st := range model.OrderedStages {
		if r, ok := m[st]; ok {
			out = append(out, Draft{Stage: st, Status: r.Status, Date: r.Date, CumAt: r.CumAt})
		} else {
			out = append(out, Draft{Stage: st, Status: phenology.StatusForecast})
		}
	}
	return out
}

// Change is a detected stage date movement.
type Change struct {
	PlotID  int64
	Stage   model.Stage
	OldDate *model.Date
	NewDate *model.Date
	Reason  string
}

// Diff compares the new draft vector against the previously persisted dates.
// oldDate maps stage -> previously known date; absent/nil means "unknown".
// A movement in either direction (earlier correction, later correction,
// unknown -> known, known -> unknown) is emitted; equal dates produce nothing.
func Diff(plotID int64, next []Draft, oldDate map[model.Stage]model.Date, oldKnown map[model.Stage]bool, reason string) []Change {
	var out []Change
	for _, d := range next {
		var oldPtr, newPtr *model.Date
		if oldKnown[d.Stage] {
			od := oldDate[d.Stage]
			oldPtr = &od
		}
		if !d.Date.IsZero() {
			nd := d.Date
			newPtr = &nd
		}
		if oldPtr == nil && newPtr == nil {
			continue
		}
		if oldPtr != nil && newPtr != nil && oldPtr.Equal(*newPtr) {
			continue
		}
		out = append(out, Change{
			PlotID:  plotID,
			Stage:   d.Stage,
			OldDate: oldPtr,
			NewDate: newPtr,
			Reason:  reason,
		})
	}
	return out
}
