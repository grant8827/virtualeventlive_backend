package middleware

import (
	"net/url"
	"strings"

	"github.com/gofiber/fiber/v2"
)

// NormalizeOrigins turns a comma-separated list of URLs into canonical
// origins ("scheme://host[:port]").
func NormalizeOrigins(lists ...string) map[string]bool {
	out := map[string]bool{}
	for _, list := range lists {
		for _, raw := range strings.Split(list, ",") {
			if o := originOf(strings.TrimSpace(raw)); o != "" {
				out[o] = true
			}
		}
	}
	return out
}

func originOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return strings.ToLower(u.Scheme + "://" + u.Host)
}

// RequireTrustedOrigin is the CSRF defence for cookie sessions. A request
// that changes state and carries the session cookie must come from one of
// our own web origins (Origin header, or Referer when Origin is absent).
// Requests without the cookie carry no ambient credentials, so they pass
// (e.g. guest checkout, provider webhooks). SameSite=Lax cookies already
// stop most cross-site requests; this also covers sibling subdomains.
func RequireTrustedOrigin(allowed map[string]bool, cookieName string) fiber.Handler {
	return func(c *fiber.Ctx) error {
		switch c.Method() {
		case fiber.MethodGet, fiber.MethodHead, fiber.MethodOptions:
			return c.Next()
		}
		if c.Cookies(cookieName) == "" {
			return c.Next()
		}
		origin := originOf(c.Get(fiber.HeaderOrigin))
		if origin == "" {
			origin = originOf(c.Get(fiber.HeaderReferer))
		}
		if origin == "" || !allowed[origin] {
			return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "request origin not allowed"})
		}
		return c.Next()
	}
}

// SecurityHeaders sets defensive response headers. The API serves JSON and
// media, so its content policy forbids everything except the inline
// auto-submit page some payment providers need (HTML responses only).
func SecurityHeaders(production bool) fiber.Handler {
	return func(c *fiber.Ctx) error {
		err := c.Next()
		h := &c.Response().Header
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=()")
		if strings.HasPrefix(string(h.ContentType()), "text/html") {
			h.Set("Content-Security-Policy", "default-src 'none'; script-src 'unsafe-inline'; form-action https:; frame-ancestors 'none'; base-uri 'none'")
		} else {
			h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'; base-uri 'none'")
		}
		if production {
			h.Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
		}
		return err
	}
}
