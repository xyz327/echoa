package auth

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v4"
)

// JWK 表示符合 RFC 7517 标准的单条 JSON Web Key
type JWK struct {
	Kty string   `json:"kty"`
	Kid string   `json:"kid"`
	Use string   `json:"use"`
	Alg string   `json:"alg"`
	N   string   `json:"n"`
	E   string   `json:"e"`
	Crv string   `json:"crv"`
	X   string   `json:"x"`
	Y   string   `json:"y"`
	X5c []string `json:"x5c"`
}

// JWKS 表示 RFC 7517 格式的公钥集合
type JWKS struct {
	Keys []JWK `json:"keys"`
}

// JWKSManager 负责拉取、解析并带缓存轮转 Casdoor 的 JWKS 公钥集合
type JWKSManager struct {
	jwksURI     string
	httpClient  *http.Client
	ttl         time.Duration
	minInterval time.Duration
	mu          sync.RWMutex
	keys        map[string]crypto.PublicKey
	rawKeys     []crypto.PublicKey
	lastFetched time.Time
}

// NewJWKSManager 创建一个具备内存缓存与防风暴主动轮转机制的 JWKS 管理器
func NewJWKSManager(uri string, ttl time.Duration) *JWKSManager {
	if ttl <= 0 {
		ttl = 1 * time.Hour
	}
	return &JWKSManager{
		jwksURI: uri,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
		ttl:         ttl,
		minInterval: 5 * time.Second,
		keys:        make(map[string]crypto.PublicKey),
		rawKeys:     make([]crypto.PublicKey, 0),
	}
}

// ResolveJWKSURI 根据配置解析 JWKS 地址，优先取 jwksURI，未配置时从 endpoint 推导
func ResolveJWKSURI(configuredURI, endpoint string) string {
	if strings.TrimSpace(configuredURI) != "" {
		return strings.TrimSpace(configuredURI)
	}
	endpoint = strings.TrimRight(strings.TrimSpace(endpoint), "/")
	if endpoint != "" {
		return endpoint + "/.well-known/jwks"
	}
	return ""
}

// GetKey 根据 kid 获取公钥；若缓存未命中或过期，自动拉取更新
func (m *JWKSManager) GetKey(ctx context.Context, kid string) (crypto.PublicKey, error) {
	if m == nil || m.jwksURI == "" {
		return nil, errors.New("jwks manager is not configured")
	}

	// 1. 快路径：读锁检查缓存
	m.mu.RLock()
	if !m.lastFetched.IsZero() && time.Since(m.lastFetched) < m.ttl {
		if kid != "" {
			if key, ok := m.keys[kid]; ok {
				m.mu.RUnlock()
				return key, nil
			}
		} else if len(m.rawKeys) == 1 {
			key := m.rawKeys[0]
			m.mu.RUnlock()
			return key, nil
		}
	}
	m.mu.RUnlock()

	// 2. 慢路径：写锁刷新缓存
	m.mu.Lock()
	defer m.mu.Unlock()

	// 双重检查
	if !m.lastFetched.IsZero() && time.Since(m.lastFetched) < m.ttl {
		if kid != "" {
			if key, ok := m.keys[kid]; ok {
				return key, nil
			}
		} else if len(m.rawKeys) == 1 {
			return m.rawKeys[0], nil
		}
	}

	// 限制最小刷新间隔，避免恶意无效 kid 频繁打垮认证服务器
	if !m.lastFetched.IsZero() && time.Since(m.lastFetched) < m.minInterval {
		if kid != "" {
			if key, ok := m.keys[kid]; ok {
				return key, nil
			}
		} else if len(m.rawKeys) > 0 {
			return m.rawKeys[0], nil
		}
		return nil, fmt.Errorf("public key with kid %q not found in JWKS (rate limited)", kid)
	}

	if err := m.fetchLocked(ctx); err != nil {
		// 网络故障时，若本地已有旧缓存则尽量宽容服务
		if kid != "" {
			if key, ok := m.keys[kid]; ok {
				return key, nil
			}
		} else if len(m.rawKeys) > 0 {
			return m.rawKeys[0], nil
		}
		return nil, fmt.Errorf("failed to fetch JWKS from %s: %w", m.jwksURI, err)
	}

	if kid != "" {
		if key, ok := m.keys[kid]; ok {
			return key, nil
		}
		return nil, fmt.Errorf("public key with kid %q not found in fetched JWKS", kid)
	}

	if len(m.rawKeys) == 1 {
		return m.rawKeys[0], nil
	}
	if len(m.rawKeys) > 1 {
		return nil, errors.New("multiple keys in JWKS but token header has no kid")
	}
	return nil, errors.New("no public keys available in JWKS")
}

