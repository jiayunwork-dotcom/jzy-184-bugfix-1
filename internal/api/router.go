// Package api exposes the HTTP JSON API used by the mini program.
package api

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"agriheat/internal/service"
	"agriheat/internal/store"
)

// Handler holds dependencies.
type Handler struct {
	Svc *service.Service
	DB  *store.DB
}

// NewRouter wires all routes.
func NewRouter(h *Handler) *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())
	r.GET("/healthz", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })

	api := r.Group("/api/v1")
	{
		api.POST("/stations", h.CreateStation)
		api.POST("/varieties", h.CreateVariety)
		api.POST("/plots", h.CreatePlot)
		api.POST("/plots/:id/rebind", h.Rebind)

		api.POST("/observations/batch", h.BatchObservations)

		api.GET("/plots/:id", h.GetPlot)
		api.GET("/plots/:id/days", h.PlotDays)
		api.GET("/events", h.Events)
	}
	return r
}

func badRequest(c *gin.Context, msg string) {
	c.JSON(http.StatusBadRequest, gin.H{"error": msg})
}
