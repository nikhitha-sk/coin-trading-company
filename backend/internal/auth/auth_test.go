package auth_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/nikhitha-sk/coin-trading-company/backend/internal/auth"
	"github.com/nikhitha-sk/coin-trading-company/backend/internal/ledger"
	"github.com/nikhitha-sk/coin-trading-company/backend/internal/testutil"
)

func TestSessionRoundTripAndTamper(t *testing.T) {
	s := auth.NewSessions("test-secret", false)
	rec := httptest.NewRecorder()
	s.Issue(rec, 42)
	cookie := rec.Result().Cookies()[0]

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(cookie)
	if id, ok := s.UserID(req); !ok || id != 42 {
		t.Fatalf("UserID = %d, %v; want 42, true", id, ok)
	}

	tampered := *cookie
	tampered.Value = "NDM6OTk5OTk5OTk5OQ" + cookie.Value[len("NDI6"):] // try to swap the user id
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&tampered)
	if _, ok := s.UserID(req); ok {
		t.Fatal("tampered cookie was accepted")
	}

	other := auth.NewSessions("different-secret", false)
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(cookie)
	if _, ok := other.UserID(req); ok {
		t.Fatal("cookie signed with another secret was accepted")
	}
}

func TestDevLoginGrantsCoinsOnlyOnce(t *testing.T) {
	pool := testutil.NewDB(t)
	ctx := context.Background()
	l, err := ledger.Load(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	svc := auth.NewService(pool, l)

	// 10 concurrent first logins for the same new player: one account, one grant.
	var wg sync.WaitGroup
	ids := make([]int64, 10)
	for i := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			u, _, err := svc.DevLogin(ctx, "Alice")
			if err != nil {
				t.Error(err)
				return
			}
			ids[i] = u.ID
		}()
	}
	wg.Wait()
	for _, id := range ids {
		if id != ids[0] {
			t.Fatalf("different user ids for the same name: %v", ids)
		}
	}

	u, created, err := svc.DevLogin(ctx, "alice") // case-insensitive
	if err != nil || created || u.ID != ids[0] {
		t.Fatalf("second login: %+v created=%v err=%v", u, created, err)
	}
	if u.Balance != auth.SignupGrant {
		t.Fatalf("balance = %d, want %d", u.Balance, auth.SignupGrant)
	}

	r, err := l.Reconcile(ctx, pool)
	if err != nil || !r.OK || r.Minted != auth.SignupGrant {
		t.Fatalf("reconcile: %+v err=%v", r, err)
	}
}

func TestDevLoginValidatesName(t *testing.T) {
	pool := testutil.NewDB(t)
	l, _ := ledger.Load(context.Background(), pool)
	svc := auth.NewService(pool, l)
	for _, bad := range []string{"", "ab", "has space", "way-too-long-name-for-this-game", "drop;table"} {
		if _, _, err := svc.DevLogin(context.Background(), bad); !errors.Is(err, auth.ErrInvalidName) {
			t.Errorf("DevLogin(%q) err = %v, want ErrInvalidName", bad, err)
		}
	}
}
