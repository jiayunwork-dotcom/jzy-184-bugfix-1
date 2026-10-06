// Package model contains the shared domain types of the heat-unit service.
//
// All calendar dates are handled as civil dates in UTC. Time zones never enter
// the calculation: a station row for "2026-05-01" denotes that calendar day,
// and every day is treated as a full 24h day. This matters because the
// accumulations are sums of whole-day contributions.
package model

import (
	"database/sql/driver"
	"fmt"
	"time"
)

// Date is a calendar date without a time component.
type Date struct {
	y int
	m time.Month
	d int
}

// NewDate constructs a Date from year/month/day.
func NewDate(y int, m time.Month, d int) Date {
	return Date{y: y, m: m, d: d}
}

// ParseDate parses an ISO "YYYY-MM-DD" date.
func ParseDate(s string) (Date, error) {
	t, err := time.ParseInLocation("2006-01-02", s, time.UTC)
	if err != nil {
		return Date{}, err
	}
	return DateFromTime(t), nil
}

// DateFromTime converts a time.Time (read in UTC) to a Date.
func DateFromTime(t time.Time) Date {
	u := t.UTC()
	return Date{y: u.Year(), m: u.Month(), d: u.Day()}
}

// Today returns the current UTC calendar date.
func Today() Date { return DateFromTime(time.Now().UTC()) }

// Time returns midnight UTC of the date.
func (d Date) Time() time.Time {
	return time.Date(d.y, d.m, d.d, 0, 0, 0, 0, time.UTC)
}

// String renders ISO format.
func (d Date) String() string {
	return d.Time().Format("2006-01-02")
}

// AddDays returns the date n days later (n may be negative).
func (d Date) AddDays(n int) Date { return DateFromTime(d.Time().AddDate(0, 0, n)) }

// Sub returns the number of whole days d - o.
func (d Date) Sub(o Date) int {
	return int(d.Time().Sub(o.Time()).Hours() / 24)
}

// Before reports whether d is strictly before o.
func (d Date) Before(o Date) bool { return d.Sub(o) < 0 }

// After reports whether d is strictly after o.
func (d Date) After(o Date) bool { return d.Sub(o) > 0 }

// Equal compares two dates.
func (d Date) Equal(o Date) bool { return d.Sub(o) == 0 }

// Weekday mirrors time.Time.Weekday.
func (d Date) Weekday() time.Weekday { return d.Time().Weekday() }

// Year returns the year.
func (d Date) Year() int { return d.y }

// Month returns the month.
func (d Date) Month() time.Month { return d.m }

// Day returns the day.
func (d Date) Day() int { return d.d }

// MarshalJSON renders the date as a JSON string, or null for the zero date
// (e.g. a stage that has not been reached yet).
func (d Date) MarshalJSON() ([]byte, error) {
	if d.IsZero() {
		return []byte("null"), nil
	}
	return []byte(`"` + d.String() + `"`), nil
}

// UnmarshalJSON parses a JSON string date (or null into the zero date).
func (d *Date) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		*d = Date{}
		return nil
	}
	if len(b) < 2 || b[0] != '"' || b[len(b)-1] != '"' {
		return fmt.Errorf("date must be a JSON string")
	}
	v, err := ParseDate(string(b[1 : len(b)-1]))
	if err != nil {
		return err
	}
	*d = v
	return nil
}

// IsZero reports whether the date is the zero value.
func (d Date) IsZero() bool { return d.y == 0 }

// Value implements driver.Valuer so pgx can bind model.Date to a DATE column.
func (d Date) Value() (driver.Value, error) {
	if d.IsZero() {
		return nil, nil
	}
	return d.Time(), nil
}

// Scan implements sql.Scanner; pgx decodes a DATE into time.Time.
func (d *Date) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		*d = Date{}
	case time.Time:
		*d = DateFromTime(v)
	case string:
		parsed, err := ParseDate(v)
		if err != nil {
			return err
		}
		*d = parsed
	default:
		return fmt.Errorf("Date.Scan: unsupported type %T", src)
	}
	return nil
}

// Stage is one of the five growth stages tracked by the service.
type Stage string

const (
	StageEmergence Stage = "emergence" // 出苗
	StageJointing  Stage = "jointing"  // 拔节
	StageTasseling Stage = "tasseling" // 抽雄
	StageSilking   Stage = "silking"   // 吐丝
	StageMaturity  Stage = "maturity"  // 成熟
)

// OrderedStages is the fixed, strictly increasing order of stages.
var OrderedStages = []Stage{
	StageEmergence,
	StageJointing,
	StageTasseling,
	StageSilking,
	StageMaturity,
}

// StageCN maps stages to their Chinese names for output.
var StageCN = map[Stage]string{
	StageEmergence: "出苗",
	StageJointing:  "拔节",
	StageTasseling: "抽雄",
	StageSilking:   "吐丝",
	StageMaturity:  "成熟",
}

