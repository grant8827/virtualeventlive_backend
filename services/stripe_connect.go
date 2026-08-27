package services

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
)

// CreateExpressAccountV2 creates a Connect account via Stripe's Accounts v2
// API (POST /v2/core/accounts) and returns its ID.
//
// stripe-go v82.5.1 has no typed client for this endpoint yet (only
// v2/core/event and v2/core/eventdestination), so this talks to it directly.
// Accounts v1 (the account.New/account package the rest of this codebase
// still uses for everything except creation) is deprecated for new Connect
// integrations on this Stripe account; v1's account_links, which the hosted
// onboarding flow depends on, still accept a v2 account's ID directly, per
// https://docs.stripe.com/connect/accounts-v2 ("you can still pass the ID of
// a v2 Account to an Accounts v1 API endpoint") -- so only account creation
// itself needs to move.
//
// The "merchant" configuration with the card_payments capability is the v2
// equivalent of v1's Express account type with CardPayments+Transfers
// requested: it's what a destination charge (this codebase's
// PaymentIntentData.TransferData.Destination) needs the receiving account to
// have. v2's "recipient" configuration and its stripe_transfers capability
// are for the separate Transfers API ("indirect charges"), which this
// codebase doesn't use, so it's deliberately left off.
//
// dashboard is fixed to "express", matching the hosted Express Dashboard
// access hosts got under v1. Stripe requires fees_collector and
// losses_collector to both be "application" for that combination unless
// you opt into a currently-preview API version for Stripe-owned liability;
// "application" also matches how a v1 Express account defaults for a
// marketplace like this one, so this isn't a liability change from before.
func CreateExpressAccountV2(secretKey, contactEmail string) (accountID string, err error) {
	if secretKey == "" {
		return "", fmt.Errorf("stripe secret key not configured")
	}

	type capability struct {
		Requested bool `json:"requested"`
	}
	body := map[string]any{
		"dashboard": "express",
		// v2 requires identity.country before it will accept a merchant
		// configuration -- v1 didn't need this because it silently defaulted
		// an omitted Country to the platform's own account country. Matching
		// that default explicitly (this platform's Stripe account is
		// US-registered) rather than asking the host up front keeps this a
		// like-for-like migration; a real per-host country selector, if
		// wanted, is a separate feature, not a v1->v2 compatibility fix.
		"identity": map[string]any{
			"country": "US",
		},
		"configuration": map[string]any{
			"merchant": map[string]any{
				"capabilities": map[string]any{
					"card_payments": capability{Requested: true},
				},
			},
		},
		"defaults": map[string]any{
			"responsibilities": map[string]string{
				"fees_collector":   "application",
				"losses_collector": "application",
			},
		},
	}
	if contactEmail != "" {
		body["contact_email"] = contactEmail
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequest(http.MethodPost, "https://api.stripe.com/v2/core/accounts", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+secretKey)
	// Unlike v1, the v2 API has no silent default -- omitting this header
	// fails every request with "You did not provide an API version."
	// Pinned to the version this account's own events were observed
	// carrying (see the ConstructEvent API-version mismatch this same
	// handler hit earlier): confirmed compatible, and only the stable `id`
	// field is read back here regardless.
	req.Header.Set("Stripe-Version", "2026-07-29.dahlia")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("stripe v2 account create request: %w", err)
	}
	defer resp.Body.Close()

	var out struct {
		ID    string `json:"id"`
		Error struct {
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"error"`
		// v2 errors have been observed to also come back flat (message/code
		// at the top level, matching v1's shape) depending on the failure --
		// decoded separately below rather than assumed.
		Message string `json:"message"`
		Code    string `json:"code"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("stripe v2 account create: unexpected response (status %d)", resp.StatusCode)
	}
	if resp.StatusCode >= 300 || out.ID == "" {
		msg := out.Error.Message
		if msg == "" {
			msg = out.Message
		}
		code := out.Error.Code
		if code == "" {
			code = out.Code
		}
		if msg == "" {
			msg = fmt.Sprintf("request failed with status %d", resp.StatusCode)
		}
		if code != "" {
			return "", fmt.Errorf("%s (%s)", msg, code)
		}
		return "", fmt.Errorf("%s", msg)
	}

	return out.ID, nil
}
