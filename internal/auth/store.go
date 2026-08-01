package auth

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/gorilla/securecookie"
	"github.com/gorilla/sessions"
)

// sessionSchema lives here rather than in internal/db because the sessions
// table is an implementation detail of this package's store.
const sessionSchema = `
CREATE TABLE IF NOT EXISTS sessions (
	id         TEXT    PRIMARY KEY,
	data       BLOB    NOT NULL,
	expires_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS sessions_expires_idx ON sessions (expires_at);
`

// errSessionGone covers both "no such row" and "row expired"; to the caller
// both mean the cookie resolves to nothing.
var errSessionGone = errors.New("session not found or expired")

// SQLiteStore is a gorilla/sessions Store backed by the app's SQLite database.
// The browser cookie holds only the session ID, signed with the store's HMAC
// key; the session data stays server-side, so deleting the row (logout) or
// letting it expire invalidates the cookie outright.
type SQLiteStore struct {
	db      *sql.DB
	codecs  []securecookie.Codec
	options *sessions.Options
}

// NewSQLiteStore builds the store. keyPairs are passed to
// securecookie.CodecsFromPairs (hash key first, optional block keys after) and
// sign both the cookie's session ID and the stored session data.
func NewSQLiteStore(db *sql.DB, options *sessions.Options, keyPairs ...[]byte) *SQLiteStore {
	return &SQLiteStore{
		db:      db,
		codecs:  securecookie.CodecsFromPairs(keyPairs...),
		options: options,
	}
}

// Get returns the named session, cached per request by gorilla's registry.
func (s *SQLiteStore) Get(r *http.Request, name string) (*sessions.Session, error) {
	return sessions.GetRegistry(r).Get(s, name)
}

// New returns the session addressed by the request cookie, or a fresh one when
// the cookie is absent, forged, or points at a row that is gone or expired.
func (s *SQLiteStore) New(r *http.Request, name string) (*sessions.Session, error) {
	session := sessions.NewSession(s, name)
	opts := *s.options
	session.Options = &opts
	session.IsNew = true

	cookie, err := r.Cookie(name)
	if err != nil {
		return session, nil
	}
	var id string
	if err := securecookie.DecodeMulti(name, cookie.Value, &id, s.codecs...); err != nil {
		return session, nil
	}
	data, err := s.load(r.Context(), id)
	if errors.Is(err, errSessionGone) {
		return session, nil
	}
	if err != nil {
		return session, err
	}
	if err := securecookie.DecodeMulti(name, string(data), &session.Values, s.codecs...); err != nil {
		return session, fmt.Errorf("decode session data: %w", err)
	}
	session.ID = id
	session.IsNew = false
	return session, nil
}

// Save persists the session row and sets the signed-ID cookie. A session
// saved with MaxAge <= 0 is deleted server-side and the cookie is expired.
func (s *SQLiteStore) Save(r *http.Request, w http.ResponseWriter, session *sessions.Session) error {
	if session.Options.MaxAge <= 0 {
		if session.ID != "" {
			if err := s.remove(r.Context(), session.ID); err != nil {
				return err
			}
		}
		http.SetCookie(w, sessions.NewCookie(session.Name(), "", session.Options))
		return nil
	}

	if session.ID == "" {
		id, err := randomID()
		if err != nil {
			return err
		}
		session.ID = id
	}
	encoded, err := securecookie.EncodeMulti(session.Name(), session.Values, s.codecs...)
	if err != nil {
		return fmt.Errorf("encode session data: %w", err)
	}
	expires := time.Now().Add(time.Duration(session.Options.MaxAge) * time.Second)
	if err := s.upsert(r.Context(), session.ID, []byte(encoded), expires.Unix()); err != nil {
		return err
	}

	signedID, err := securecookie.EncodeMulti(session.Name(), session.ID, s.codecs...)
	if err != nil {
		return fmt.Errorf("sign session id: %w", err)
	}
	http.SetCookie(w, sessions.NewCookie(session.Name(), signedID, session.Options))
	return nil
}

func (s *SQLiteStore) load(ctx context.Context, id string) ([]byte, error) {
	var (
		data      []byte
		expiresAt int64
	)
	err := s.db.QueryRowContext(ctx,
		"SELECT data, expires_at FROM sessions WHERE id = ?", id).Scan(&data, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errSessionGone
	}
	if err != nil {
		return nil, fmt.Errorf("load session: %w", err)
	}
	if time.Now().Unix() >= expiresAt {
		return nil, errSessionGone
	}
	return data, nil
}

func (s *SQLiteStore) upsert(ctx context.Context, id string, data []byte, expiresAt int64) error {
	if _, err := s.db.ExecContext(ctx,
		"INSERT OR REPLACE INTO sessions (id, data, expires_at) VALUES (?, ?, ?)",
		id, data, expiresAt); err != nil {
		return fmt.Errorf("save session: %w", err)
	}
	return nil
}

func (s *SQLiteStore) remove(ctx context.Context, id string) error {
	if _, err := s.db.ExecContext(ctx, "DELETE FROM sessions WHERE id = ?", id); err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	return nil
}

// PurgeExpired deletes rows past their expiry. Best-effort housekeeping,
// called once at startup.
func (s *SQLiteStore) PurgeExpired(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx,
		"DELETE FROM sessions WHERE expires_at <= ?", time.Now().Unix()); err != nil {
		return fmt.Errorf("purge expired sessions: %w", err)
	}
	return nil
}

// randomID returns 32 random bytes, base64url-encoded (43 chars, no padding).
func randomID() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate session id: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
