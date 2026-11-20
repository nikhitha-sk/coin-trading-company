package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"testing"

	"github.com/nikhitha-sk/coin-trading-company/backend/internal/auth"
	"github.com/nikhitha-sk/coin-trading-company/backend/internal/config"
	"github.com/nikhitha-sk/coin-trading-company/backend/internal/httpapi"
	"github.com/nikhitha-sk/coin-trading-company/backend/internal/ledger"
	"github.com/nikhitha-sk/coin-trading-company/backend/internal/market"
	"github.com/nikhitha-sk/coin-trading-company/backend/internal/testutil"
)

func newTestServer(t *testing.T, env string) *httptest.Server {
	t.Helper()
	pool := testutil.NewDB(t)
	l, err := ledger.Load(context.Background(), pool)
	if err != nil {
		t.Fatal(err)
	}
	h := httpapi.NewServer(httpapi.Deps{
		Config:   config.Config{AppEnv: env},
		DB:       pool,
		Users:    auth.NewService(pool, l),
		Market:   market.NewService(pool, l),
		Sessions: auth.NewSessions("test-secret", false),
	})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

type client struct {
	t    *testing.T
	base string
	http *http.Client
}

func newClient(t *testing.T, base string) *client {
	jar, _ := cookiejar.New(nil)
	return &client{t: t, base: base, http: &http.Client{Jar: jar}}
}

func (c *client) do(method, path string, body any, out any) int {
	c.t.Helper()
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req, _ := http.NewRequest(method, c.base+path, &buf)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		_ = json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode
}

func TestTradingOverHTTP(t *testing.T) {
	srv := newTestServer(t, "dev")
	alice := newClient(t, srv.URL)

	if code := alice.do("GET", "/me", nil, nil); code != http.StatusUnauthorized {
		t.Fatalf("GET /me before login = %d, want 401", code)
	}

	var me auth.User
	if code := alice.do("POST", "/dev/login", map[string]string{"name": "alice"}, &me); code != http.StatusCreated {
		t.Fatalf("login = %d", code)
	}
	if me.Balance != auth.SignupGrant {
		t.Fatalf("balance = %d", me.Balance)
	}

	var card market.Card
	if code := alice.do("GET", "/cards/golden-monkey", nil, &card); code != http.StatusOK {
		t.Fatalf("get card = %d", code)
	}

	var quote market.Quote
	if code := alice.do("GET", "/quote?card=golden-monkey&side=buy&qty=5", nil, &quote); code != http.StatusOK {
		t.Fatalf("quote = %d", code)
	}

	order := map[string]any{"card_id": card.ID, "side": "buy", "qty": 5, "idempotency_key": "click-0001"}
	var trade market.Trade
	if code := alice.do("POST", "/trades", order, &trade); code != http.StatusCreated {
		t.Fatalf("trade = %d", code)
	}
	if trade.Total != quote.Total {
		t.Fatalf("executed total %d != quoted %d", trade.Total, quote.Total)
	}

	// Double-click: same key returns the same trade with 200 and charges nothing extra.
	var again market.Trade
	if code := alice.do("POST", "/trades", order, &again); code != http.StatusOK || again.ID != trade.ID || !again.Replayed {
		t.Fatalf("replay = %d %+v", code, again)
	}

	alice.do("GET", "/me", nil, &me)
	if me.Balance != auth.SignupGrant-trade.Total {
		t.Fatalf("balance = %d, want %d", me.Balance, auth.SignupGrant-trade.Total)
	}

	var holdings []market.Holding
	alice.do("GET", "/me/holdings", nil, &holdings)
	if len(holdings) != 1 || holdings[0].Qty != 5 {
		t.Fatalf("holdings = %+v", holdings)
	}

	var errBody struct {
		Error struct{ Code string } `json:"error"`
	}
	tooMuch := map[string]any{"card_id": card.ID, "side": "sell", "qty": 6, "idempotency_key": "click-0002"}
	if code := alice.do("POST", "/trades", tooMuch, &errBody); code != http.StatusConflict || errBody.Error.Code != "insufficient_holdings" {
		t.Fatalf("oversell = %d %+v", code, errBody)
	}

	var rep ledger.Report
	if code := alice.do("GET", "/dev/reconcile", nil, &rep); code != http.StatusOK || !rep.OK {
		t.Fatalf("reconcile = %d %+v", code, rep)
	}
}

func TestDevRoutesDisabledInProd(t *testing.T) {
	srv := newTestServer(t, "prod")
	c := newClient(t, srv.URL)
	if code := c.do("POST", "/dev/login", map[string]string{"name": "alice"}, nil); code != http.StatusNotFound {
		t.Fatalf("POST /dev/login in prod = %d, want 404", code)
	}
	if code := c.do("GET", "/dev/reconcile", nil, nil); code != http.StatusNotFound {
		t.Fatalf("GET /dev/reconcile in prod = %d, want 404", code)
	}
}
