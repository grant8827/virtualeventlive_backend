package middleware

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/redis/go-redis/v9"
)

// Rate limiting (AUTH-04). Counters live in Redis so every server instance
// shares them; if Redis is unreachable an in-memory fallback keeps limits in
// force on this instance rather than failing open entirely.

// ─── Client IP ──────────────────────────────────────────────────────────────

var trustedProxyHops = 0

// SetTrustedProxyHops says how many reverse proxies (e.g. Railway's edge)
// sit in front of the API. With N hops the client is the Nth address from
// the right of X-Forwarded-For — the one our own proxy appended — not the
// leftmost, which the client controls.
func SetTrustedProxyHops(n int) { trustedProxyHops = n }

// ClientIP is the address used for rate limits, sessions and audit events.
func ClientIP(c *fiber.Ctx) string {
	if trustedProxyHops > 0 {
		if xff := c.Get(fiber.HeaderXForwardedFor); xff != "" {
			parts := strings.Split(xff, ",")
			if i := len(parts) - trustedProxyHops; i >= 0 {
				if ip := strings.TrimSpace(parts[i]); ip != "" {
					return ip
				}
			}
		}
	}
	return c.IP()
}

// ─── Limiter ────────────────────────────────────────────────────────────────

type RateLimiter struct {
	rdb *redis.Client
	mem *memoryCounter
}

func NewRateLimiter(rdb *redis.Client) *RateLimiter {
	return &RateLimiter{rdb: rdb, mem: newMemoryCounter()}
}

// Hit counts one event for key in a fixed window and returns the count so
// far in this window and when the window resets.
func (l *RateLimiter) Hit(ctx context.Context, key string, window time.Duration) (int64, time.Duration) {
	now := time.Now()
	bucket := now.Unix() / int64(window.Seconds())
	reset := time.Unix((bucket+1)*int64(window.Seconds()), 0).Sub(now)
	if l != nil && l.rdb != nil {
		rk := "rl:" + key + ":" + strconv.FormatInt(bucket, 10)
		pipe := l.rdb.TxPipeline()
		incr := pipe.Incr(ctx, rk)
		pipe.Expire(ctx, rk, window+time.Minute)
		if _, err := pipe.Exec(ctx); err == nil {
			return incr.Val(), reset
		} else {
			log.Printf("rate limit: redis unavailable, using in-memory counters: %v", err)
		}
	}
	if l == nil {
		return 0, reset
	}
	return l.mem.hit(key+":"+strconv.FormatInt(bucket, 10), now.Add(reset)), reset
}

// Count returns the current window's count for key without adding to it.
func (l *RateLimiter) Count(ctx context.Context, key string, window time.Duration) int64 {
	bucket := time.Now().Unix() / int64(window.Seconds())
	if l != nil && l.rdb != nil {
		v, err := l.rdb.Get(ctx, "rl:"+key+":"+strconv.FormatInt(bucket, 10)).Int64()
		if err == nil || err == redis.Nil {
			return v
		}
	}
	if l == nil {
		return 0
	}
	return l.mem.get(key + ":" + strconv.FormatInt(bucket, 10))
}

// Reset clears key's current window (e.g. failed-login counter on success).
func (l *RateLimiter) Reset(ctx context.Context, key string, window time.Duration) {
	bucket := time.Now().Unix() / int64(window.Seconds())
	k := key + ":" + strconv.FormatInt(bucket, 10)
	if l != nil && l.rdb != nil {
		_ = l.rdb.Del(ctx, "rl:"+k).Err()
	}
	if l != nil {
		l.mem.reset(k)
	}
}

// Limit is a per-route limit: at most Max requests per Window per key.
type Limit struct {
	Name   string
	Max    int64
	Window time.Duration
	// Key picks what's counted; nil means the client IP.
	Key func(c *fiber.Ctx) string
}

// RateLimit enforces limit, answering 429 with Retry-After when exceeded.
func RateLimit(l *RateLimiter, limit Limit) fiber.Handler {
	return func(c *fiber.Ctx) error {
		key := ClientIP(c)
		if limit.Key != nil {
			key = limit.Key(c)
		}
		if key == "" {
			return c.Next()
		}
		n, reset := l.Hit(c.Context(), limit.Name+":"+key, limit.Window)
		if n > limit.Max {
			c.Set(fiber.HeaderRetryAfter, fmt.Sprint(int(reset.Seconds())+1))
			return c.Status(fiber.StatusTooManyRequests).JSON(fiber.Map{
				"error": "Too many attempts. Please wait a few minutes and try again.",
			})
		}
		return c.Next()
	}
}

// ByUser keys a limit on the signed-in user (after RequireSession).
func ByUser(c *fiber.Ctx) string {
	id, _ := c.Locals("user_id").(string)
	return id
}

// HashKey turns personal data (an email address) into a counter key, so
// Redis never stores it in the clear.
func HashKey(s string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(s))))
	return hex.EncodeToString(sum[:16])
}

// ─── In-memory fallback ─────────────────────────────────────────────────────

type memoryCounter struct {
	mu     sync.Mutex
	counts map[string]memoryCount
}

type memoryCount struct {
	n       int64
	expires time.Time
}

func newMemoryCounter() *memoryCounter {
	return &memoryCounter{counts: map[string]memoryCount{}}
}

func (m *memoryCounter) hit(key string, expires time.Time) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	if len(m.counts) > 50000 {
		for k, v := range m.counts {
			if now.After(v.expires) {
				delete(m.counts, k)
			}
		}
	}
	c := m.counts[key]
	if now.After(c.expires) {
		c = memoryCount{expires: expires}
	}
	c.n++
	m.counts[key] = c
	return c.n
}

func (m *memoryCounter) get(key string) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.counts[key]
	if time.Now().After(c.expires) {
		return 0
	}
	return c.n
}

func (m *memoryCounter) reset(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.counts, key)
}
