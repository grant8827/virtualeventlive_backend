// Command superuser creates a platform superuser, or promotes an existing
// account to one. Superusers can't sign up through the site.
//
//	go run ./cmd/superuser -email you@example.com -name "Your Name"
//
// It prompts for a password when creating a new account; an existing
// account keeps its password unless -reset-password is given.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"

	"github.com/joho/godotenv"
	"golang.org/x/crypto/bcrypt"

	"vertualeventlive/backend/config"
	"vertualeventlive/backend/database"
)

func main() {
	email := flag.String("email", "", "email address of the superuser (required)")
	name := flag.String("name", "", "full name for a new account")
	resetPassword := flag.Bool("reset-password", false, "set a new password on an existing account")
	demote := flag.Bool("demote", false, "turn an existing superuser back into a buyer account")
	flag.Parse()

	*email = strings.TrimSpace(*email)
	if *email == "" {
		flag.Usage()
		os.Exit(2)
	}

	_ = godotenv.Load()
	cfg := config.Load()
	db, err := database.ConnectPostgres(cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("PostgreSQL: %v", err)
	}
	defer db.Close()
	if err := database.RunMigrations(db); err != nil {
		log.Fatalf("Migrations: %v", err)
	}
	ctx := context.Background()

	var (
		id, role string
		ownerID  *string
	)
	err = db.QueryRow(ctx, `SELECT id, role, account_owner_id FROM users WHERE email = $1`, *email).Scan(&id, &role, &ownerID)
	exists := err == nil

	if *demote {
		if !exists || role != "superuser" {
			log.Fatalf("%s is not a superuser", *email)
		}
		if _, err := db.Exec(ctx, `UPDATE users SET role = 'buyer', updated_at = NOW() WHERE id = $1`, id); err != nil {
			log.Fatalf("demote: %v", err)
		}
		fmt.Printf("%s is no longer a superuser.\n", *email)
		return
	}

	if exists {
		if ownerID != nil {
			log.Fatalf("%s is a staff/admin login on a host account; use a separate email for the superuser", *email)
		}
		if role == "host" {
			log.Fatalf("%s is a host account; promoting it would lock the host out of their dashboard. Use a separate email for the superuser", *email)
		}
		var hash *string
		if *resetPassword {
			h := hashPassword(readPassword())
			hash = &h
		}
		if _, err := db.Exec(ctx,
			`UPDATE users SET role = 'superuser', status = 'active',
			        password_hash = COALESCE($1, password_hash), updated_at = NOW()
			 WHERE id = $2`, hash, id,
		); err != nil {
			log.Fatalf("promote: %v", err)
		}
		fmt.Printf("%s is now a superuser. Sign in at /login.\n", *email)
		return
	}

	hash := hashPassword(readPassword())
	if _, err := db.Exec(ctx,
		`INSERT INTO users (email, password_hash, role, full_name) VALUES ($1, $2, 'superuser', $3)`,
		*email, hash, strings.TrimSpace(*name),
	); err != nil {
		log.Fatalf("create: %v", err)
	}
	fmt.Printf("Created superuser %s. Sign in at /login.\n", *email)
}

func readPassword() string {
	for {
		first := prompt("Password (min 8 characters): ")
		if len(first) < 8 {
			fmt.Println("Too short.")
			continue
		}
		if prompt("Confirm password: ") != first {
			fmt.Println("Passwords don't match.")
			continue
		}
		return first
	}
}

var stdin = bufio.NewReader(os.Stdin)

// prompt reads a line, hiding what's typed when run in a terminal.
func prompt(label string) string {
	fmt.Print(label)
	if setEcho(false) == nil {
		defer func() {
			_ = setEcho(true)
			fmt.Println()
		}()
	}
	line, _ := stdin.ReadString('\n')
	return strings.TrimRight(line, "\r\n")
}

// setEcho turns terminal echo on or off with stty; it fails harmlessly when
// stdin isn't a terminal (e.g. a piped password).
func setEcho(on bool) error {
	arg := "-echo"
	if on {
		arg = "echo"
	}
	cmd := exec.Command("stty", arg)
	cmd.Stdin = os.Stdin
	return cmd.Run()
}

func hashPassword(pw string) string {
	hash, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	if err != nil {
		log.Fatalf("hash password: %v", err)
	}
	return string(hash)
}
