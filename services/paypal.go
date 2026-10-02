package services

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

// PayPalService implements PayPal Checkout and Commerce Platform multiparty
// seller onboarding. Host ticket revenue is split at capture time.
type PayPalService struct {
	ClientID             string
	ClientSecret         string
	Environment          string // "sandbox" or "live"
	PartnerMerchantID    string
	PartnerAttributionID string
	WebhookID            string
	// APIBase overrides the PayPal API host (tests point it at a local fake).
	APIBase string
}

// VerifyWebhookSignature asks PayPal to validate the signed transmission.
// Webhook bodies must never be acted on before this returns true.
func (p *PayPalService) VerifyWebhookSignature(transmissionID, transmissionTime, certURL, authAlgo, transmissionSig string, event json.RawMessage) (bool, error) {
	if !p.PartnerEnabled() || p.WebhookID == "" {
		return false, fmt.Errorf("paypal webhook verification not configured")
	}
	token, err := p.accessToken()
	if err != nil {
		return false, err
	}
	body := map[string]any{
		"transmission_id":   transmissionID,
		"transmission_time": transmissionTime,
		"cert_url":          certURL,
		"auth_algo":         authAlgo,
		"transmission_sig":  transmissionSig,
		"webhook_id":        p.WebhookID,
		"webhook_event":     event,
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return false, err
	}
	req, err := http.NewRequest(http.MethodPost, p.baseURL()+"/v1/notifications/verify-webhook-signature", bytes.NewReader(payload))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, fmt.Errorf("paypal webhook verification request: %w", err)
	}
	defer resp.Body.Close()
	var out struct {
		VerificationStatus string `json:"verification_status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return false, fmt.Errorf("paypal webhook verification decode: %w", err)
	}
	if resp.StatusCode >= 300 {
		return false, fmt.Errorf("paypal webhook verification failed (status %d)", resp.StatusCode)
	}
	return out.VerificationStatus == "SUCCESS", nil
}

// CheckoutOrderRequest describes a one-time PayPal Checkout payment.
// Amounts are cents and are sent to PayPal as exact decimal strings.
type CheckoutOrderRequest struct {
	AmountCents      int64
	Description      string
	Reference        string
	ReturnURL        string
	CancelURL        string
	PayeeMerchantID  string
	PlatformFeeCents int64
	// CustomID is our order ID; PayPal echoes it on captures, refunds and
	// disputes so notifications can be matched to the order.
	CustomID string
}

type CheckoutOrder struct {
	ID          string
	ApprovalURL string
}

func (p *PayPalService) Enabled() bool {
	return p.ClientID != "" && p.ClientSecret != ""
}

func (p *PayPalService) PartnerEnabled() bool {
	return p.Enabled() && p.PartnerMerchantID != "" && p.PartnerAttributionID != ""
}

func (p *PayPalService) authAssertion(merchantID string) string {
	header, _ := json.Marshal(map[string]string{"alg": "none"})
	payload, _ := json.Marshal(map[string]string{"iss": p.ClientID, "payer_id": merchantID})
	return base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload) + "."
}

type SellerStatus struct {
	MerchantID            string `json:"merchant_id"`
	TrackingID            string `json:"tracking_id"`
	PaymentsReceivable    bool   `json:"payments_receivable"`
	PrimaryEmailConfirmed bool   `json:"primary_email_confirmed"`
}

func (p *PayPalService) CreateSellerReferral(trackingID, returnURL string) (string, error) {
	if !p.PartnerEnabled() {
		return "", fmt.Errorf("paypal partner onboarding not configured")
	}
	token, err := p.accessToken()
	if err != nil {
		return "", err
	}
	body := map[string]any{
		"tracking_id":             trackingID,
		"operations":              []map[string]any{{"operation": "API_INTEGRATION", "api_integration_preference": map[string]any{"rest_api_integration": map[string]any{"integration_method": "PAYPAL", "integration_type": "THIRD_PARTY", "third_party_details": map[string]any{"features": []string{"PAYMENT", "REFUND", "PARTNER_FEE"}}}}}},
		"products":                []string{"EXPRESS_CHECKOUT"},
		"legal_consents":          []map[string]any{{"type": "SHARE_DATA_CONSENT", "granted": true}},
		"partner_config_override": map[string]string{"return_url": returnURL, "return_url_description": "Return to Virtual Event Plus"},
	}
	payload, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, p.baseURL()+"/v2/customer/partner-referrals", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("PayPal-Partner-Attribution-Id", p.PartnerAttributionID)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("paypal referral request: %w", err)
	}
	defer resp.Body.Close()
	var out struct {
		Links         []struct{ Href, Rel string } `json:"links"`
		Name, Message string
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("paypal referral failed: %s %s", out.Name, out.Message)
	}
	for _, link := range out.Links {
		if link.Rel == "action_url" {
			return link.Href, nil
		}
	}
	return "", fmt.Errorf("paypal referral did not include an action URL")
}

func (p *PayPalService) GetSellerStatus(merchantID string) (*SellerStatus, error) {
	if !p.PartnerEnabled() {
		return nil, fmt.Errorf("paypal partner onboarding not configured")
	}
	token, err := p.accessToken()
	if err != nil {
		return nil, err
	}
	endpoint := p.baseURL() + "/v1/customer/partners/" + url.PathEscape(p.PartnerMerchantID) + "/merchant-integrations/" + url.PathEscape(merchantID)
	req, _ := http.NewRequest(http.MethodGet, endpoint, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("PayPal-Partner-Attribution-Id", p.PartnerAttributionID)
	req.Header.Set("PayPal-Auth-Assertion", p.authAssertion(p.PartnerMerchantID))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out SellerStatus
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("paypal seller status failed (status %d)", resp.StatusCode)
	}
	return &out, nil
}

func (p *PayPalService) baseURL() string {
	if p.APIBase != "" {
		return p.APIBase
	}
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
			"custom_id":    order.CustomID,
			"description":  order.Description,
			"amount": map[string]string{
				"currency_code": "USD",
				"value":         FormatCents(order.AmountCents),
			},
		}},
		"application_context": map[string]string{
			"return_url":  order.ReturnURL,
			"cancel_url":  order.CancelURL,
			"brand_name":  "Virtual Event Plus",
			"user_action": "PAY_NOW",
		},
	}
	if order.PayeeMerchantID != "" {
		unit := body["purchase_units"].([]map[string]any)[0]
		unit["payee"] = map[string]string{"merchant_id": order.PayeeMerchantID}
		instruction := map[string]any{"disbursement_mode": "INSTANT"}
		if order.PlatformFeeCents > 0 {
			instruction["platform_fees"] = []map[string]any{{"amount": map[string]string{"currency_code": "USD", "value": FormatCents(order.PlatformFeeCents)}}}
		}
		unit["payment_instruction"] = instruction
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
	if order.PayeeMerchantID != "" {
		req.Header.Set("PayPal-Partner-Attribution-Id", p.PartnerAttributionID)
		req.Header.Set("PayPal-Auth-Assertion", p.authAssertion(order.PayeeMerchantID))
	}

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

// CapturedOrder is what PayPal reports was actually paid, so callers can
// check the payment belongs to what they're about to fulfil (PAY-04).
type CapturedOrder struct {
	Reference       string // the purchase unit's reference_id set at order creation
	CustomID        string // our order ID
	PayeeMerchantID string // the seller the money went to
	CaptureID       string // PayPal's payment reference, kept on the order
	Currency        string // e.g. USD
	AmountCents     int64  // total of the completed captures
}

type paypalAmount struct {
	CurrencyCode string `json:"currency_code"`
	Value        string `json:"value"`
}

// CaptureOrder captures a buyer-approved order and returns what was paid.
func (p *PayPalService) CaptureOrder(orderID string, merchantID ...string) (*CapturedOrder, error) {
	if !p.Enabled() {
		return nil, fmt.Errorf("paypal checkout not configured")
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
	// A stable request ID makes capture retries idempotent if PayPal succeeds
	// but our database transaction or network response fails afterward.
	req.Header.Set("PayPal-Request-Id", "capture-"+orderID)
	if len(merchantID) > 0 && merchantID[0] != "" {
		req.Header.Set("PayPal-Partner-Attribution-Id", p.PartnerAttributionID)
		req.Header.Set("PayPal-Auth-Assertion", p.authAssertion(merchantID[0]))
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("paypal capture request: %w", err)
	}
	defer resp.Body.Close()
	var out struct {
		Status        string `json:"status"`
		Name          string `json:"name"`
		Message       string `json:"message"`
		PurchaseUnits []struct {
			ReferenceID string `json:"reference_id"`
			CustomID    string `json:"custom_id"`
			Payee       struct {
				MerchantID string `json:"merchant_id"`
			} `json:"payee"`
			Payments struct {
				Captures []struct {
					ID       string       `json:"id"`
					Status   string       `json:"status"`
					CustomID string       `json:"custom_id"`
					Amount   paypalAmount `json:"amount"`
				} `json:"captures"`
			} `json:"payments"`
		} `json:"purchase_units"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("paypal capture decode: %w", err)
	}
	if resp.StatusCode >= 300 || out.Status != "COMPLETED" {
		return nil, fmt.Errorf("paypal capture failed: %s %s (status %d)", out.Name, out.Message, resp.StatusCode)
	}

	captured := &CapturedOrder{}
	if len(out.PurchaseUnits) > 0 {
		unit := out.PurchaseUnits[0]
		captured.Reference = unit.ReferenceID
		captured.CustomID = unit.CustomID
		captured.PayeeMerchantID = unit.Payee.MerchantID
		for _, c := range unit.Payments.Captures {
			if c.Status != "COMPLETED" {
				continue
			}
			v, err := ParseDecimalCents(c.Amount.Value)
			if err != nil {
				return nil, fmt.Errorf("paypal capture amount: %w", err)
			}
			if captured.Currency != "" && captured.Currency != c.Amount.CurrencyCode {
				return nil, fmt.Errorf("paypal capture has mixed currencies")
			}
			captured.AmountCents += v
			captured.Currency = c.Amount.CurrencyCode
			if captured.CaptureID == "" {
				captured.CaptureID = c.ID
			}
			if captured.CustomID == "" {
				captured.CustomID = c.CustomID
			}
		}
	}
	return captured, nil
}

