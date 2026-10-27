package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"echoa/internal/config"
	"github.com/casdoor/casdoor-go-sdk/casdoorsdk"
	"github.com/golang-jwt/jwt/v4"
)

// ClaimsContextKey 是请求 context 中存放 *casdoorsdk.Claims 的键。
// 下游 handler 用它读取当前请求的身份声明。
var ClaimsContextKey = &struct{}{}

// ClaimsFrom 从 request context 中读取 Casdoor 声明
func ClaimsFrom(ctx context.Context) (*casdoorsdk.Claims, bool) {
	claims, ok := ctx.Value(ClaimsContextKey).(*casdoorsdk.Claims)
	return claims, ok
}

// SanitizeToken 对 Token 进行脱敏处理，防止敏感信息在日志中泄露
func SanitizeToken(token string) string {
	token = strings.TrimSpace(token)
	if token == "" {
		return ""
	}
	if len(token) <= 8 {
		return "[REDACTED]"
	}
	return token[:4] + "..." + token[len(token)-4:]
}

// CasdoorMiddleware 实现 MCP OAuth 2.1 规范的鉴权中间件。
// 校验通过后把 Casdoor 声明写入请求 context，供下游 handler 读取。
func CasdoorMiddleware(cfg config.AuthConfig, client *casdoorsdk.Client) func(http.Handler) http.Handler {
	return CasdoorMiddlewareWithJWKS(cfg, client, nil)
}

// CasdoorMiddlewareWithJWKS 允许显式注入 JWKSManager（方便单元测试 Mock）
func CasdoorMiddlewareWithJWKS(cfg config.AuthConfig, client *casdoorsdk.Client, jwksMgr *JWKSManager) func(http.Handler) http.Handler {
	prmURL := strings.TrimRight(cfg.ResourceURL, "/") + "/.well-known/oauth-protected-resource"
	scopeArg := strings.Join(cfg.ScopesRequired, " ")

	if jwksMgr == nil && cfg.Enabled {
		jwksURI := ResolveJWKSURI(cfg.JWKSURI, cfg.Endpoint)
		if jwksURI != "" {
			jwksMgr = NewJWKSManager(jwksURI, 1*time.Hour)
		}
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// 开发模式或未开启鉴权时直接放行
			if !cfg.Enabled {
				devClaims := &casdoorsdk.Claims{
					User: casdoorsdk.User{
						Name:        "developer",
						DisplayName: "Local Developer",
						Owner:       "local",
					},
				}
				next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ClaimsContextKey, devClaims)))
				return
			}

			var token string

			// 1. 优先从 Authorization: Bearer <token> 请求头提取（标准推荐方式）
			authHeader := r.Header.Get("Authorization")
			if strings.HasPrefix(authHeader, "Bearer ") {
				token = strings.TrimPrefix(authHeader, "Bearer ")
			}

			// 2. 兼容老旧客户端通过 Query 参数传 Token（已标记 Deprecated，记录安全告警）
			if token == "" {
				q := r.URL.Query()
				if qToken := q.Get("token"); qToken != "" {
					log.Printf("[WARN] Deprecated: Client (%s) sent token in query parameter 'token'. Please use 'Authorization: Bearer <token>' header instead to prevent token leakage in proxy logs.", clientIP(r))
					token = qToken
				} else if qAccessToken := q.Get("access_token"); qAccessToken != "" {
					log.Printf("[WARN] Deprecated: Client (%s) sent token in query parameter 'access_token'. Please use 'Authorization: Bearer <token>' header instead to prevent token leakage in proxy logs.", clientIP(r))
					token = qAccessToken
				}
			}

			// 未携带 Token：按照 RFC 9728 规范下发 WWW-Authenticate 与 PRM 元数据地址
			if token == "" {
				w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer resource_metadata="%s", scope="%s"`, prmURL, scopeArg))
				writeJSON(w, http.StatusUnauthorized, map[string]any{
					"jsonrpc": "2.0",
					"error": map[string]any{
						"code":    -32001,
						"message": "Unauthorized: missing bearer token",
					},
				})
				return
			}

			// 3. 校验 JWT：优先走 JWKS 动态公钥，失败时回退静态 PEM 证书；并强制校验 Audience 与 Refresh Token 拦截
			claims, err := parseAndValidateToken(r.Context(), token, cfg, jwksMgr, client)
			if err != nil {
				w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer error="invalid_token", error_description="%s", resource_metadata="%s"`, err.Error(), prmURL))
				writeJSON(w, http.StatusUnauthorized, map[string]any{
					"jsonrpc": "2.0",
					"error": map[string]any{
						"code":    -32001,
						"message": "Unauthorized: " + err.Error(),
					},
				})
				return
			}

			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ClaimsContextKey, claims)))
		})
	}
}

