package services

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// WiPayService sends host payouts through WiPay's merchant API. WiPay's public
// docs cover accepting payments (Direct Payment API) in detail but not a
// standard disbursement/payout endpoint, so WipayAPIBaseURL is left blank
// until WiPay's merchant support confirms the correct path for this account —
// SendPayout fails closed rather than guessing at a request shape that could
// silently misroute a host's money.
type WiPayService struct {
	AccountNumber string
	CheckoutURL   string
	CountryCode   string
	Currency      string
	FeeStructure  string
	APIBaseURL    string
	APIKey        string
	Environment   string // "sandbox" or "live"
}

func (w *WiPayService) Enabled() bool {
	return w.APIBaseURL != "" && w.APIKey != ""
}

func (w *WiPayService) CheckoutEnabled() bool {
	return w.CheckoutURL != "" && w.AccountNumber != "" && w.APIKey != ""
}

type HostedCheckout struct {
	Method string            `json:"checkout_method"`
	URL    string            `json:"checkout_url"`
	Fields map[string]string `json:"checkout_fields,omitempty"`
}

type HostedCheckoutRequest struct {
	Amount      float64
	Description string
	Reference   string
	SuccessURL  string
	CancelURL   string
}

func (w *WiPayService) BuildHostedCheckout(req HostedCheckoutRequest) (*HostedCheckout, error) {
	if !w.CheckoutEnabled() {
		return nil, fmt.Errorf("wipay checkout not configured")
	}

	currency := w.Currency
	if currency == "" {
		currency = "USD"
	}
	countryCode := w.CountryCode
	if countryCode == "" {
		countryCode = "JM"
	}
	feeStructure := w.FeeStructure
	if feeStructure == "" {
		feeStructure = "merchant_absorb"
	}

	return &HostedCheckout{
		Method: http.MethodPost,
		URL:    w.CheckoutURL,
		Fields: map[string]string{
			"account_number": w.AccountNumber,
			"api_key":        w.APIKey,
			"country_code":   countryCode,
			"currency":       currency,
			"environment":    w.Environment,
			"fee_structure":  feeStructure,
			"method":         "credit_card",
			"order_id":       req.Reference,
			"origin":         "Virtual Event Plus",
			"response_url":   req.SuccessURL,
			"total":          strconv.FormatFloat(req.Amount, 'f', 2, 64),
		},
	}, nil
}

// SendPayout pays accountID (the host's WiPay account number) amountUSD.
// currencyCode defaults to TTD (WiPay's home currency) when empty.
func (w *WiPayService) SendPayout(accountID string, amountUSD float64, currencyCode string) (string, error) {
	if !w.Enabled() {
		return "", fmt.Errorf("wipay not configured")
	}
	if currencyCode == "" {
		currencyCode = "TTD"
	}

	body := map[string]any{
		"account_number": accountID,
		"amount":         strconv.FormatFloat(amountUSD, 'f', 2, 64),
		"currency":       currencyCode,
		"reference":      fmt.Sprintf("vel-%d", time.Now().UnixNano()),
		"environment":    w.Environment,
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequest(http.MethodPost, w.APIBaseURL+"/payouts", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+w.APIKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("wipay payout request: %w", err)
	}
	defer resp.Body.Close()

	var out struct {
		TransactionID string `json:"transaction_id"`
		Status        string `json:"status"`
		Message       string `json:"message"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("wipay payout decode: %w", err)
	}
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("wipay payout failed: %s (status %d)", out.Message, resp.StatusCode)
	}

	return out.TransactionID, nil
}
