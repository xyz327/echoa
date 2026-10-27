package mcp

import "encoding/json"

const (
	// ProtocolVersion20260728 是 MCP 2026-07-28 无状态 Streamable HTTP 协议版本
	ProtocolVersion20260728 = "2026-07-28"

	// JSON-RPC 2.0 及 MCP 规范保留错误码
	ErrCodeParseError      = -32700
	ErrCodeInvalidRequest  = -32600
	ErrCodeMethodNotFound  = -32601
	ErrCodeInvalidParams   = -32602
	ErrCodeInternalError   = -32603

	// MCP 2026-07-28 规范保留错误码范围 (-32020 ~ -32099)
	ErrCodeHeaderMismatch              = -32020
	ErrCodeMissingRequiredCapability   = -32021
	ErrCodeUnsupportedProtocolVersion  = -32022

	// 服务端应用级自定义错误码
	ErrCodeTooManyRequests = -32000
)

// JSONRPCRequest 基础请求报文
type JSONRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// JSONRPCResponse 基础响应报文
type JSONRPCResponse struct {
	JSONRPC string        `json:"jsonrpc"`
	ID      any           `json:"id,omitempty"`
	Result  any           `json:"result,omitempty"`
	Error   *JSONRPCError `json:"error,omitempty"`
}

// JSONRPCError 错误对象
type JSONRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// ServerCapabilities 表示服务端声明的能力集
type ServerCapabilities struct {
	Tools map[string]any `json:"tools"`
}

// ServerInfo 服务端软件实现信息
type ServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// DiscoverResult server/discover 方法的响应结果
type DiscoverResult struct {
	ResultType        string             `json:"resultType"` // "complete"
	SupportedVersions []string           `json:"supportedVersions"`
	Capabilities      ServerCapabilities `json:"capabilities"`
	Instructions      string             `json:"instructions,omitempty"`
	TTLMs             int64              `json:"ttlMs"`
	CacheScope        string             `json:"cacheScope"` // "public"
	Meta              map[string]any     `json:"_meta,omitempty"`
}

// ListToolsResult tools/list 方法的响应结果
type ListToolsResult struct {
	ResultType string            `json:"resultType"` // "complete"
	Tools      []*ToolDefinition `json:"tools"`
	TTLMs      int64             `json:"ttlMs"`
	CacheScope string            `json:"cacheScope"` // "public"
}

// ToolContent MCP Tool 文本内容
type ToolContent struct {
	Type string `json:"type"` // "text"
	Text string `json:"text"`
}

// CallToolResult tools/call 响应结果
type CallToolResult struct {
	ResultType string        `json:"resultType"` // "complete"
	Content    []ToolContent `json:"content"`
	IsError    bool          `json:"isError,omitempty"`
}

// CallToolParams tools/call 入参
type CallToolParams struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}
