// Package httpapi wires HTTP routes to the domain services.
package httpapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/nikhitha-sk/coin-trading-company/backend/internal/config"
)

// Deps are the shared dependencies the HTTP layer needs.
type Deps struct {
	Config config.Config
	DB     *pgxpool.Pool
	Redis  *redis.Client
	Log    *slog.Logger
}

type server struct {
	Deps
}

// NewServer builds the router with all routes registered.
func NewServer(d Deps) http.Handler {
	s := &server{Deps: d}

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(15 * time.Second))

	r.Get("/healthz", s.handleHealth)
	return r
}

func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	status := map[string]string{"db": "ok", "redis": "ok"}
	code := http.StatusOK
	if err := s.DB.Ping(ctx); err != nil {
		status["db"] = err.Error()
		code = http.StatusServiceUnavailable
	}
	if s.Redis != nil {
		if err := s.Redis.Ping(ctx).Err(); err != nil {
			status["redis"] = err.Error()
			code = http.StatusServiceUnavailable
		}
	}
	writeJSON(w, code, status)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
