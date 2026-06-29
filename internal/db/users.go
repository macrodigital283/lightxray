package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// User is the in-memory shape of a row in `users`. Times are pointers so
// we can distinguish "never connected" (nil) from a zero-value timestamp.
type User struct {
	UUID            uuid.UUID
	Name            string
	Comment         string
	UsageBytes      int64
	UsageLimitBytes int64
	PackageDays     int
	StartDate       *time.Time // null until first connect
	LastResetTime   time.Time
	LastOnline      *time.Time // null = never
	Enable          bool
	Mode            string
	Lang            string
	AddedByUUID     uuid.UUID
	SerialID        int64
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// ErrNotFound — surface as 404 in the HTTP layer.
var ErrNotFound = errors.New("user not found")

const userCols = `uuid, name, comment, usage_bytes, usage_limit_bytes,
                  package_days, start_date, last_reset_time, last_online,
                  enable, mode, lang, added_by_uuid, serial_id,
                  created_at, updated_at`

func scanUser(row pgx.Row) (User, error) {
	var u User
	err := row.Scan(
		&u.UUID, &u.Name, &u.Comment, &u.UsageBytes, &u.UsageLimitBytes,
		&u.PackageDays, &u.StartDate, &u.LastResetTime, &u.LastOnline,
		&u.Enable, &u.Mode, &u.Lang, &u.AddedByUUID, &u.SerialID,
		&u.CreatedAt, &u.UpdatedAt,
	)
	return u, err
}

// ListUsers returns every user, ordered by serial_id so output is stable.
// At fleet scale (few thousand users per panel) loading the lot is fine —
// the Hiddify pool pulls the full list every usage-sync tick anyway.
func (s *Store) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+userCols+` FROM users ORDER BY serial_id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// GetUser fetches a single user by UUID. ErrNotFound if absent.
func (s *Store) GetUser(ctx context.Context, id uuid.UUID) (User, error) {
	row := s.Pool.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE uuid = $1`, id)
	u, err := scanUser(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrNotFound
	}
	return u, err
}

// CreateUserInput is what handlers pass to CreateUser. UUID is OPTIONAL:
// nil → the server generates a fresh one (the normal "add new user" flow);
// when set (e.g. the pool re-pushing a user onto a rebuilt server for
// disaster recovery) the row is created with that exact UUID so the
// customer's existing key keeps working. Matches real Hiddify, which also
// honors a caller-supplied uuid.
type CreateUserInput struct {
	UUID            *uuid.UUID
	Name            string
	Comment         string
	UsageLimitBytes int64
	PackageDays     int
	Mode            string
	Lang            string
	Enable          bool
	AddedByUUID     uuid.UUID
	// StartDate anchors the expiry clock (start_date + package_days) at creation.
	// nil = the default Hiddify behavior (starts on first connect). The pool
	// sets it so a new key shows its expiry date immediately.
	StartDate *time.Time
}

// CreateUser inserts a new row and returns (user, created, err). When
// in.UUID is nil a fresh UUID is generated; when set it is used verbatim.
// If a row with that UUID already exists the insert is a no-op and the
// existing row is returned with created=false — idempotent, so the pool
// can re-push the same user without clobbering live data (usage, dates,
// serial_id stay as they are locally).
func (s *Store) CreateUser(ctx context.Context, in CreateUserInput) (User, bool, error) {
	id := uuid.New()
	if in.UUID != nil {
		id = *in.UUID
	}
	mode := in.Mode
	if mode == "" {
		mode = "no_reset"
	}
	lang := in.Lang
	if lang == "" {
		lang = "en"
	}
	row := s.Pool.QueryRow(ctx, `
		INSERT INTO users (uuid, name, comment, usage_limit_bytes,
		                   package_days, mode, lang, enable, added_by_uuid, start_date)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (uuid) DO NOTHING
		RETURNING `+userCols,
		id, in.Name, in.Comment, in.UsageLimitBytes,
		in.PackageDays, mode, lang, in.Enable, in.AddedByUUID, in.StartDate,
	)
	u, err := scanUser(row)
	if errors.Is(err, pgx.ErrNoRows) {
		// UUID already present — return the existing row untouched.
		existing, gerr := s.GetUser(ctx, id)
		if gerr != nil {
			return User{}, false, gerr
		}
		return existing, false, nil
	}
	if err != nil {
		return User{}, false, err
	}
	return u, true, nil
}

