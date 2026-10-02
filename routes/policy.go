package routes

// Access policy (AUTH-06). Every route Register adds must be listed here
// with the class of caller it admits; routes/authz_matrix_test.go fails when
// a route is missing, and checks each class's guards against every role.
//
// Guards per class (all in middleware/account.go and middleware/session.go):
//
//	Public          no session. Handlers that act on a resource prove access
//	                another way (signed token, ticket code, webhook signature).
//	Session         RequireSession: any active, signed-in user.
//	HostStaff       + HostAccount(admin, staff): the host owner or a team
//	                member. Queries are scoped to the owner's account; staff
//	                are further limited to their assigned events in the handler.
//	HostStaffEvent  + RequireEventAccess: as HostStaff, and staff get 404 for
//	                any /events/:id not assigned to them.
//	HostAdmin       + HostAccount(admin): the host owner or a team admin.
//	HostAdminPayout + PayoutAuth: as HostAdmin, plus the payout passcode token.
//	Superuser       + RequireSuperuser: platform superusers only.
//
// Objects are always looked up with the caller's account (host_id or user_id)
// in the WHERE clause, so another account's IDs answer 404, never data.
type AccessClass string

const (
	Public          AccessClass = "public"
	Session         AccessClass = "session"
	HostStaff       AccessClass = "host-staff"
	HostStaffEvent  AccessClass = "host-staff-event"
	HostAdmin       AccessClass = "host-admin"
	HostAdminPayout AccessClass = "host-admin-payout"
	Superuser       AccessClass = "superuser"
)

// AccessPolicy maps "METHOD path" (as registered) to its access class.
var AccessPolicy = map[string]AccessClass{
	"GET /health":                          Public,
	"POST /api/v1/webhooks/stripe":         Public, // Stripe signature
	"POST /api/v1/webhooks/paypal":         Public, // PayPal signature verification
	"GET /api/v1/pricing":                  Public,
	"GET /api/v1/events/public":            Public,
	"GET /api/v1/events/:id":               Public,
	"GET /api/v1/media/events/:id/:kind":   Public,
	"GET /api/v1/advertisements":           Public,
	"GET /api/v1/events/:id/stream-status": Public,

	// Auth
	"POST /api/v1/auth/register":              Public, // invite token
	"GET /api/v1/auth/invitations/:token":     Public,
	"POST /api/v1/auth/forgot-password":       Public,
	"GET /api/v1/auth/password-resets/:token": Public,
	"POST /api/v1/auth/reset-password":        Public, // reset token
	"POST /api/v1/auth/login":                 Public,
	"POST /api/v1/auth/logout":                Public, // revokes the cookie's session, if any
	"POST /api/v1/auth/verify-email":          Public, // verification token
	"POST /api/v1/auth/resend-verification":   Public,
	"POST /api/v1/auth/logout-everywhere":     Session,
	"GET /api/v1/auth/me":                     Session,
	"PUT /api/v1/auth/profile":                Session,
	"POST /api/v1/auth/change-password":       Session,

	// Venue fee payment returns (signed state tied to event and host)
	"GET /api/v1/events/:id/wipay/launch":    Public,
	"POST /api/v1/events/:id/wipay/launch":   Public,
	"GET /api/v1/events/:id/wipay/complete":  Public,
	"POST /api/v1/events/:id/wipay/complete": Public,
	"GET /api/v1/events/:id/paypal/complete": Public,
	"GET /api/v1/events/:id/stripe/complete": Public,

	// Host events
	"POST /api/v1/events":                        HostAdmin,
	"GET /api/v1/events":                         HostStaff,
	"POST /api/v1/events/:id/checkout":           HostAdmin,
	"POST /api/v1/events/:id/bypass-activate":    HostAdmin,
	"DELETE /api/v1/events/:id":                  HostAdmin,
	"PATCH /api/v1/events/:id/ticket":            HostStaffEvent,
	"POST /api/v1/events/:id/images/:kind":       HostStaffEvent,
	"DELETE /api/v1/events/:id/images/:kind":     HostStaffEvent,
	"GET /api/v1/events/:id/stream-credentials":  HostStaffEvent,
	"POST /api/v1/events/:id/reprovision-stream": HostStaffEvent,
	"GET /api/v1/events/:id/viewers":             HostStaffEvent,

	// Flyers
	"GET /api/v1/advertisements/mine":   HostStaff,
	"POST /api/v1/advertisements":       HostStaff,
	"PUT /api/v1/advertisements/:id":    HostStaff,
	"DELETE /api/v1/advertisements/:id": HostStaff,

	// Payouts
	"GET /api/v1/connect/security/status":    HostAdmin,
	"POST /api/v1/connect/security/passcode": HostAdmin,
	"POST /api/v1/connect/security/unlock":   HostAdmin,
	"POST /api/v1/connect/onboard":           HostAdminPayout,
	"GET /api/v1/connect/status":             HostAdminPayout,
	"POST /api/v1/connect/wipay":             HostAdminPayout,
	"POST /api/v1/connect/paypal":            HostAdminPayout,
	"GET /api/v1/connect/paypal/complete":    Public, // PayPal redirect, confirmed with PayPal's API
	"POST /api/v1/connect/activate":          HostAdminPayout,
	"POST /api/v1/connect/deactivate":        HostAdminPayout,
	"GET /api/v1/connect/balance":            HostAdminPayout,
	"POST /api/v1/connect/payout":            HostAdminPayout,

	// Team
	"GET /api/v1/team":        HostAdmin,
	"POST /api/v1/team":       HostAdmin,
	"PATCH /api/v1/team/:id":  HostAdmin,
	"DELETE /api/v1/team/:id": HostAdmin,

	// Superuser
	"GET /api/v1/superuser/overview":                Superuser,
	"GET /api/v1/superuser/security-events":         Superuser,
	"GET /api/v1/superuser/users":                   Superuser,
	"PATCH /api/v1/superuser/users/:id":             Superuser,
	"DELETE /api/v1/superuser/users/:id":            Superuser,
	"GET /api/v1/superuser/invitations":             Superuser,
	"POST /api/v1/superuser/invitations":            Superuser,
	"POST /api/v1/superuser/invitations/:id/resend": Superuser,
	"DELETE /api/v1/superuser/invitations/:id":      Superuser,
	"GET /api/v1/superuser/events":                  Superuser,
	"PATCH /api/v1/superuser/events/:id":            Superuser,
	"POST /api/v1/superuser/events/:id/activate":    Superuser,
	"POST /api/v1/superuser/events/:id/cancel":      Superuser,

	// Tickets
	"POST /api/v1/tickets/lookup":         Public, // emails the address its tickets; generic reply, rate-limited
	"GET /api/v1/tickets/lookup":          Public, // retired: always 410
	"GET /api/v1/tickets/claim/:token":    Public, // claim token
	"GET /api/v1/tickets/enter":           Public, // ticket code
	"POST /api/v1/tickets/guest-purchase": Public,
	"GET /api/v1/tickets/paypal/complete": Public, // PayPal order capture
	"POST /api/v1/tickets/purchase":       Session,
	"GET /api/v1/tickets/mine":            Session,
	"POST /api/v1/tickets/checkin":        HostStaff, // staff: assigned events only

	// Streaming and chat
	"POST /api/v1/stream/watch":             Session,
	"POST /api/v1/stream/heartbeat":         Session,
	"POST /api/v1/stream/release":           Session,
	"POST /api/v1/events/:id/chat/register": Public, // ticket code
	"GET /api/v1/events/:id/chat/ws":        Public, // chat token or host session, checked in the handler
}
