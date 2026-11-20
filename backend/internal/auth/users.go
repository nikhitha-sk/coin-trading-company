package auth

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nikhitha-sk/coin-trading-company/backend/internal/ledger"
)

// SignupGrant is the number of coins every new player starts with.
const SignupGrant int64 = 10_000

var (
	ErrInvalidName  = errors.New("auth: name must be 3-20 characters: letters, digits, _ or -")
	ErrNameTaken    = errors.New("auth: name already taken")
	ErrUserNotFound = errors.New("auth: user not found")
)

var nameRe = regexp.MustCompile(`^[A-Za-z0-9_-]{3,20}$`)

// User is a player and their spendable balance.
type User struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	Balance   int64     `json:"balance"`
	CreatedAt time.Time `json:"created_at"`
}

// Service manages players.
type Service struct {
	db     *pgxpool.Pool
	ledger *ledger.Ledger
}

// NewService creates the user service.
func NewService(db *pgxpool.Pool, l *ledger.Ledger) *Service {
	return &Service{db: db, ledger: l}
}

// DevLogin finds or creates a player by name (development only, no password).
// New players get a wallet and a SignupGrant recorded as a ledger entry from system:mint.
func (s *Service) DevLogin(ctx context.Context, name string) (u User, created bool, err error) {
	if !nameRe.MatchString(name) {
		return User{}, false, ErrInvalidName
	}
	return s.findOrCreate(ctx, "dev", strings.ToLower(name), name)
}

func (s *Service) findOrCreate(ctx context.Context, provider, externalID, name string) (User, bool, error) {
	var userID int64
	created := false

	err := pgx.BeginTxFunc(ctx, s.db, pgx.TxOptions{}, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			INSERT INTO users (oauth_provider, oauth_id, name) VALUES ($1, $2, $3)
			ON CONFLICT (oauth_provider, oauth_id) DO NOTHING
			RETURNING id`, provider, externalID, name).Scan(&userID)
		if errors.Is(err, pgx.ErrNoRows) {
			// Existing player (a concurrent login of the same new player also lands here).
			return tx.QueryRow(ctx, `SELECT id FROM users WHERE oauth_provider = $1 AND oauth_id = $2`,
				provider, externalID).Scan(&userID)
		}
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.ConstraintName == "users_name_key" {
				return ErrNameTaken
			}
			return fmt.Errorf("insert user: %w", err)
		}

		created = true
		accountID, err := s.ledger.OpenUserAccount(ctx, tx, userID)
		if err != nil {
			return err
		}
		_, err = s.ledger.Post(ctx, tx, ledger.KindSignup,
			ledger.Entry{AccountID: s.ledger.Sys.Mint, Amount: -SignupGrant},
			ledger.Entry{AccountID: accountID, Amount: SignupGrant})
		return err
	})
	if err != nil {
		return User{}, false, err
	}
	u, err := s.Get(ctx, userID)
	return u, created, err
}

// Get loads a player with their current balance.
func (s *Service) Get(ctx context.Context, userID int64) (User, error) {
	var u User
	err := s.db.QueryRow(ctx, `
		SELECT u.id, u.name, w.balance, u.created_at
		FROM users u JOIN wallets w ON w.user_id = u.id
		WHERE u.id = $1`, userID).Scan(&u.ID, &u.Name, &u.Balance, &u.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrUserNotFound
	}
	return u, err
}
