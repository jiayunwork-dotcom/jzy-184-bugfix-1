// Command agriheat runs the heat-unit accumulation HTTP service and the
// background recomputation worker.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"agriheat/internal/api"
	"agriheat/internal/service"
	"agriheat/internal/store"
	"agriheat/internal/worker"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	dsn := env("DATABASE_URL",
		"postgres://agri:agri@localhost:5432/agriheat?sslmode=disable")
	addr := env("HTTP_ADDR", ":8080")

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	db, err := store.Open(ctx, dsn)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer db.Close()

	svc := service.New(db)
	h := &api.Handler{Svc: svc, DB: db}
	router := api.NewRouter(h)

	srv := &http.Server{
		Addr:              addr,
		Handler:           router,
		ReadHeaderTimeout: 10 * time.Second,
	}

	w := worker.New(db, 0)
	go func() {
		log.Printf("worker started")
		w.Run(ctx)
	}()

	go func() {
		log.Printf("http listening on %s", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("http: %v", err)
		}
	}()

	<-ctx.Done()
	log.Printf("shutting down")
	shCtx, shCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shCancel()
	_ = srv.Shutdown(shCtx)
}
