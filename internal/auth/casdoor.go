package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"

	"echoa/internal/config"
	"github.com/casdoor/casdoor-go-sdk/casdoorsdk"
)

// ClaimsContextKey 是请求 context 中存放 *casdoorsdk.Claims 的键。
// 下游 handler 用它读取当前请求的身份声明。
var ClaimsContextKey = &struct{}{}

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
	prmURL := strings.TrimRight(cfg.ResourceURL, "/") + "/.well-known/oauth-protected-resource"
	scopeArg := strings.Join(cfg.ScopesRequired, " ")

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

			// 2. 兼容老旧 SSE 客户端通过 Query 参数传 Token（已标记 Deprecated，记录安全告警）
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

			// 3. 校验 JWT (Casdoor 无状态公钥验签)
			claims, err := client.ParseJwtToken(token)
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
