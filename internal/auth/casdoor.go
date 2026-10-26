package auth

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/casdoor/casdoor-go-sdk/casdoorsdk"
	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/common/utils"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
	"echoa/internal/config"
)

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

// CasdoorMiddleware 实现 MCP OAuth 2.1 规范的鉴权中间件
func CasdoorMiddleware(cfg config.AuthConfig, client *casdoorsdk.Client) app.HandlerFunc {
	prmURL := strings.TrimRight(cfg.ResourceURL, "/") + "/.well-known/oauth-protected-resource"
	scopeArg := strings.Join(cfg.ScopesRequired, " ")

	return func(ctx context.Context, c *app.RequestContext) {
		// 开发模式或未开启鉴权时直接放行
		if !cfg.Enabled {
			devClaims := &casdoorsdk.Claims{
				User: casdoorsdk.User{
					Name:        "developer",
					DisplayName: "Local Developer",
					Owner:       "local",
				},
			}
			c.Set("claims", devClaims)
			c.Next(ctx)
			return
		}

		var token string

		// 1. 优先从 Authorization: Bearer <token> 请求头提取（标准推荐方式）
		authHeader := string(c.GetHeader("Authorization"))
		if strings.HasPrefix(authHeader, "Bearer ") {
			token = strings.TrimPrefix(authHeader, "Bearer ")
		}

		// 2. 兼容老旧 SSE 客户端通过 Query 参数传 Token（已标记 Deprecated，记录安全告警）
		if token == "" {
			if qToken := string(c.Query("token")); qToken != "" {
				log.Printf("[WARN] Deprecated: Client (%s) sent token in query parameter 'token'. Please use 'Authorization: Bearer <token>' header instead to prevent token leakage in proxy logs.", c.ClientIP())
				token = qToken
			} else if qAccessToken := string(c.Query("access_token")); qAccessToken != "" {
				log.Printf("[WARN] Deprecated: Client (%s) sent token in query parameter 'access_token'. Please use 'Authorization: Bearer <token>' header instead to prevent token leakage in proxy logs.", c.ClientIP())
				token = qAccessToken
			}
		}

		// 未携带 Token：按照 RFC 9728 规范下发 WWW-Authenticate 与 PRM 元数据地址
		if token == "" {
			c.Header("WWW-Authenticate", fmt.Sprintf(`Bearer resource_metadata="%s", scope="%s"`, prmURL, scopeArg))
			c.AbortWithStatusJSON(consts.StatusUnauthorized, utils.H{
				"jsonrpc": "2.0",
				"error": utils.H{
					"code":    -32001,
					"message": "Unauthorized: missing bearer token",
				},
			})
			return
		}

		// 3. 校验 JWT (Casdoor 无状态公钥验签)
		claims, err := client.ParseJwtToken(token)
		if err != nil {
			c.Header("WWW-Authenticate", fmt.Sprintf(`Bearer error="invalid_token", error_description="%s", resource_metadata="%s"`, err.Error(), prmURL))
			c.AbortWithStatusJSON(consts.StatusUnauthorized, utils.H{
				"jsonrpc": "2.0",
				"error": utils.H{
					"code":    -32001,
					"message": "Unauthorized: " + err.Error(),
				},
			})
			return
		}

		c.Set("claims", claims)
		c.Next(ctx)
	}
}

// ProtectedResourceMetadataHandler 提供 RFC 9728 元数据端点
func ProtectedResourceMetadataHandler(cfg config.AuthConfig) app.HandlerFunc {
	metadata := ProtectedResourceMetadata{
		Resource:               cfg.ResourceURL,
		AuthorizationServers:   []string{cfg.Endpoint},
		ScopesSupported:        cfg.ScopesRequired,
		BearerMethodsSupported: []string{"header"},
		ResourceName:           "Echoa",
	}

	return func(ctx context.Context, c *app.RequestContext) {
		c.JSON(consts.StatusOK, metadata)
	}
}
