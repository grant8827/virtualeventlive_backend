package middleware

import (
	"context"
	"slices"

	"github.com/gofiber/fiber/v2"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Team roles. The host who registered the account is the "owner"; the
// people they add through the dashboard are "admin" (everything the owner
// can do) or "staff" (Go Live, Chat, Tickets/Flyer, Scan Tickets).
const (
	TeamOwner = "owner"
	TeamAdmin = "admin"
	TeamStaff = "staff"
)

// Account is who a logged-in user acts as on the host dashboard.
type Account struct {
	OwnerID  string // the host account whose data is read and written
	TeamRole string // owner, admin or staff
	Active   bool
}

// ResolveAccount reads the user's live role and status from the database, so
// a suspension or role change takes effect on the next request rather than
// when their 24h token expires.
func ResolveAccount(ctx context.Context, db *pgxpool.Pool, userID string) (Account, error) {
	var (
		role, status string
		ownerID      *string
		ownerStatus  *string
	)
	err := db.QueryRow(ctx,
		`SELECT u.role, u.status, u.account_owner_id, o.status
		 FROM users u LEFT JOIN users o ON o.id = u.account_owner_id
		 WHERE u.id = $1`, userID,
	).Scan(&role, &status, &ownerID, &ownerStatus)
	if err != nil {
		return Account{}, err
	}

	// A team member is locked out too when a superuser suspends their host.
	acct := Account{Active: status == "active" && (ownerStatus == nil || *ownerStatus == "active")}
	switch {
	case role == "host" && ownerID == nil:
		acct.OwnerID, acct.TeamRole = userID, TeamOwner
	case (role == TeamAdmin || role == TeamStaff) && ownerID != nil:
		acct.OwnerID, acct.TeamRole = *ownerID, role
	}
	return acct, nil
}

// HostAccount replaces RequireRole("host") on dashboard routes. The owner is
// always let through; team members only when their role is in allowed. For
// members, Locals("user_id") is swapped to the owner's ID so existing
// handlers scope their queries to the owner's account unchanged; the real
// user stays available as Locals("actor_id").
func HostAccount(db *pgxpool.Pool, allowed ...string) fiber.Handler {
	return func(c *fiber.Ctx) error {
		userID, _ := c.Locals("user_id").(string)
		acct, err := ResolveAccount(c.Context(), db, userID)
		if err != nil || !acct.Active {
			// 401 makes the frontend drop the session, which is what a
			// suspended or deleted member should see.
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "account is not active"})
		}
		if acct.TeamRole == "" || (acct.TeamRole != TeamOwner && !slices.Contains(allowed, acct.TeamRole)) {
			return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "insufficient permissions"})
		}

		c.Locals("actor_id", userID)
		c.Locals("user_id", acct.OwnerID)
		c.Locals("role", "host")
		c.Locals("team_role", acct.TeamRole)
		return c.Next()
	}
}

// AssignedStaff returns the signed-in user's ID when they are a staff member,
// whose dashboard access is limited to events assigned to them. It returns
// false for the owner and admins, who see every event on the account.
func AssignedStaff(c *fiber.Ctx) (string, bool) {
	if role, _ := c.Locals("team_role").(string); role != TeamStaff {
		return "", false
	}
	actorID, _ := c.Locals("actor_id").(string)
	return actorID, true
}

// RequireEventAccess guards /events/:id routes behind HostAccount: staff get
// a 404 for any event not assigned to them, the same answer as an event on
// another host's account.
func RequireEventAccess(db *pgxpool.Pool) fiber.Handler {
	return func(c *fiber.Ctx) error {
		staffID, isStaff := AssignedStaff(c)
		if !isStaff {
			return c.Next()
		}
		var assigned bool
		err := db.QueryRow(c.Context(),
			`SELECT EXISTS(SELECT 1 FROM events WHERE id = $1 AND host_id = $2 AND assigned_to = $3)`,
			c.Params("id"), c.Locals("user_id"), staffID,
		).Scan(&assigned)
		if err != nil || !assigned {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "event not found"})
		}
		return c.Next()
	}
}

// RequireSuperuser guards the platform dashboard. The role is read live from
// the database, like HostAccount, so revoking it takes effect immediately.
func RequireSuperuser(db *pgxpool.Pool) fiber.Handler {
	return func(c *fiber.Ctx) error {
		userID, _ := c.Locals("user_id").(string)
		var role, status string
		err := db.QueryRow(c.Context(),
			`SELECT role, status FROM users WHERE id = $1`, userID,
		).Scan(&role, &status)
		if err != nil || status != "active" {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "account is not active"})
		}
		if role != "superuser" {
			return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "insufficient permissions"})
		}
		return c.Next()
	}
}
