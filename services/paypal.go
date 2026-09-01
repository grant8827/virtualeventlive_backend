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
	ClientID          string
	ClientSecret      string
	Environment       string // "sandbox" or "live"
	PartnerMerchantID string
	BNCode            string
	WebhookID         string
}

type PayPalSellerStatus struct {
	MerchantID         string
	PaymentsReceivable bool
	EmailConfirmed     bool
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

type MarketplaceOrderRequest struct {
	Amount           float64
	PlatformFee      float64
	Description      string
	Reference        string
	SellerMerchantID string
	ReturnURL        string
	CancelURL        string
}

type MarketplaceCapture struct {
	OrderID   string
	CaptureID string
}

func (p *PayPalService) Enabled() bool {
	return p.ClientID != "" && p.ClientSecret != ""
}

func (p *PayPalService) MarketplaceEnabled() bool {
	return p.Enabled() && p.PartnerMerchantID != "" && p.BNCode != ""
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

// CreateSellerReferral starts PayPal-hosted seller onboarding. The host enters
// identity, tax, and bank details only on PayPal; the platform stores only the
// resulting merchant ID and eligibility flags.
func (p *PayPalService) CreateSellerReferral(trackingID, returnURL string) (string, error) {
	if !p.MarketplaceEnabled() {
		return "", fmt.Errorf("paypal marketplace onboarding is not configured")
	}
	token, err := p.accessToken()
	if err != nil {
		return "", err
	}
	body := map[string]any{
		"tracking_id": trackingID,
		"partner_config_override": map[string]any{
			"return_url":             returnURL,
			"return_url_description": "Return to Virtual Event Plus",
			"show_add_credit_card":   true,
		},
		"operations": []map[string]any{{
			"operation": "API_INTEGRATION",
			"api_integration_preference": map[string]any{
				"rest_api_integration": map[string]any{
					"integration_method": "PAYPAL",
					"integration_type":   "THIRD_PARTY",
					"third_party_details": map[string]any{
						"features": []string{"PAYMENT", "REFUND", "PARTNER_FEE"},
					},
				},
			},
		}},
		"products": []string{"PPCP"},
		"legal_consents": []map[string]any{{
			"type":    "SHARE_DATA_CONSENT",
			"granted": true,
		}},
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequest(http.MethodPost, p.baseURL()+"/v2/customer/partner-referrals", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("PayPal-Partner-Attribution-Id", p.BNCode)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("paypal seller referral request: %w", err)
	}
	defer resp.Body.Close()
	var out struct {
		Links []struct {
			Href string `json:"href"`
			Rel  string `json:"rel"`
		} `json:"links"`
		Name    string `json:"name"`
		Message string `json:"message"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("paypal seller referral decode: %w", err)
	}
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("paypal seller referral failed: %s %s (status %d)", out.Name, out.Message, resp.StatusCode)
	}
	for _, link := range out.Links {
		if link.Rel == "action_url" {
			return link.Href, nil
		}
	}
	return "", fmt.Errorf("paypal seller referral did not include an action URL")
}

// GetSellerStatus verifies a merchant ID directly with PayPal. Callback query
// parameters are never trusted on their own.
func (p *PayPalService) GetSellerStatus(merchantID string) (*PayPalSellerStatus, error) {
	if !p.MarketplaceEnabled() {
		return nil, fmt.Errorf("paypal marketplace onboarding is not configured")
	}
	token, err := p.accessToken()
	if err != nil {
		return nil, err
	}
	endpoint := fmt.Sprintf("%s/v1/customer/partners/%s/merchant-integrations/%s",
		p.baseURL(), url.PathEscape(p.PartnerMerchantID), url.PathEscape(merchantID))
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("PayPal-Partner-Attribution-Id", p.BNCode)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("paypal seller status request: %w", err)
	}
	defer resp.Body.Close()
	var out struct {
		MerchantIDPrimary     string `json:"merchant_id"`
		PaymentsReceivable    bool   `json:"payments_receivable"`
		PrimaryEmailConfirmed bool   `json:"primary_email_confirmed"`
		Name                  string `json:"name"`
		Message               string `json:"message"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("paypal seller status decode: %w", err)
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("paypal seller status failed: %s %s (status %d)", out.Name, out.Message, resp.StatusCode)
	}
	if out.MerchantIDPrimary == "" {
		out.MerchantIDPrimary = merchantID
	}
	return &PayPalSellerStatus{
		MerchantID:         out.MerchantIDPrimary,
		PaymentsReceivable: out.PaymentsReceivable,
		EmailConfirmed:     out.PrimaryEmailConfirmed,
	}, nil
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
			"brand_name":  "Virtual Event Plus",
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

// CreateMarketplaceOrder makes the connected host the PayPal payee and asks
// PayPal to disburse the platform commission to the partner account.
func (p *PayPalService) CreateMarketplaceOrder(order MarketplaceOrderRequest) (*CheckoutOrder, error) {
	if !p.MarketplaceEnabled() {
		return nil, fmt.Errorf("paypal marketplace checkout is not configured")
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
			"payee":        map[string]string{"merchant_id": order.SellerMerchantID},
			"amount": map[string]string{
				"currency_code": "USD",
				"value":         strconv.FormatFloat(order.Amount, 'f', 2, 64),
			},
			"payment_instruction": map[string]any{
				"disbursement_mode": "INSTANT",
				"platform_fees": []map[string]any{{
					"amount": map[string]string{
						"currency_code": "USD",
						"value":         strconv.FormatFloat(order.PlatformFee, 'f', 2, 64),
					},
				}},
			},
		}},
		"application_context": map[string]string{
			"return_url":  order.ReturnURL,
			"cancel_url":  order.CancelURL,
			"brand_name":  "Virtual Event Plus",
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
	req.Header.Set("PayPal-Partner-Attribution-Id", p.BNCode)
	req.Header.Set("PayPal-Request-Id", order.Reference)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("paypal marketplace order request: %w", err)
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
		return nil, fmt.Errorf("paypal marketplace order decode: %w", err)
	}
	if resp.StatusCode >= 300 || out.ID == "" {
		return nil, fmt.Errorf("paypal marketplace order failed: %s %s (status %d)", out.Name, out.Message, resp.StatusCode)
	}
	for _, link := range out.Links {
		if link.Rel == "approve" || link.Rel == "payer-action" {
			return &CheckoutOrder{ID: out.ID, ApprovalURL: link.Href}, nil
		}
	}
	return nil, fmt.Errorf("paypal marketplace order did not include an approval URL")
}

func (p *PayPalService) CaptureMarketplaceOrder(orderID string) (*MarketplaceCapture, error) {
	if !p.MarketplaceEnabled() {
		return nil, fmt.Errorf("paypal marketplace checkout is not configured")
	}
	token, err := p.accessToken()
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, p.baseURL()+"/v2/checkout/orders/"+url.PathEscape(orderID)+"/capture", bytes.NewBufferString("{}"))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("PayPal-Partner-Attribution-Id", p.BNCode)
	req.Header.Set("PayPal-Request-Id", "capture-"+orderID)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("paypal marketplace capture request: %w", err)
	}
	defer resp.Body.Close()
	var out struct {
		ID            string `json:"id"`
		Status        string `json:"status"`
		PurchaseUnits []struct {
			Payments struct {
				Captures []struct {
					ID     string `json:"id"`
					Status string `json:"status"`
				} `json:"captures"`
			} `json:"payments"`
		} `json:"purchase_units"`
		Name    string `json:"name"`
		Message string `json:"message"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("paypal marketplace capture decode: %w", err)
	}
	if resp.StatusCode >= 300 || out.Status != "COMPLETED" {
		return nil, fmt.Errorf("paypal marketplace capture failed: %s %s (status %d)", out.Name, out.Message, resp.StatusCode)
	}
	result := &MarketplaceCapture{OrderID: out.ID}
	for _, unit := range out.PurchaseUnits {
		for _, capture := range unit.Payments.Captures {
			if capture.Status == "COMPLETED" {
				result.CaptureID = capture.ID
				return result, nil
			}
		}
	}
	return nil, fmt.Errorf("paypal completed order did not include a completed capture")
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
			"email_subject":   "You have a payout from Virtual Event Plus",
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