// parseAndValidateToken 统一处理签名验签、密钥回退、Audience 校验与 Refresh Token 拦截
func parseAndValidateToken(ctx context.Context, tokenStr string, cfg config.AuthConfig, jwksMgr *JWKSManager, client *casdoorsdk.Client) (*casdoorsdk.Claims, error) {
	var claims *casdoorsdk.Claims
	var lastErr error

	// 1. 优先尝试 JWKS 验签 (支持自动公钥轮转)
	if jwksMgr != nil {
		parsedClaims := &casdoorsdk.Claims{}
		parsedToken, err := jwt.ParseWithClaims(tokenStr, parsedClaims, jwksMgr.Keyfunc())
		if err == nil && parsedToken != nil && parsedToken.Valid {
			claims = parsedClaims
		} else {
			lastErr = err
		}
	}

	// 2. 若 JWKS 未能成功验签，且配置了静态证书，尝试回退到静态 PEM 证书验签
	if claims == nil && client != nil && strings.TrimSpace(cfg.Certificate) != "" {
		fallbackClaims, err := client.ParseJwtToken(tokenStr)
		if err == nil && fallbackClaims != nil {
			claims = fallbackClaims
		} else if lastErr == nil {
			lastErr = err
		}
	}

	if claims == nil {
		if lastErr != nil {
			return nil, lastErr
		}
		return nil, errors.New("no valid verification keys available or signature verification failed")
	}

	// 3. 拦截 OAuth Refresh Token (防止 Refresh Token 越权充当 Access Token)
	if claims.IsRefreshToken() {
		return nil, errors.New("refresh token cannot be used as access token")
	}

	// 4. 强制校验 Audience (aud)，杜绝 Casdoor 同一实例下不同 Client ID 跨应用伪造
	expectedAud := strings.TrimSpace(cfg.Audience)
	if expectedAud == "" {
		expectedAud = strings.TrimSpace(cfg.ClientID)
	}
	if expectedAud != "" && !claims.VerifyAudience(expectedAud, true) {
		return nil, fmt.Errorf("invalid audience, expected %q", expectedAud)
	}

	return claims, nil
}

// clientIP 取代理链中最左侧的客户端地址，没有代理头时退回 RemoteAddr。
func clientIP(r *http.Request) string {
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		if ip, _, ok := strings.Cut(fwd, ","); ok {
			return strings.TrimSpace(ip)
		}
		return strings.TrimSpace(fwd)
	}
	return r.RemoteAddr
}

// ProtectedResourceMetadataHandler 提供 RFC 9728 元数据端点
func ProtectedResourceMetadataHandler(cfg config.AuthConfig) http.HandlerFunc {
	metadata := ProtectedResourceMetadata{
		Resource:               cfg.ResourceURL,
		AuthorizationServers:   []string{cfg.Endpoint},
		ScopesSupported:        cfg.ScopesRequired,
		BearerMethodsSupported: []string{"header"},
		ResourceName:           "Echoa",
	}

	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, metadata)
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
