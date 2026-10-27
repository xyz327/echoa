package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"echoa/internal/config"
	"github.com/casdoor/casdoor-go-sdk/casdoorsdk"
	"github.com/golang-jwt/jwt/v4"
)

// generateRSAKeyPair 生成用于测试的 RSA 密钥对
func generateRSAKeyPair(t *testing.T) (*rsa.PrivateKey, *rsa.PublicKey, string) {
	t.Helper()
	privKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate RSA key: %v", err)
	}
	pubKeyBytes, err := x509.MarshalPKIXPublicKey(&privKey.PublicKey)
	if err != nil {
		t.Fatalf("failed to marshal public key: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{
		Type:  "PUBLIC KEY",
		Bytes: pubKeyBytes,
	})
	return privKey, &privKey.PublicKey, string(pemBytes)
}

func encodeJWKRSA(pubKey *rsa.PublicKey, kid string) JWK {
	nBase64 := base64.RawURLEncoding.EncodeToString(pubKey.N.Bytes())
	eBytes := big.NewInt(int64(pubKey.E)).Bytes()
	eBase64 := base64.RawURLEncoding.EncodeToString(eBytes)
	return JWK{
		Kty: "RSA",
		Kid: kid,
		Use: "sig",
		Alg: "RS256",
		N:   nBase64,
		E:   eBase64,
	}
}

func signToken(t *testing.T, privKey *rsa.PrivateKey, kid string, claims *casdoorsdk.Claims) string {
	t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	if kid != "" {
		token.Header["kid"] = kid
	}
	signed, err := token.SignedString(privKey)
	if err != nil {
		t.Fatalf("failed to sign token: %v", err)
	}
	return signed
}

func TestJWKSManagerFetchAndParse(t *testing.T) {
	_, pubKey, _ := generateRSAKeyPair(t)
	jwk := encodeJWKRSA(pubKey, "key-1")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(JWKS{Keys: []JWK{jwk}})
	}))
	defer server.Close()

	mgr := NewJWKSManager(server.URL, 1*time.Minute)
	key, err := mgr.GetKey(context.Background(), "key-1")
	if err != nil {
		t.Fatalf("failed to get key: %v", err)
	}
	rsaPub, ok := key.(*rsa.PublicKey)
	if !ok || rsaPub.N.Cmp(pubKey.N) != 0 {
		t.Errorf("expected matching public key")
	}

	// 测试空 kid 且只有一个 key 时的兜底匹配
	keyFallback, err := mgr.GetKey(context.Background(), "")
	if err != nil {
		t.Fatalf("failed to get key with empty kid: %v", err)
	}
	if keyFallback.(*rsa.PublicKey).N.Cmp(pubKey.N) != 0 {
		t.Errorf("expected matching fallback public key")
	}
}

