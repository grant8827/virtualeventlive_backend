package middleware

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Server-side sessions (AUTH-03). Sign-in creates a row in `sessions` and
// gives the browser a random ID in an HttpOnly cookie that page scripts can't
// read. Every request re-checks the session and the user's live role/status
// in the database, so revocation, suspension and role changes apply at once.
const (
	SessionIdleTimeout   = 24 * time.Hour     // ends after a day without use
	SessionAbsoluteTTL   = 7 * 24 * time.Hour // ends a week after sign-in regardless
	sessionRotateEvery   = time.Hour          // a fresh ID is issued hourly
	sessionRotationGrace = time.Minute        // the old ID works briefly after rotation
	sessionTouchEvery    = time.Minute        // last_seen_at write throttle
)

// SessionConfig controls the cookie. Secure is on everywhere except local
// development over plain http.
type SessionConfig struct {
	Secure bool
}

// CookieName uses the __Host- prefix when Secure: browsers then only accept
// the cookie if it is Secure, has Path=/ and no Domain, so another subdomain
// can't plant or overwrite it.
func (s SessionConfig) CookieName() string {
	if s.Secure {
		return "__Host-vep_session"
	}
	return "vep_session"
}

func newSessionToken() (token, hash string, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	token = base64.RawURLEncoding.EncodeToString(b)
	return token, HashSessionToken(token), nil
}

// HashSessionToken is how a session ID is stored and looked up.
func HashSessionToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// CreateSession starts a session for userID and returns its ID (the cookie
// value) and absolute expiry.
func CreateSession(ctx context.Context, db *pgxpool.Pool, userID, ip, userAgent string) (string, time.Time, error) {
	token, hash, err := newSessionToken()
	if err != nil {
		return "", time.Time{}, err
	}
	if len(userAgent) > 300 {
		userAgent = userAgent[:300]
	}
	expires := time.Now().Add(SessionAbsoluteTTL)
	if _, err := db.Exec(ctx,
		`INSERT INTO sessions (user_id, token_hash, expires_at, ip, user_agent) VALUES ($1, $2, $3, $4, $5)`,
		userID, hash, expires, ip, userAgent,
	); err != nil {
		return "", time.Time{}, err
	}
	return token, expires, nil
}

// SetSessionCookie sends the session cookie, valid until expires.
func SetSessionCookie(c *fiber.Ctx, cfg SessionConfig, token string, expires time.Time) {
	c.Cookie(&fiber.Cookie{
		Name:     cfg.CookieName(),
		Value:    token,
		Path:     "/",
		Expires:  expires,
		MaxAge:   int(time.Until(expires).Seconds()),
		HTTPOnly: true,
		Secure:   cfg.Secure,
		SameSite: fiber.CookieSameSiteLaxMode,
	})
}

// ClearSessionCookie removes the session cookie from the browser.
func ClearSessionCookie(c *fiber.Ctx, cfg SessionConfig) {
	c.Cookie(&fiber.Cookie{
		Name:     cfg.CookieName(),
		Value:    "",
		Path:     "/",
		Expires:  time.Unix(0, 0),
		MaxAge:   -1,
		HTTPOnly: true,
		Secure:   cfg.Secure,
		SameSite: fiber.CookieSameSiteLaxMode,
	})
}

// SessionUser is the signed-in user for this request, read live from the
// database.
type SessionUser struct {
	SessionID string
	UserID    string
	Role      string
	Status    string
	ExpiresAt time.Time

	current   bool // matched the current ID (not the post-rotation grace ID)
	rotatedAt time.Time
	lastSeen  time.Time
}

var ErrNoSession = errors.New("no valid session")

