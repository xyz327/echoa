package auth

import (
	"testing"

	"echoa/internal/config"
	"github.com/casdoor/casdoor-go-sdk/casdoorsdk"
	"github.com/golang-jwt/jwt/v4"
)

func TestAudienceMatchesResource(t *testing.T) {
	resource := "https://echoa.zz7990.com"
	tests := []struct {
		aud  string
		want bool
	}{
		{aud: "https://echoa.zz7990.com", want: true},
		{aud: "https://echoa.zz7990.com/", want: true},
		{aud: "https://echoa.zz7990.com/all", want: true},
		{aud: "https://echoa.zz7990.com/mysql", want: true},
		{aud: "https://echoa.zz7990.com/redis", want: true},
		{aud: "https://echoa.zz7990.com/all/extra", want: false},
		{aud: "https://echoa.zz7990.com/all?x=1", want: false},
		{aud: "https://evil.example/all", want: false},
		{aud: "f983ca0f75f64773a09c", want: false},
		{aud: "", want: false},
	}
	for _, tt := range tests {
		if got := audienceMatchesResource(tt.aud, resource); got != tt.want {
			t.Fatalf("audienceMatchesResource(%q) = %v, want %v", tt.aud, got, tt.want)
		}
	}
}

func TestAudienceAccepted(t *testing.T) {
	cfg := config.AuthConfig{
		Audience:    "https://echoa.zz7990.com",
		ResourceURL: "https://echoa.zz7990.com",
		ClientID:    "f983ca0f75f64773a09c",
	}

	mustAccept := []string{
		"https://echoa.zz7990.com",
		"https://echoa.zz7990.com/all",
		"https://echoa.zz7990.com/mysql",
	}
	for _, aud := range mustAccept {
		claims := &casdoorsdk.Claims{RegisteredClaims: jwt.RegisteredClaims{Audience: jwt.ClaimStrings{aud}}}
		if !audienceAccepted(claims, cfg) {
			t.Fatalf("expected accept aud=%q", aud)
		}
	}

	claims := &casdoorsdk.Claims{RegisteredClaims: jwt.RegisteredClaims{Audience: jwt.ClaimStrings{"https://evil.example/all"}}}
	if audienceAccepted(claims, cfg) {
		t.Fatal("expected reject foreign resource aud")
	}

	// 仍接受显式配置的 client_id audience（过渡 / 非 resource 模式）
	cfgClient := config.AuthConfig{
		Audience:    "f983ca0f75f64773a09c",
		ResourceURL: "https://echoa.zz7990.com",
		ClientID:    "f983ca0f75f64773a09c",
	}
	clientClaims := &casdoorsdk.Claims{RegisteredClaims: jwt.RegisteredClaims{Audience: jwt.ClaimStrings{"f983ca0f75f64773a09c"}}}
	if !audienceAccepted(clientClaims, cfgClient) {
		t.Fatal("expected accept configured client_id audience")
	}
	pathClaims := &casdoorsdk.Claims{RegisteredClaims: jwt.RegisteredClaims{Audience: jwt.ClaimStrings{"https://echoa.zz7990.com/all"}}}
	if !audienceAccepted(pathClaims, cfgClient) {
		t.Fatal("expected accept resource+/path even when audience is client_id")
	}
}
