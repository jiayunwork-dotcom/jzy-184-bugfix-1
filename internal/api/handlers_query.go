package api

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"agriheat/internal/model"
	"agriheat/internal/store"
)

// GetPlot GET /plots/:id returns metadata, bindings and stage states.
func (h *Handler) GetPlot(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		badRequest(c, "bad plot id")
		return
	}
	ctx := c.Request.Context()
	t, err := h.DB.Begin(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	defer t.Rollback()
	plot, err := t.GetPlot(id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "plot not found"})
		return
	}
	variety, err := t.GetVariety(plot.VarietyID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	stages, err := t.ListStages(id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	bindings := make([]gin.H, 0, len(plot.Bindings))
	for _, b := range plot.Bindings {
		row := gin.H{"station_id": b.StationID, "effective_from": b.EffectiveFrom}
		if !b.EffectiveTo.IsZero() {
			row["effective_to"] = b.EffectiveTo
		}
		bindings = append(bindings, row)
	}
	c.JSON(http.StatusOK, gin.H{
		"id":           plot.ID,
		"code":         plot.Code,
		"name":         plot.Name,
		"variety_id":   plot.VarietyID,
		"variety_name": variety.Name,
		"sowing_date":  plot.SowingDate,
		"bindings":     bindings,
		"stages":       stages,
	})
}

// PlotDays GET /plots/:id/days?from=&to= returns the daily GDD curve.
func (h *Handler) PlotDays(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		badRequest(c, "bad plot id")
		return
	}
	ctx := c.Request.Context()
	t, err := h.DB.Begin(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	plot, err := t.GetPlot(id)
	if err != nil {
		t.Rollback()
		c.JSON(http.StatusNotFound, gin.H{"error": "plot not found"})
		return
	}
	if err := t.Rollback(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	from := plot.SowingDate
	to := h.Svc.Now().AddDays(ForecastQueryDays)
	if v := c.Query("from"); v != "" {
		from, err = model.ParseDate(v)
		if err != nil {
			badRequest(c, "from must be YYYY-MM-DD")
			return
		}
	}
	if v := c.Query("to"); v != "" {
		to, err = model.ParseDate(v)
		if err != nil {
			badRequest(c, "to must be YYYY-MM-DD")
			return
		}
	}
	if from.Before(plot.SowingDate) {
		badRequest(c, "query date before sowing date")
		return
	}
	days, err := store.QueryPlotDays(ctx, h.DB, id, from, to)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	stages, err := store.QueryStages(ctx, h.DB, id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"plot_id": id, "days": days, "stages": stages})
}

// ForecastQueryDays is the default future horizon for the day-curve query.
const ForecastQueryDays = 366

// Events GET /events?after_id=&limit= pulls change events in ascending order.
func (h *Handler) Events(c *gin.Context) {
	afterID, _ := strconv.ParseInt(c.DefaultQuery("after_id", "0"), 10, 64)
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "200"))
	evs, err := store.QueryEvents(c.Request.Context(), h.DB, afterID, limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"events": evs})
}