func TestCasdoorMiddlewareJWKSAndAudience(t *testing.T) {
	privKey, pubKey, _ := generateRSAKeyPair(t)
	jwk := encodeJWKRSA(pubKey, "test-kid")

	jwksServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(JWKS{Keys: []JWK{jwk}})
	}))
	defer jwksServer.Close()

	cfg := config.AuthConfig{
		Enabled:        true,
		ClientID:       "mcp-service",
		Audience:       "f983ca0f75f64773a09c",
		JWKSURI:        jwksServer.URL,
		ScopesRequired: []string{"openid"},
		ResourceURL:    "http://127.0.0.1:8080",
	}

	jwksMgr := NewJWKSManager(cfg.JWKSURI, 1*time.Hour)
	handler := CasdoorMiddlewareWithJWKS(cfg, nil, jwksMgr)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		claims, ok := ClaimsFrom(r.Context())
		if !ok || claims == nil {
			t.Errorf("expected claims in context")
		}
		w.WriteHeader(http.StatusOK)
	}))

	// 1. 合法 Token (拥有匹配的 audience) -> 200
	validClaims := &casdoorsdk.Claims{
		User: casdoorsdk.User{Name: "alice"},
		RegisteredClaims: jwt.RegisteredClaims{
			Audience:  jwt.ClaimStrings{"f983ca0f75f64773a09c"},
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(1 * time.Hour)),
		},
	}
	validToken := signToken(t, privKey, "test-kid", validClaims)

	req1 := httptest.NewRequest(http.MethodPost, "/test", nil)
	req1.Header.Set("Authorization", "Bearer "+validToken)
	rr1 := httptest.NewRecorder()
	handler.ServeHTTP(rr1, req1)
	if rr1.Code != http.StatusOK {
		t.Errorf("expected 200 for valid token with correct audience, got %d, body: %s", rr1.Code, rr1.Body.String())
	}

	// 2. Audience 不匹配 (跨应用越权 Token) -> 401 Unauthorized
	wrongAudClaims := &casdoorsdk.Claims{
		User: casdoorsdk.User{Name: "attacker"},
		RegisteredClaims: jwt.RegisteredClaims{
			Audience:  jwt.ClaimStrings{"other-client-id"},
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(1 * time.Hour)),
		},
	}
	wrongAudToken := signToken(t, privKey, "test-kid", wrongAudClaims)

	req2 := httptest.NewRequest(http.MethodPost, "/test", nil)
	req2.Header.Set("Authorization", "Bearer "+wrongAudToken)
	rr2 := httptest.NewRecorder()
	handler.ServeHTTP(rr2, req2)
	if rr2.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for token with wrong audience, got %d", rr2.Code)
	}
	if !strings.Contains(rr2.Body.String(), "invalid audience") {
		t.Errorf("expected 'invalid audience' error message, got: %s", rr2.Body.String())
	}

	// 3. Refresh Token 被当作 Access Token 传入 -> 401 Unauthorized
	refreshClaims := &casdoorsdk.Claims{
		User:      casdoorsdk.User{Name: "alice"},
		TokenType: "refresh-token",
		RegisteredClaims: jwt.RegisteredClaims{
			Audience:  jwt.ClaimStrings{"f983ca0f75f64773a09c"},
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(1 * time.Hour)),
		},
	}
	refreshToken := signToken(t, privKey, "test-kid", refreshClaims)

	req3 := httptest.NewRequest(http.MethodPost, "/test", nil)
	req3.Header.Set("Authorization", "Bearer "+refreshToken)
	rr3 := httptest.NewRecorder()
	handler.ServeHTTP(rr3, req3)
	if rr3.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for refresh token, got %d", rr3.Code)
	}
	if !strings.Contains(rr3.Body.String(), "refresh token") {
		t.Errorf("expected 'refresh token' error message, got: %s", rr3.Body.String())
	}
}

func TestCasdoorMiddlewareFallbackToStaticCert(t *testing.T) {
	privKey, _, pubKeyPEM := generateRSAKeyPair(t)

	cfg := config.AuthConfig{
		Enabled:        true,
		ClientID:       "mcp-service",
		Certificate:    pubKeyPEM,
		JWKSURI:        "http://127.0.0.1:54321/non-existent-jwks", // 无效 JWKS 地址
		ScopesRequired: []string{"openid"},
		ResourceURL:    "http://127.0.0.1:8080",
	}

	casClient := casdoorsdk.NewClient("https://casdoor.example.com", cfg.ClientID, "secret", cfg.Certificate, "org", "app")

	handler := CasdoorMiddleware(cfg, casClient)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	claims := &casdoorsdk.Claims{
		User: casdoorsdk.User{Name: "bob"},
		RegisteredClaims: jwt.RegisteredClaims{
			Audience:  jwt.ClaimStrings{"mcp-service"}, // 默认取 client_id
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(1 * time.Hour)),
		},
	}
	token := signToken(t, privKey, "", claims)

	req := httptest.NewRequest(http.MethodPost, "/test", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected 200 via fallback to static certificate, got %d, body: %s", rr.Code, rr.Body.String())
	}
}