// Variety defines a maize cultivar's thermal parameters and stage requirements.
type Variety struct {
	ID           int64
	Name         string
	BaseTemp     float64 // 基点温度 °C
	CeilingTemp  float64 // 上限温度 °C
	StageRequire map[Stage]float64
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// Station is an automatic weather station.
type Station struct {
	ID        int64
	Code      string
	Name      string
	Lat       float64
	Lon       float64
	Elev      float64
	CreatedAt time.Time
}

// Binding maps a plot to a station for an interval of dates [EffectiveFrom, EffectiveTo).
// EffectiveTo is zero when the binding is still current.
type Binding struct {
	PlotID        int64
	StationID     int64
	EffectiveFrom Date
	EffectiveTo   Date // zero = open ended
}

// Plot is a parcel sown with one variety on a fixed sowing date.
type Plot struct {
	ID         int64
	Code       string
	Name       string
	VarietyID  int64
	SowingDate Date
	Bindings   []Binding // ordered by EffectiveFrom, non-overlapping
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// StationAt returns the station bound to the plot on date d.
// Bindings must be ordered by EffectiveFrom (as loaded by the store).
func (p *Plot) StationAt(d Date) (int64, bool) {
	var station int64
	found := false
	for _, b := range p.Bindings {
		if d.Before(b.EffectiveFrom) {
			break
		}
		if !b.EffectiveTo.IsZero() && !d.Before(b.EffectiveTo) {
			continue
		}
		station, found = b.StationID, true
	}
	return station, found
}

// Observation is one reported daily row from a station.
type Observation struct {
	StationID int64
	Date      Date
	TMax      float64
	TMin      float64
	Seq       int64 // 上报序号: larger wins
}

const (
	// SourceObs marks a value actually reported by a station.
	SourceObs = "obs"
	// SourceFillStation marks a value filled from the same station's climatology.
	SourceFillStation = "fill_station"
	// SourceFillNeighbor marks a value filled by distance/elevation-adjusted neighbors.
	SourceFillNeighbor = "fill_neighbor"
	// SourceClimate marks a future-day value extrapolated from climatology;
	// such rows are never persisted in the station table.
	SourceClimate = "climate"
	// SourceMissing marks a past day with neither observation nor any fill.
	SourceMissing = "missing"
	// SourceUnbound marks a day when the plot has no bound station.
	SourceUnbound = "unbound"
)

// IsFilled reports whether a source tag denotes an imputed (not observed) value.
func IsFilled(source string) bool {
	return source == SourceFillStation || source == SourceFillNeighbor
}

// WeatherDay is the authoritative daily temperature row for a station/date.
type WeatherDay struct {
	StationID int64
	Date      Date
	TMax      float64
	TMin      float64
	Source    string
	Seq       int64 // winning observation seq; 0 for fills
	Basis     []WeatherBasis
}

// WeatherBasis records that a filled row depends on an observed neighbor row,
// so that observation changes can trigger cascading re-fills.
type WeatherBasis struct {
	StationID int64
	Date      Date
}

// Snapshot is a persisted cumulative GDD checkpoint for a plot.
type Snapshot struct {
	PlotID int64
	Date   Date
	CumGDD float64
}

// PlotDay is one row of a plot's daily heat-unit curve.
type PlotDay struct {
	Date        Date    `json:"date"`
	StationID   int64   `json:"-"`
	StationCode string  `json:"station_code,omitempty"`
	TMax        float64 `json:"tmax,omitempty"`
	TMin        float64 `json:"tmin,omitempty"`
	DailyGDD    float64 `json:"daily_gdd"`
	CumGDD      float64 `json:"cum_gdd"`
	Source      string  `json:"source"` // obs / fill_station / fill_neighbor / climate
}

// StageState is the current answer for one stage of a plot.
type StageState struct {
	Stage    Stage   `json:"stage"`
	NameCN   string  `json:"name_cn"`
	Status   string  `json:"status"` // reached | forecast
	Date     Date    `json:"date"`
	Require  float64 `json:"require_gdd"`
	CumAtDay float64 `json:"cum_gdd_at_date"`
}

// Change describes what triggered a recompute, for event attribution.
type Change struct {
	StationID int64
	Date      Date
	Kind      string // insert | correct | supersede_ignored | rebind | variety | fill
	Detail    string
}

func (c Change) String() string {
	switch {
	case c.StationID != 0 && !c.Date.IsZero():
		return fmt.Sprintf("station %d %s data %s (%s)", c.StationID, c.Date, c.Kind, c.Detail)
	case c.StationID != 0:
		return fmt.Sprintf("station %d data %s (%s)", c.StationID, c.Kind, c.Detail)
	default:
		return fmt.Sprintf("%s %s", c.Kind, c.Detail)
	}
}

// StageEvent is an emitted change notification for the mini program.
type StageEvent struct {
	ID        int64     `json:"id"`
	PlotID    int64     `json:"plot_id"`
	Stage     Stage     `json:"stage"`
	NameCN    string    `json:"name_cn"`
	OldDate   *Date     `json:"old_date"`
	NewDate   *Date     `json:"new_date"`
	Reason    string    `json:"reason"`
	CreatedAt time.Time `json:"created_at"`
}