// Keyfunc 返回适用于 golang-jwt 的 Keyfunc
func (m *JWKSManager) Keyfunc() jwt.Keyfunc {
	return func(token *jwt.Token) (interface{}, error) {
		kid, _ := token.Header["kid"].(string)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return m.GetKey(ctx, kid)
	}
}

func (m *JWKSManager) fetchLocked(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.jwksURI, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected HTTP status %d from JWKS endpoint", resp.StatusCode)
	}

	var jwks JWKS
	if err := json.NewDecoder(resp.Body).Decode(&jwks); err != nil {
		return fmt.Errorf("failed to decode JWKS JSON: %w", err)
	}

	newKeys := make(map[string]crypto.PublicKey)
	newRawKeys := make([]crypto.PublicKey, 0, len(jwks.Keys))

	for _, k := range jwks.Keys {
		pubKey, err := parseJWKToPublicKey(k)
		if err != nil {
			continue
		}
		if k.Kid != "" {
			newKeys[k.Kid] = pubKey
		}
		newRawKeys = append(newRawKeys, pubKey)
	}

	if len(newRawKeys) == 0 {
		return errors.New("no valid public keys parsed from JWKS response")
	}

	m.keys = newKeys
	m.rawKeys = newRawKeys
	m.lastFetched = time.Now()
	return nil
}

func parseJWKToPublicKey(k JWK) (crypto.PublicKey, error) {
	// 优先从 x5c 证书链解析
	if len(k.X5c) > 0 {
		certDer, err := base64.StdEncoding.DecodeString(k.X5c[0])
		if err == nil {
			cert, err := x509.ParseCertificate(certDer)
			if err == nil && cert.PublicKey != nil {
				return cert.PublicKey, nil
			}
		}
	}

	switch k.Kty {
	case "RSA":
		nBytes, err := decodeBase64URLOrStd(k.N)
		if err != nil {
			return nil, fmt.Errorf("failed to decode RSA modulus n: %w", err)
		}
		eBytes, err := decodeBase64URLOrStd(k.E)
		if err != nil {
			return nil, fmt.Errorf("failed to decode RSA exponent e: %w", err)
		}

		nBig := new(big.Int).SetBytes(nBytes)
		var eInt int
		for _, b := range eBytes {
			eInt = (eInt << 8) | int(b)
		}
		if eInt <= 0 {
			eInt = 65537
		}

		return &rsa.PublicKey{
			N: nBig,
			E: eInt,
		}, nil

	case "EC":
		var curve elliptic.Curve
		switch k.Crv {
		case "P-256":
			curve = elliptic.P256()
		case "P-384":
			curve = elliptic.P384()
		case "P-521":
			curve = elliptic.P521()
		default:
			return nil, fmt.Errorf("unsupported EC curve %q", k.Crv)
		}

		xBytes, err := decodeBase64URLOrStd(k.X)
		if err != nil {
			return nil, fmt.Errorf("failed to decode EC coordinate x: %w", err)
		}
		yBytes, err := decodeBase64URLOrStd(k.Y)
		if err != nil {
			return nil, fmt.Errorf("failed to decode EC coordinate y: %w", err)
		}

		return &ecdsa.PublicKey{
			Curve: curve,
			X:     new(big.Int).SetBytes(xBytes),
			Y:     new(big.Int).SetBytes(yBytes),
		}, nil

	default:
		return nil, fmt.Errorf("unsupported key type: %s", k.Kty)
	}
}

func decodeBase64URLOrStd(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if b, err := base64.RawURLEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	if b, err := base64.URLEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	if b, err := base64.RawStdEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return base64.StdEncoding.DecodeString(s)
}
