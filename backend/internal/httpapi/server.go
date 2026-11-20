// Package httpapi wires HTTP routes to the domain services.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/nikhitha-sk/coin-trading-company/backend/internal/auth"
	"github.com/nikhitha-sk/coin-trading-company/backend/internal/config"
	"github.com/nikhitha-sk/coin-trading-company/backend/internal/market"
)

// Deps are the shared dependencies the HTTP layer needs.
type Deps struct {
	Config   config.Config
	DB       *pgxpool.Pool
	Redis    *redis.Client // optional in tests
	Log      *slog.Logger
	Users    *auth.Service
	Market   *market.Service
	Sessions *auth.Sessions
}

type server struct {
	Deps
}

// NewServer builds the router with all routes registered.
func NewServer(d Deps) http.Handler {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	s := &server{Deps: d}

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(15 * time.Second))

	r.Get("/", s.handleIndex)
	r.Get("/healthz", s.handleHealth)

	r.Get("/cards", s.handleListCards)
	r.Get("/cards/{ref}", s.handleGetCard)
	r.Get("/quote", s.handleQuote)
	r.Post("/logout", s.handleLogout)

	r.Group(func(r chi.Router) {
		r.Use(s.requireUser)
		r.Get("/me", s.handleMe)
		r.Get("/me/holdings", s.handleHoldings)
		r.Post("/trades", s.handleTrade)
	})

	// Development-only helpers. Never registered in prod.
	if d.Config.IsDev() {
		r.Post("/dev/login", s.handleDevLogin)
		r.Get("/dev/reconcile", s.handleReconcile)
	}
	return r
}

type ctxKey int

const userIDKey ctxKey = 1

func (s *server) requireUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := s.Sessions.UserID(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, "unauthorized", "log in first")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userIDKey, id)))
	})
}

func userID(r *http.Request) int64 { return r.Context().Value(userIDKey).(int64) }

// handleIndex lists the endpoints, so opening the API in a browser isn't a bare 404.
// (The web UI arrives in Phase 2.)
func (s *server) handleIndex(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"name":   "Coin Trading Company API",
		"status": "running",
		"note":   "This is the backend API only. The web UI comes in Phase 2.",
		"try_in_browser": []string{
			"/healthz",
			"/cards",
			"/cards/golden-monkey",
			"/quote?card=golden-monkey&side=buy&qty=10",
			"/dev/reconcile",
		},
		"needs_login": []string{"POST /dev/login", "GET /me", "GET /me/holdings", "POST /trades"},
	})
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
	} else {
		status["redis"] = "not configured"
	}
	writeJSON(w, code, status)
}

func (s *server) handleDevLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if !decode(w, r, &body) {
		return
	}
	u, created, err := s.Users.DevLogin(r.Context(), body.Name)
	if err != nil {
		s.writeDomainError(w, r, err)
		return
	}
	s.Sessions.Issue(w, u.ID)
	code := http.StatusOK
	if created {
		code = http.StatusCreated
	}
	writeJSON(w, code, u)
}

func (s *server) handleLogout(w http.ResponseWriter, _ *http.Request) {
	s.Sessions.Clear(w)
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) handleMe(w http.ResponseWriter, r *http.Request) {
	u, err := s.Users.Get(r.Context(), userID(r))
	if err != nil {
		s.writeDomainError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, u)
}

func (s *server) handleHoldings(w http.ResponseWriter, r *http.Request) {
	h, err := s.Market.Holdings(r.Context(), userID(r))
	if err != nil {
		s.writeDomainError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, h)
}

func (s *server) handleListCards(w http.ResponseWriter, r *http.Request) {
	cards, err := s.Market.ListCards(r.Context())
	if err != nil {
		s.writeDomainError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, cards)
}

func (s *server) handleGetCard(w http.ResponseWriter, r *http.Request) {
	c, err := s.Market.GetCard(r.Context(), chi.URLParam(r, "ref"))
	if err != nil {
		s.writeDomainError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

// GET /quote?card=golden-monkey&side=buy&qty=5
func (s *server) handleQuote(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	qty, ok := parseInt(q.Get("qty"))
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_qty", "qty must be a whole number")
		return
	}
	quote, err := s.Market.Quote(r.Context(), q.Get("card"), market.Side(q.Get("side")), qty)
	if err != nil {
		s.writeDomainError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, quote)
}

// POST /trades {"card_id":1,"side":"buy","qty":3,"idempotency_key":"<uuid>"}
// The key may also be sent as an Idempotency-Key header.
func (s *server) handleTrade(w http.ResponseWriter, r *http.Request) {
	var body struct {
		CardID         int64  `json:"card_id"`
		Side           string `json:"side"`
		Qty            int64  `json:"qty"`
		IdempotencyKey string `json:"idempotency_key"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.IdempotencyKey == "" {
		body.IdempotencyKey = r.Header.Get("Idempotency-Key")
	}
	t, err := s.Market.ExecuteTrade(r.Context(), market.TradeRequest{
		UserID: userID(r), CardID: body.CardID, Side: market.Side(body.Side),
		Qty: body.Qty, IdempotencyKey: body.IdempotencyKey,
	})
	if err != nil {
		s.writeDomainError(w, r, err)
		return
	}
	code := http.StatusCreated
	if t.Replayed {
		code = http.StatusOK
	}
	writeJSON(w, code, t)
}

func (s *server) handleReconcile(w http.ResponseWriter, r *http.Request) {
	rep, err := s.Market.Reconcile(r.Context())
	if err != nil {
		s.writeDomainError(w, r, err)
		return
	}
	code := http.StatusOK
	if !rep.OK {
		code = http.StatusInternalServerError
	}
	writeJSON(w, code, rep)
}

// writeDomainError maps service errors to clean HTTP responses.
func (s *server) writeDomainError(w http.ResponseWriter, r *http.Request, err error) {
	type mapping struct {
		target error
		status int
		code   string
	}
	for _, m := range []mapping{
		{auth.ErrInvalidName, http.StatusBadRequest, "invalid_name"},
		{auth.ErrNameTaken, http.StatusConflict, "name_taken"},
		{auth.ErrUserNotFound, http.StatusNotFound, "user_not_found"},
		{market.ErrInvalidQty, http.StatusBadRequest, "invalid_qty"},
		{market.ErrInvalidSide, http.StatusBadRequest, "invalid_side"},
		{market.ErrInvalidKey, http.StatusBadRequest, "invalid_idempotency_key"},
		{market.ErrTradeTooSmall, http.StatusBadRequest, "trade_too_small"},
		{market.ErrCardNotFound, http.StatusNotFound, "card_not_found"},
		{market.ErrInsufficientFunds, http.StatusPaymentRequired, "insufficient_funds"},
		{market.ErrInsufficientHoldings, http.StatusConflict, "insufficient_holdings"},
		{market.ErrNotEnoughSupply, http.StatusConflict, "insufficient_holdings"},
		{market.ErrSupplyExceeded, http.StatusConflict, "supply_exceeded"},
		{market.ErrIdempotencyKeyReused, http.StatusConflict, "idempotency_key_reused"},
		{market.ErrOverflow, http.StatusBadRequest, "amount_too_large"},
	} {
		if errors.Is(err, m.target) {
			writeError(w, m.status, m.code, err.Error())
			return
		}
	}
	s.Log.Error("request failed", "path", r.URL.Path, "request_id", middleware.GetReqID(r.Context()), "err", err)
	writeError(w, http.StatusInternalServerError, "internal", "something went wrong")
}

func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return false
	}
	return true
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": msg}})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
