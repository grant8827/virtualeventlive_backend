package services

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// TestPlaybackKeyPEM returns a fresh P-384 private key in PKCS#8 PEM, the
// format IVS playback key pairs use. Exported for handler tests via
// NewTestPlaybackSigner.
func testPlaybackKeyPEM(t *testing.T, curve elliptic.Curve) string {
	t.Helper()
	k, err := ecdsa.GenerateKey(curve, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

func TestSignedPlaybackURLCarriesVerifiableShortLivedToken(t *testing.T) {
	signer, err := NewPlaybackSigner(testPlaybackKeyPEM(t, elliptic.P384()), "https://virtualeventplus.com", 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	arn := "arn:aws:ivs:us-east-1:123456789012:channel/abcdEFGH1234"
	signed, expires, err := signer.SignURL("https://abc.us-east-1.playback.live-video.net/api/video/v1/x.m3u8", arn)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(signed)
	tok := u.Query().Get("token")
	if tok == "" {
		t.Fatal("signed URL has no token")
	}
	claims := jwt.MapClaims{}
	parsed, err := jwt.ParseWithClaims(tok, claims, func(*jwt.Token) (interface{}, error) { return signer.PublicKey(), nil },
		jwt.WithValidMethods([]string{"ES384"}), jwt.WithExpirationRequired())
	if err != nil || !parsed.Valid {
		t.Fatalf("token does not verify: %v", err)
	}
	if claims["aws:channel-arn"] != arn || claims["aws:access-control-allow-origin"] != "https://virtualeventplus.com" {
		t.Fatalf("wrong IVS claims: %v", claims)
	}
	if d := time.Until(expires); d <= 0 || d > 5*time.Minute+time.Second {
		t.Fatalf("token lifetime %v, want ≤ 5m", d)
	}
}

func TestPlaybackSignerRejectsBadKeysAndMissingChannel(t *testing.T) {
	if _, err := NewPlaybackSigner("not a key", "", 0); err == nil {
		t.Error("garbage key accepted")
	}
	if _, err := NewPlaybackSigner(testPlaybackKeyPEM(t, elliptic.P256()), "", 0); err == nil {
		t.Error("P-256 key accepted; IVS requires P-384")
	}
	// Env vars often store PEM with literal \n.
	escaped := strings.ReplaceAll(testPlaybackKeyPEM(t, elliptic.P384()), "\n", `\n`)
	signer, err := NewPlaybackSigner(escaped, "", 0)
	if err != nil {
		t.Fatalf("escaped PEM rejected: %v", err)
	}
	if _, _, err := signer.SignURL("https://x/y.m3u8", ""); err == nil {
		t.Error("signing without a channel ARN must fail")
	}
}