// PatchUserInput holds only fields a PATCH may change. nil = "don't touch".
type PatchUserInput struct {
	Name            *string
	Comment         *string
	UsageLimitBytes *int64
	PackageDays     *int
	StartDate       *time.Time // re-anchor the expiry clock (DATE column)
	Enable          *bool
	Mode            *string
	Lang            *string
}

// PatchUser applies a partial update. Returns the post-update row.
// ErrNotFound if the uuid doesn't exist.
func (s *Store) PatchUser(ctx context.Context, id uuid.UUID, in PatchUserInput) (User, error) {
	// Build SET clause dynamically. Trivially safe — column names are
	// hard-coded; only the values come from the caller.
	sets := []string{"updated_at = NOW()"}
	args := []any{}
	addSet := func(col string, v any) {
		args = append(args, v)
		sets = append(sets, fmt.Sprintf("%s = $%d", col, len(args)))
	}
	if in.Name != nil {
		addSet("name", *in.Name)
	}
	if in.Comment != nil {
		addSet("comment", *in.Comment)
	}
	if in.UsageLimitBytes != nil {
		addSet("usage_limit_bytes", *in.UsageLimitBytes)
	}
	if in.PackageDays != nil {
		addSet("package_days", *in.PackageDays)
	}
	if in.StartDate != nil {
		// start_date is a DATE column; pass the date as a string + cast so
		// pgx doesn't try to bind it as timestamptz (avoids type-inference
		// surprises, like the 42P08 TouchLastOnline hit).
		args = append(args, in.StartDate.UTC().Format("2006-01-02"))
		sets = append(sets, fmt.Sprintf("start_date = $%d::date", len(args)))
	}
	if in.Enable != nil {
		addSet("enable", *in.Enable)
	}
	if in.Mode != nil {
		addSet("mode", *in.Mode)
	}
	if in.Lang != nil {
		addSet("lang", *in.Lang)
	}
	args = append(args, id)
	q := fmt.Sprintf(
		`UPDATE users SET %s WHERE uuid = $%d RETURNING `+userCols,
		joinComma(sets), len(args),
	)
	row := s.Pool.QueryRow(ctx, q, args...)
	u, err := scanUser(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrNotFound
	}
	return u, err
}

// DeleteUser removes the row. Returns ErrNotFound if the uuid is unknown.
func (s *Store) DeleteUser(ctx context.Context, id uuid.UUID) error {
	tag, err := s.Pool.Exec(ctx, `DELETE FROM users WHERE uuid = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// RestoreUserInput is the shape RestoreUser takes. Unlike CreateUserInput
// the caller supplies UUID + timestamps (this is a backup-restore path,
// not a fresh create — we need to preserve identity + creation time).
type RestoreUserInput struct {
	UUID            uuid.UUID
	Name            string
	Comment         string
	UsageBytes      int64
	UsageLimitBytes int64
	PackageDays     int
	StartDate       *time.Time
	LastResetTime   time.Time
	LastOnline      *time.Time
	Enable          bool
	Mode            string
	Lang            string
	AddedByUUID     uuid.UUID
	CreatedAt       time.Time
}

// RestoreUser inserts a row from a backup, preserving the original UUID.
// Returns (user, inserted, err) where `inserted` is false when a row with
// that UUID already exists — restore is non-destructive (never overwrites
// live data). serial_id is regenerated (the local sequence wins).
func (s *Store) RestoreUser(ctx context.Context, in RestoreUserInput) (User, bool, error) {
	if in.Mode == "" {
		in.Mode = "no_reset"
	}
	if in.Lang == "" {
		in.Lang = "en"
	}
	if in.LastResetTime.IsZero() {
		in.LastResetTime = in.CreatedAt
	}
	if in.CreatedAt.IsZero() {
		in.CreatedAt = time.Now().UTC()
	}
	row := s.Pool.QueryRow(ctx, `
		INSERT INTO users (uuid, name, comment, usage_bytes, usage_limit_bytes,
		                   package_days, start_date, last_reset_time, last_online,
		                   enable, mode, lang, added_by_uuid, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
		ON CONFLICT (uuid) DO NOTHING
		RETURNING `+userCols,
		in.UUID, in.Name, in.Comment, in.UsageBytes, in.UsageLimitBytes,
		in.PackageDays, in.StartDate, in.LastResetTime, in.LastOnline,
		in.Enable, in.Mode, in.Lang, in.AddedByUUID, in.CreatedAt,
	)
	u, err := scanUser(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, false, nil // conflict — already exists, not an error
	}
	if err != nil {
		return User{}, false, err
	}
	return u, true, nil
}

// CountUsers returns total user count — used by server_status.
func (s *Store) CountUsers(ctx context.Context) (int64, error) {
	var n int64
	err := s.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

// CountEnabledUsers — enabled-only count for server_status.
func (s *Store) CountEnabledUsers(ctx context.Context) (int64, error) {
	var n int64
	err := s.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM users WHERE enable = TRUE`).Scan(&n)
	return n, err
}

// CountOnlineUsers counts users whose last_online is newer than `since`.
// The dashboard passes a 2-minute cutoff — the same window the user table's
// "online" pill uses — so the header figure and the per-row dots agree.
func (s *Store) CountOnlineUsers(ctx context.Context, since time.Time) (int64, error) {
	var n int64
	err := s.Pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM users WHERE last_online IS NOT NULL AND last_online > $1`,
		since,
	).Scan(&n)
	return n, err
}

// UserExists is a cheap presence check used by the reconciler to skip
// UUIDs xray still has stats for but we've deleted from the DB. Without
// this, deleted-user ghosts would inflate the `online` count and emit
// pointless UPDATE/UPSERT churn.
func (s *Store) UserExists(ctx context.Context, id uuid.UUID) (bool, error) {
	var exists bool
	err := s.Pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM users WHERE uuid = $1)`, id,
	).Scan(&exists)
	return exists, err
}

