package mcp

import (
	"encoding/json"
	"fmt"
	"strconv"
	"sync/atomic"
)

// msg is a JSON-RPC 2.0 request, notification, response or error response.
type msg struct {
	JSONRPC string          `json:"jsonrpc,omitempty"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

func (m *msg) isRequest() bool     { return m.Method != "" && len(m.ID) > 0 }
func (m *msg) isNotification() bool { return m.Method != "" && len(m.ID) == 0 }
func (m *msg) isResponse() bool    { return m.Method == "" && len(m.ID) > 0 }

// rpcError is a JSON-RPC error object.
type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string {
	if e == nil {
		return "<nil>"
	}
	return fmt.Sprintf("mcp error %d: %s", e.Code, e.Message)
}

// Supported reports whether the error is one of the errors introduced by the
// 2026-07-28 revision, i.e. whether the peer speaks a "modern" MCP (spec §versioning:
// "a recognized modern JSON-RPC error identifies a modern server").
func (e *rpcError) Modern() bool {
	if e == nil {
		return false
	}
	switch e.Code {
	case CodeHeaderMismatch, CodeMissingClientCapability, CodeUnsupportedVersion:
		return true
	}
	return false
}

// Error codes (2026-07-28 allocation policy).
const (
	CodeParseError          = -32700
	CodeInvalidRequest      = -32600
	CodeMethodNotFound      = -32601
	CodeInvalidParams       = -32602
	CodeInternalError       = -32603
	CodeHeaderMismatch      = -32020
	CodeMissingClientCapability = -32021
	CodeUnsupportedVersion  = -32022
)

// idSource hands out JSON-RPC ids.
type idSource struct{ n atomic.Int64 }

func (s *idSource) next() json.RawMessage {
	return json.RawMessage(strconv.FormatInt(s.n.Add(1), 10))
}

// key names reserved by the 2026-07-28 revision.
const (
	metaKey               = "_meta"
	metaProtocolVersion   = "io.modelcontextprotocol/protocolVersion"
	metaClientInfo        = "io.modelcontextprotocol/clientInfo"
	metaClientCaps        = "io.modelcontextprotocol/clientCapabilities"
	metaLogLevel          = "io.modelcontextprotocol/logLevel"
	metaServerInfo        = "io.modelcontextprotocol/serverInfo"
	metaSubscriptionID    = "io.modelcontextprotocol/subscriptionId"
	metaProgressToken     = "progressToken"
	protocolModernVersion = "2026-07-28"
	protocolLegacyVersion = "2025-11-25"
)

// Methods we speak.
const (
	mDiscover            = "server/discover"
	mInitialize          = "initialize"
	mInitialized         = "notifications/initialized"
	mToolsList           = "tools/list"
	mToolsCall           = "tools/call"
	mCancelled           = "notifications/cancelled"
	mSubscribe           = "subscriptions/listen"
	mElicitation         = "elicitation/create"
	mRootsList           = "roots/list"
	mSamplingCreate      = "sampling/createMessage"
	mNotifySubAck        = "notifications/subscriptions/acknowledged"
	mToolsListChanged    = "notifications/tools/list_changed"
	mLogging             = "notifications/message"
	mProgress            = "notifications/progress"
)

// Implementation list (the only one we need).
var impl = map[string]any{"name": "bareloop", "version": "0.1.0"}

// marshalMsg renders a JSON-RPC message line.
func marshalMsg(m *msg) ([]byte, error) {
	if m.JSONRPC == "" {
		m.JSONRPC = "2.0"
	}
	return json.Marshal(m)
}