// PayPalRefund is PayPal's answer to a refund request.
type PayPalRefund struct {
	ID          string
	Status      string // COMPLETED, PENDING, FAILED, CANCELLED
	AmountCents int64
}

// RefundCapture refunds amountCents of a capture from the seller's account,
// returning the proportional platform fee as well. requestID makes retries
// idempotent.
func (p *PayPalService) RefundCapture(captureID string, amountCents, platformFeeCents int64, merchantID, requestID, note string) (*PayPalRefund, error) {
	if !p.Enabled() {
		return nil, fmt.Errorf("paypal checkout not configured")
	}
	token, err := p.accessToken()
	if err != nil {
		return nil, err
	}
	body := map[string]any{
		"amount":        map[string]string{"currency_code": "USD", "value": FormatCents(amountCents)},
		"note_to_payer": note,
	}
	if platformFeeCents > 0 {
		body["payment_instruction"] = map[string]any{"platform_fees": []map[string]any{{
			"amount": map[string]string{"currency_code": "USD", "value": FormatCents(platformFeeCents)},
		}}}
	}
	payload, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, p.baseURL()+"/v2/payments/captures/"+url.PathEscape(captureID)+"/refund", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Prefer", "return=representation")
	req.Header.Set("PayPal-Request-Id", requestID)
	if merchantID != "" {
		req.Header.Set("PayPal-Partner-Attribution-Id", p.PartnerAttributionID)
		req.Header.Set("PayPal-Auth-Assertion", p.authAssertion(merchantID))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("paypal refund request: %w", err)
	}
	defer resp.Body.Close()
	var out struct {
		ID      string       `json:"id"`
		Status  string       `json:"status"`
		Amount  paypalAmount `json:"amount"`
		Name    string       `json:"name"`
		Message string       `json:"message"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("paypal refund decode: %w", err)
	}
	if resp.StatusCode >= 300 || out.ID == "" {
		return nil, fmt.Errorf("paypal refund failed: %s %s (status %d)", out.Name, out.Message, resp.StatusCode)
	}
	cents, err := ParseDecimalCents(out.Amount.Value)
	if err != nil {
		return nil, fmt.Errorf("paypal refund amount: %w", err)
	}
	return &PayPalRefund{ID: out.ID, Status: out.Status, AmountCents: cents}, nil
}

// PayPalCapture is a capture's current state, for reconciliation.
type PayPalCapture struct {
	ID          string
	Status      string // COMPLETED, PARTIALLY_REFUNDED, REFUNDED, ...
	Currency    string
	AmountCents int64
	FeeCents    int64 // PayPal's fee (charged to the seller)
}

// GetCapture reads a capture as PayPal currently sees it.
func (p *PayPalService) GetCapture(captureID string) (*PayPalCapture, error) {
	if !p.Enabled() {
		return nil, fmt.Errorf("paypal checkout not configured")
	}
	token, err := p.accessToken()
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodGet, p.baseURL()+"/v2/payments/captures/"+url.PathEscape(captureID), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("paypal capture lookup: %w", err)
	}
	defer resp.Body.Close()
	var out struct {
		ID                        string       `json:"id"`
		Status                    string       `json:"status"`
		Amount                    paypalAmount `json:"amount"`
		SellerReceivableBreakdown struct {
			PayPalFee paypalAmount `json:"paypal_fee"`
		} `json:"seller_receivable_breakdown"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("paypal capture lookup decode: %w", err)
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("paypal capture lookup failed (status %d)", resp.StatusCode)
	}
	amount, err := ParseDecimalCents(out.Amount.Value)
	if err != nil {
		return nil, err
	}
	capture := &PayPalCapture{ID: out.ID, Status: out.Status, Currency: out.Amount.CurrencyCode, AmountCents: amount}
	if v := out.SellerReceivableBreakdown.PayPalFee.Value; v != "" {
		if fee, err := ParseDecimalCents(v); err == nil {
			capture.FeeCents = fee
		}
	}
	return capture, nil
}
