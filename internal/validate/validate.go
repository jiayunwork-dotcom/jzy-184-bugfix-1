// Package validate centralizes all input validation rules so that the HTTP
// layer and the store share one definition of "illegal input".
package validate

import (
	"errors"
	"fmt"

	"agriheat/internal/gdd"
	"agriheat/internal/model"
)

// Plausible temperature range in °C. The world's reliably measured extremes
// are roughly -89/+57 °C; [-70,+60] rejects sensor garbage without rejecting
// real records.
const (
	MinPlausibleTemp = -70.0
	MaxPlausibleTemp = 60.0

	// MinBase / MaxBase bound agronomic base temperatures.
	MinBase = -20.0
	MaxBase = 50.0
)

// Err is a field-scoped validation error.
type Err struct {
	Field  string
	Reason string
}

func (e Err) Error() string { return e.Field + ": " + e.Reason }

// Errs is a per-row validation result for batch endpoints.
type Errs []Err

func (e Errs) Error() string {
	if len(e) == 0 {
		return ""
	}
	return e[0].Error()
}

// AsErrs unwraps a validation list.
func AsErrs(err error) (Errs, bool) {
	var ve Errs
	if errors.As(err, &ve) {
		return ve, true
	}
	return nil, false
}

// CheckTemp validates a daily temperature pair.
func CheckTemp(tmax, tmin float64) Errs {
	var out Errs
	if tmax < MinPlausibleTemp || tmax > MaxPlausibleTemp {
		out = append(out, Err{"tmax", fmt.Sprintf("outside plausible range [%.0f,%.0f]", MinPlausibleTemp, MaxPlausibleTemp)})
	}
	if tmin < MinPlausibleTemp || tmin > MaxPlausibleTemp {
		out = append(out, Err{"tmin", fmt.Sprintf("outside plausible range [%.0f,%.0f]", MinPlausibleTemp, MaxPlausibleTemp)})
	}
	if tmin > tmax {
		out = append(out, Err{"tmin", "tmin is higher than tmax"})
	}
	return out
}

// CheckVarietyParams validates thermal parameters and stage requirements.
func CheckVarietyParams(base, ceil float64, req map[model.Stage]float64) Errs {
	var out Errs
	if base < MinBase || base > MaxBase {
		out = append(out, Err{"base_temp", fmt.Sprintf("outside [%.0f,%.0f]", MinBase, MaxBase)})
	}
	if ceil < MinPlausibleTemp || ceil > MaxPlausibleTemp {
		out = append(out, Err{"ceiling_temp", fmt.Sprintf("outside [%.0f,%.0f]", MinPlausibleTemp, MaxPlausibleTemp)})
	}
	if base >= ceil {
		out = append(out, Err{"base_temp", "base temperature must be strictly below ceiling"})
	}
	prev := 0.0
	for i, st := range model.OrderedStages {
		v, ok := req[st]
		if !ok {
			out = append(out, Err{"stage_require", fmt.Sprintf("missing requirement for %s", st)})
			continue
		}
		if v <= 0 {
			out = append(out, Err{"stage_require", fmt.Sprintf("%s requirement must be positive", st)})
		}
		if i > 0 && v <= prev {
			out = append(out, Err{"stage_require", fmt.Sprintf("stage requirements must be strictly increasing (%s <= previous)", st)})
		}
		prev = v
	}
	return out
}

// CheckSowingDate ensures a queried/persisted date is not before sowing.
// The listed illegal case is "sowing date later than query date".
func CheckSowingDate(sowing, query model.Date) error {
	if query.Before(sowing) {
		return fmt.Errorf("query date %s is before sowing date %s", query, sowing)
	}
	return nil
}

// CheckMethod validates a GDD method name.
func CheckMethod(m string) error {
	switch gdd.Method(m) {
	case gdd.MethodSine, gdd.MethodTriangle, gdd.MethodMean:
		return nil
	default:
		return fmt.Errorf("unknown gdd method %q", m)
	}
}