// AllUUIDsEnabled returns the uuids of all enabled users — used by xray
// hydrate-from-DB on startup, and by the reconciler for stats queries.
func (s *Store) AllUUIDsEnabled(ctx context.Context) ([]uuid.UUID, error) {
	rows, err := s.Pool.Query(ctx, `SELECT uuid FROM users WHERE enable = TRUE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// TouchLastOnline updates last_online + start_date (if null) to now.
// Called when the reconciler observes a positive byte delta for a user.
//
// Both columns are explicitly cast at the SQL boundary because pgx's
// parameter-type inference chokes when the same parameter is consumed
// at two different target types ("ERROR: inconsistent types deduced
// for parameter $2 (SQLSTATE 42P08)") — last_online wants TIMESTAMPTZ
// and start_date wants DATE. Casting both sides locks the inference
// in and pgx no longer has to guess.
func (s *Store) TouchLastOnline(ctx context.Context, id uuid.UUID, at time.Time) error {
	_, err := s.Pool.Exec(ctx, `
		UPDATE users
		SET last_online = $2::TIMESTAMPTZ,
		    start_date  = COALESCE(start_date, $2::TIMESTAMPTZ::DATE),
		    updated_at  = NOW()
		WHERE uuid = $1`,
		id, at,
	)
	return err
}

// DisableOverLimitOrExpired flips enable=FALSE for every currently-enabled
// user who has hit their data cap (usage_bytes >= a POSITIVE usage_limit_bytes)
// or passed expiry (a set start_date + a POSITIVE package_days that has reached
// today: start_date + package_days <= CURRENT_DATE, so a 7-day user gets exactly
// 7 days and is disabled ON the expiry date — Hiddify duration semantics), and
// returns their uuids so the caller can evict them from xray. One
// atomic statement — this is the quota/expiry enforcement the reconciler runs
// every tick. A 0 usage_limit_bytes means unlimited; package_days 0 means no
// expiry; a NULL start_date means the clock hasn't started (never connected),
// so neither can trip those rows.
func (s *Store) DisableOverLimitOrExpired(ctx context.Context) ([]uuid.UUID, error) {
	rows, err := s.Pool.Query(ctx, `
		UPDATE users SET enable = FALSE, updated_at = NOW()
		WHERE enable = TRUE
		  AND (
		        (usage_limit_bytes > 0 AND usage_bytes >= usage_limit_bytes)
		     OR (package_days > 0 AND start_date IS NOT NULL
		         AND start_date + package_days <= CURRENT_DATE)
		      )
		RETURNING uuid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// AddUsage atomically increments usage_bytes by delta.
func (s *Store) AddUsage(ctx context.Context, id uuid.UUID, delta int64) error {
	if delta <= 0 {
		return nil
	}
	_, err := s.Pool.Exec(ctx,
		`UPDATE users SET usage_bytes = usage_bytes + $2, updated_at = NOW() WHERE uuid = $1`,
		id, delta,
	)
	return err
}

func joinComma(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ", "
		}
		out += p
	}
	return out
}
