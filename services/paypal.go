package services

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// PayPalService sends host payouts via PayPal's Payouts API:
// https://developer.paypal.com/docs/payouts/standard/
type PayPalService struct {
	ClientID     string
	ClientSecret string
	Environment  string // "sandbox" or "live"
}

// CheckoutOrderRequest describes a one-time PayPal Checkout payment.
type CheckoutOrderRequest struct {
	Amount      float64
	Description string
	Reference   string
	ReturnURL   string
	CancelURL   string
}

type CheckoutOrder struct {
	ID          string
	ApprovalURL string
}

func (p *PayPalService) Enabled() bool {
	return p.ClientID != "" && p.ClientSecret != ""
}

func (p *PayPalService) baseURL() string {
	if p.Environment == "live" {
		return "https://api-m.paypal.com"
	}
	return "https://api-m.sandbox.paypal.com"
}

func (p *PayPalService) accessToken() (string, error) {
	form := url.Values{"grant_type": {"client_credentials"}}
	req, err := http.NewRequest(http.MethodPost, p.baseURL()+"/v1/oauth2/token", bytes.NewBufferString(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(p.ClientID, p.ClientSecret)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("paypal oauth request: %w", err)
	}
	defer resp.Body.Close()

	var out struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("paypal oauth decode: %w", err)
	}
	if resp.StatusCode != http.StatusOK || out.AccessToken == "" {
		return "", fmt.Errorf("paypal oauth failed: %s (status %d)", out.Error, resp.StatusCode)
	}
	return out.AccessToken, nil
}

// CreateCheckoutOrder creates a PayPal order and returns the URL where the
// buyer approves it. Funds are not captured until CaptureCheckoutOrder runs.
func (p *PayPalService) CreateCheckoutOrder(order CheckoutOrderRequest) (*CheckoutOrder, error) {
	if !p.Enabled() {
		return nil, fmt.Errorf("paypal checkout not configured")
	}
	token, err := p.accessToken()
	if err != nil {
		return nil, err
	}

	body := map[string]any{
		"intent": "CAPTURE",
		"purchase_units": []map[string]any{{
			"reference_id": order.Reference,
			"description":  order.Description,
			"amount": map[string]string{
				"currency_code": "USD",
				"value":         strconv.FormatFloat(order.Amount, 'f', 2, 64),
			},
		}},
		"application_context": map[string]string{
			"return_url":  order.ReturnURL,
			"cancel_url":  order.CancelURL,
			"brand_name":  "VirtualEventLive",
			"user_action": "PAY_NOW",
		},
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, p.baseURL()+"/v2/checkout/orders", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("paypal order request: %w", err)
	}
	defer resp.Body.Close()
	var out struct {
		ID    string `json:"id"`
		Links []struct {
			Href string `json:"href"`
			Rel  string `json:"rel"`
		} `json:"links"`
		Name    string `json:"name"`
		Message string `json:"message"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("paypal order decode: %w", err)
	}
	if resp.StatusCode >= 300 || out.ID == "" {
		return nil, fmt.Errorf("paypal order failed: %s %s (status %d)", out.Name, out.Message, resp.StatusCode)
	}
	for _, link := range out.Links {
		if link.Rel == "approve" {
			return &CheckoutOrder{ID: out.ID, ApprovalURL: link.Href}, nil
		}
	}
	return nil, fmt.Errorf("paypal order did not include an approval URL")
}

// CaptureCheckoutOrder captures a buyer-approved PayPal order.
func (p *PayPalService) CaptureCheckoutOrder(orderID string) error {
	if !p.Enabled() {
		return fmt.Errorf("paypal checkout not configured")
	}
	token, err := p.accessToken()
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, p.baseURL()+"/v2/checkout/orders/"+url.PathEscape(orderID)+"/capture", bytes.NewBufferString("{}"))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("paypal capture request: %w", err)
	}
	defer resp.Body.Close()
	var out struct {
		Status  string `json:"status"`
		Name    string `json:"name"`
		Message string `json:"message"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("paypal capture decode: %w", err)
	}
	if resp.StatusCode >= 300 || out.Status != "COMPLETED" {
		return fmt.Errorf("paypal capture failed: %s %s (status %d)", out.Name, out.Message, resp.StatusCode)
	}
	return nil
}

// SendPayout pays receiverEmail amountUSD via a single-item PayPal payout batch
// and returns the batch ID for reconciliation.
func (p *PayPalService) SendPayout(receiverEmail string, amountUSD float64, note string) (string, error) {
	if !p.Enabled() {
		return "", fmt.Errorf("paypal not configured")
	}

	token, err := p.accessToken()
	if err != nil {
		return "", err
	}

	body := map[string]any{
		"sender_batch_header": map[string]any{
			"sender_batch_id": fmt.Sprintf("vel-%d", time.Now().UnixNano()),
			"email_subject":   "You have a payout from VirtualEventLive",
		},
		"items": []map[string]any{
			{
				"recipient_type": "EMAIL",
				"receiver":       receiverEmail,
				"note":           note,
				"amount": map[string]string{
					"value":    strconv.FormatFloat(amountUSD, 'f', 2, 64),
					"currency": "USD",
				},
			},
		},
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequest(http.MethodPost, p.baseURL()+"/v1/payments/payouts", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("paypal payout request: %w", err)
	}
	defer resp.Body.Close()

	var out struct {
		BatchHeader struct {
			PayoutBatchID string `json:"payout_batch_id"`
		} `json:"batch_header"`
		Name    string `json:"name"`
		Message string `json:"message"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("paypal payout decode: %w", err)
	}
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("paypal payout failed: %s %s (status %d)", out.Name, out.Message, resp.StatusCode)
	}

	return out.BatchHeader.PayoutBatchID, nil
}
