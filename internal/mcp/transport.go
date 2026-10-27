package mcp

import (
	"context"
	"encoding/json"
)

// callOpts carries per-request transport options.
type callOpts struct {
	// Name overrides the Mcp-Name routing header (2026-07-28).
	Name string
	// Headers are mirrored tool arguments (x-mcp-header -> Mcp-Param-<Name>).
	Headers map[string]string
	// Streaming asks the transport not to apply a request timeout (long-lived streams).
	Streaming bool
}

// transport moves JSON-RPC 2.0 messages between this client and one server. Two eras and
// two transports implement it:
//
//	era modern (2026-07-28): stateless — no initialize; every request carries _meta with
//	  protocolVersion/clientCapabilities/clientInfo; server-initiated requests do not exist
//	  (multi round-trip results replace them); on HTTP, closing the response stream is the
//	  cancellation signal.
//	era legacy (2025-11-25): initialize/notifications/initialized handshake, optional
//	  Mcp-Session-Id, a GET SSE stream, and server-initiated JSON-RPC requests.
type transport interface {
	// call sends one request and waits for its response. onMsg receives the messages that
	// belong to this request (progress, log, or notifications sharing the response stream);
	// it runs on the transport's reader goroutine and must not block.
	call(ctx context.Context, method string, params any, onMsg func(*msg), opts *callOpts) (json.RawMessage, *rpcError, error)
	// notify sends a notification and does not wait for anything.
	notify(ctx context.Context, method string, params any) error
	// respond answers a server-initiated request (legacy era only).
	respond(ctx context.Context, id json.RawMessage, result any, e *rpcError) error
	// incoming delivers server-initiated requests. nil when the era cannot produce them.
	incoming() <-chan *msg
	// close shuts the transport down.
	close() error
	// label is used in error messages.
	label() string
}

// toRaw normalises a params value.
func toRaw(v any) (json.RawMessage, error) {
	if v == nil {
		return nil, nil
	}
	if raw, ok := v.(json.RawMessage); ok {
		return raw, nil
	}
	return json.Marshal(v)
}

// idKey makes a JSON-RPC id usable as a map key.
func idKey(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	return string(raw)
}
