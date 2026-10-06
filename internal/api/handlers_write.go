package api

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"agriheat/internal/model"
	"agriheat/internal/service"
)

// MaxBatchRows caps a batch at a few thousand rows.
const MaxBatchRows = 5000

type stationReq struct {
	Code string  `json:"code" binding:"required"`
	Name string  `json:"name"`
	Lat  float64 `json:"lat"`
	Lon  float64 `json:"lon"`
	Elev float64 `json:"elev"`
}

// CreateStation POST /stations
func (h *Handler) CreateStation(c *gin.Context) {
	var req stationReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err.Error())
		return
	}
	id, err := h.Svc.RegisterStation(c.Request.Context(), &model.Station{
		Code: req.Code, Name: req.Name, Lat: req.Lat, Lon: req.Lon, Elev: req.Elev,
	})
	if err != nil {
		badRequest(c, err.Error())
		return
	}
	c.JSON(http.StatusCreated, gin.H{"id": id})
}

type varietyReq struct {
	Name         string             `json:"name" binding:"required"`
	BaseTemp     float64            `json:"base_temp"`
	CeilingTemp  float64            `json:"ceiling_temp"`
	StageRequire map[string]float64 `json:"stage_require"`
}

// CreateVariety POST /varieties
func (h *Handler) CreateVariety(c *gin.Context) {
	var req varietyReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err.Error())
		return
	}
	v := &model.Variety{
		Name: req.Name, BaseTemp: req.BaseTemp, CeilingTemp: req.CeilingTemp,
		StageRequire: map[model.Stage]float64{},
	}
	for _, st := range model.OrderedStages {
		val, ok := req.StageRequire[string(st)]
		if !ok {
			// also accept Chinese keys
			if v2, ok2 := req.StageRequire[model.StageCN[st]]; ok2 {
				val = v2
				ok = true
			}
		}
		if !ok {
			badRequest(c, "stage_require missing "+string(st))
			return
		}
		v.StageRequire[st] = val
	}
	id, err := h.Svc.RegisterVariety(c.Request.Context(), v)
	if err != nil {
		badRequest(c, err.Error())
		return
	}
	c.JSON(http.StatusCreated, gin.H{"id": id})
}

type plotReq struct {
	Code       string `json:"code" binding:"required"`
	Name       string `json:"name"`
	VarietyID  int64  `json:"variety_id"`
	StationID  int64  `json:"station_id"`
	SowingDate string `json:"sowing_date"`
}

// CreatePlot POST /plots
func (h *Handler) CreatePlot(c *gin.Context) {
	var req plotReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err.Error())
		return
	}
	if req.VarietyID == 0 || req.StationID == 0 {
		badRequest(c, "variety_id and station_id are required")
		return
	}
	sd, err := model.ParseDate(req.SowingDate)
	if err != nil {
		badRequest(c, "sowing_date must be YYYY-MM-DD")
		return
	}
	if sd.After(h.Svc.Now()) {
		badRequest(c, "sowing date cannot be in the future")
		return
	}
	id, err := h.Svc.RegisterPlot(c.Request.Context(), &model.Plot{
		Code: req.Code, Name: req.Name, VarietyID: req.VarietyID, SowingDate: sd,
	}, req.StationID)
	if err != nil {
		badRequest(c, err.Error())
		return
	}
	c.JSON(http.StatusCreated, gin.H{"id": id})
}

type rebindReq struct {
	StationID int64  `json:"station_id"`
	From      string `json:"from"`
}

// Rebind POST /plots/:id/rebind
func (h *Handler) Rebind(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		badRequest(c, "bad plot id")
		return
	}
	var req rebindReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err.Error())
		return
	}
	if req.StationID == 0 {
		badRequest(c, "station_id is required")
		return
	}
	from := h.Svc.Now()
	if req.From != "" {
		from, err = model.ParseDate(req.From)
		if err != nil {
			badRequest(c, "from must be YYYY-MM-DD")
			return
		}
	}
	if err := h.Svc.Rebind(c.Request.Context(), id, req.StationID, from); err != nil {
		badRequest(c, err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

type batchReq struct {
	Rows []service.ObservationIn `json:"rows" binding:"required"`
}

// BatchObservations POST /observations/batch
func (h *Handler) BatchObservations(c *gin.Context) {
	var req batchReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err.Error())
		return
	}
	if len(req.Rows) == 0 {
		badRequest(c, "rows must not be empty")
		return
	}
	if len(req.Rows) > MaxBatchRows {
		badRequest(c, "batch exceeds "+strconv.Itoa(MaxBatchRows)+" rows")
		return
	}
	res, err := h.Svc.IngestObservations(c.Request.Context(), req.Rows)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, res)
}