// LookupSession resolves a session ID to its user, or ErrNoSession if it's
// unknown, revoked, idle-expired or past its absolute expiry.
func LookupSession(ctx context.Context, db *pgxpool.Pool, token string) (*SessionUser, error) {
	if token == "" {
		return nil, ErrNoSession
	}
	hash := HashSessionToken(token)
	var s SessionUser
	err := db.QueryRow(ctx,
		`SELECT s.id, s.user_id, u.role, u.status, s.expires_at, s.token_hash = $1, s.rotated_at, s.last_seen_at
		 FROM sessions s JOIN users u ON u.id = s.user_id
		 WHERE (s.token_hash = $1 OR (s.previous_token_hash = $1 AND s.previous_valid_until > NOW()))
		   AND s.revoked_at IS NULL
		   AND s.expires_at > NOW()
		   AND s.last_seen_at > NOW() - make_interval(secs => $2)`,
		hash, SessionIdleTimeout.Seconds(),
	).Scan(&s.SessionID, &s.UserID, &s.Role, &s.Status, &s.ExpiresAt, &s.current, &s.rotatedAt, &s.lastSeen)
	if err != nil {
		return nil, ErrNoSession
	}
	return &s, nil
}

// RequireSession authenticates the request from the session cookie. Bearer
// tokens are no longer accepted. On success it sets Locals user_id, role
// (live from the database) and session_id.
func RequireSession(db *pgxpool.Pool, cfg SessionConfig) fiber.Handler {
	return func(c *fiber.Ctx) error {
		token := c.Cookies(cfg.CookieName())
		if token == "" {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "not signed in"})
		}
		ctx := c.Context()
		s, err := LookupSession(ctx, db, token)
		if err != nil {
			ClearSessionCookie(c, cfg)
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "your session has expired; please sign in again"})
		}
		if s.Status != "active" {
			_ = RevokeSession(ctx, db, s.SessionID, "account_inactive")
			ClearSessionCookie(c, cfg)
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "account is not active"})
		}

		now := time.Now()
		switch {
		case s.current && now.Sub(s.rotatedAt) > sessionRotateEvery:
			// Periodic rotation limits how long a stolen ID stays useful.
			if newToken, newHash, err := newSessionToken(); err == nil {
				tag, err := db.Exec(ctx,
					`UPDATE sessions
					 SET previous_token_hash = token_hash, previous_valid_until = NOW() + make_interval(secs => $3),
					     token_hash = $2, rotated_at = NOW(), last_seen_at = NOW()
					 WHERE id = $1 AND token_hash = $4`,
					s.SessionID, newHash, sessionRotationGrace.Seconds(), HashSessionToken(token))
				if err == nil && tag.RowsAffected() == 1 {
					SetSessionCookie(c, cfg, newToken, s.ExpiresAt)
				}
			}
		case now.Sub(s.lastSeen) > sessionTouchEvery:
			_, _ = db.Exec(ctx, `UPDATE sessions SET last_seen_at = NOW() WHERE id = $1`, s.SessionID)
		}

		c.Locals("user_id", s.UserID)
		c.Locals("role", s.Role)
		c.Locals("session_id", s.SessionID)
		return c.Next()
	}
}

// RevokeSession ends one session.
func RevokeSession(ctx context.Context, db *pgxpool.Pool, sessionID, reason string) error {
	_, err := db.Exec(ctx,
		`UPDATE sessions SET revoked_at = NOW(), revoked_reason = $2 WHERE id = $1 AND revoked_at IS NULL`,
		sessionID, reason)
	return err
}

// RevokeUserSessions ends all of a user's sessions except exceptSessionID
// (empty = all). Used on password change/reset, suspension, role changes and
// deletion so the change takes effect everywhere at once.
func RevokeUserSessions(ctx context.Context, db *pgxpool.Pool, userID, reason, exceptSessionID string) (int64, error) {
	tag, err := db.Exec(ctx,
		`UPDATE sessions SET revoked_at = NOW(), revoked_reason = $2
		 WHERE user_id = $1 AND revoked_at IS NULL AND ($3 = '' OR id::text <> $3)`,
		userID, reason, exceptSessionID)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
