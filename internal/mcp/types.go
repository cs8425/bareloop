package mcp

import (
	"encoding/base64"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// Implementation identifies an MCP peer.
type Implementation struct {
	Name    string `json:"name"`
	Title   string `json:"title,omitempty"`
	Version string `json:"version"`
}

// Tool is one entry of tools/list.
type Tool struct {
	Name        string          `json:"name"`
	Title       string          `json:"title,omitempty"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema,omitempty"`
	// ExecutableSchema is new in 2026-07-28 (optional).
	ExecutableSchema json.RawMessage  `json:"executableSchema,omitempty"`
	OutputSchema     json.RawMessage  `json:"outputSchema,omitempty"`
	Annotations      *ToolAnnotations `json:"annotations,omitempty"`
	Meta             map[string]any   `json:"_meta,omitempty"`
}

// ToolAnnotations carries MCP tool hints.
type ToolAnnotations struct {
	Title           string `json:"title,omitempty"`
	ReadOnlyHint    *bool  `json:"readOnlyHint,omitempty"`
	DestructiveHint *bool  `json:"destructiveHint,omitempty"`
	IdempotentHint  *bool  `json:"idempotentHint,omitempty"`
	OpenWorldHint   *bool  `json:"openWorldHint,omitempty"`
}

// DisplayName prefers title over name.
func (t Tool) DisplayName() string {
	if strings.TrimSpace(t.Title) != "" {
		return t.Title
	}
	return t.Name
}

// ContentBlock is one item of a tool result's content array. It is a superset of text,
// image, audio, resource_link and embedded-resource (resource) blocks, plus the legacy
// "resource" wrapper shape.
type ContentBlock struct {
	Type        string `json:"type"`
	Text        string `json:"text,omitempty"`
	Data        string `json:"data,omitempty"`
	MimeType    string `json:"mimeType,omitempty"`
	URI         string `json:"uri,omitempty"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	// Resource is the nested payload of a legacy "resource" block.
	Resource *ResourceContents `json:"resource,omitempty"`
	Meta     map[string]any    `json:"_meta,omitempty"`
}

// ResourceContents carries embedded or linked resource data.
type ResourceContents struct {
	URI      string `json:"uri,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
	Text     string `json:"text,omitempty"`
	Blob     string `json:"blob,omitempty"`
}

// Image reports whether the block should be attached to the model as an image.
func (c ContentBlock) Image() (string, string, bool) {
	switch c.Type {
	case "image":
		if c.Data != "" {
			mt := c.MimeType
			if mt == "" {
				mt = "image/png"
			}
			return c.Data, mt, true
		}
	case "resource":
		if c.Resource != nil && strings.HasPrefix(c.Resource.MimeType, "image/") && c.Resource.Blob != "" {
			return c.Resource.Blob, c.Resource.MimeType, true
		}
	}
	return "", "", false
}

// Audio reports whether the block carries audio data.
func (c ContentBlock) Audio() (string, string, bool) {
	if c.Type == "audio" && c.Data != "" {
		mt := c.MimeType
		if mt == "" {
			mt = "audio/wav"
		}
		return c.Data, mt, true
	}
	return "", "", false
}

// Text returns the textual form of a content block.
func (c ContentBlock) Text2() string {
	switch c.Type {
	case "", "text":
		return c.Text
	case "resource_link":
		s := "[resource link] " + c.URI
		if c.Name != "" {
			s += " " + c.Name
		}
		return s
	case "resource":
		if c.Resource == nil {
			return ""
		}
		if c.Resource.Text != "" {
			return c.Resource.Text
		}
		return "[embedded binary resource] " + c.Resource.URI
	case "image", "audio":
		if c.MimeType != "" {
			return "[" + c.Type + " " + c.MimeType + "]"
		}
		return "[" + c.Type + "]"
	}
	return c.Text
}

// ToolOutput is the result of tools/call.
type ToolOutput struct {
	// Content is the ordered content array.
	Content []ContentBlock `json:"content,omitempty"`
	// StructuredContent is the structured result (validated against outputSchema).
	StructuredContent json.RawMessage `json:"structuredContent,omitempty"`
	// IsError is the protocol-level error flag (content still holds the message).
	IsError bool `json:"isError,omitempty"`
	// ResultType is the 2026-07-28 completion marker: "complete" or "input_required".
	// Absent means complete (2025-11-25 style servers).
	ResultType string `json:"resultType,omitempty"`
	// InputRequests is the multi round-trip request list (2026-07-28).
	InputRequests []InputRequest `json:"inputRequests,omitempty"`
	// RequestState is opaque server state echoed back on the retry.
	RequestState json.RawMessage `json:"requestState,omitempty"`
}

// Complete reports whether the result is final. Absent resultType means complete (§tool
// results: "resultType is absent on responses from 2025-11-25 servers").
func (o *ToolOutput) Complete() bool {
	return o.ResultType == "" || o.ResultType == "complete"
}

// InputRequest is a server-initiated input request delivered inside a tool result
// (multi round-trip requests replace server-initiated requests, 2026-07-28).
type InputRequest struct {
	ID     string          `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
	// TaskID may scope an input request to a background task.
	TaskID string `json:"taskId,omitempty"`
}

// InputResponse answers one InputRequest.
type InputResponse struct {
	Behavior string          `json:"behavior"`
	Content  map[string]any  `json:"content,omitempty"`
	Action   string          `json:"action,omitempty"`
	Message  string          `json:"message,omitempty"`
	Result   json.RawMessage `json:"result,omitempty"`
}

// DiscoverResult is the result of server/discover (2026-07-28). It is the only version
// negotiation probe on the modern path — no initialize handshake exists.
type DiscoverResult struct {
	SupportedVersions []string                   `json:"supportedVersions,omitempty"`
	LatestVersion     string                     `json:"latestVersion,omitempty"`
	Versions          []VersionEntry             `json:"versions,omitempty"`
	Capabilities      map[string]json.RawMessage `json:"capabilities,omitempty"`
	Meta              map[string]json.RawMessage `json:"_meta,omitempty"`
	TTLMs             *int                       `json:"ttlMs,omitempty"`
	CacheScope        string                     `json:"cacheScope,omitempty"`
}

// VersionEntry is one entry of the versions array.
type VersionEntry struct {
	Version      string          `json:"version"`
	Available    *bool           `json:"available,omitempty"`
	DeprecatedAt string          `json:"deprecatedAt,omitempty"`
	Info         *Implementation `json:"info,omitempty"`
}

// ServerInfo pulls serverInfo out of _meta, matching the shape used by the spec examples.
func (d *DiscoverResult) ServerInfo() Implementation {
	if d == nil {
		return Implementation{}
	}
	raw, ok := d.Meta[metaServerInfo]
	if !ok {
		return Implementation{}
	}
	var out Implementation
	_ = json.Unmarshal(raw, &out)
	return out
}

// ServerCapabilities is the subset of the capability map this client reacts to.
type ServerCapabilities struct {
	Tools         map[string]any `json:"tools,omitempty"`
	Resources     map[string]any `json:"resources,omitempty"`
	Prompts       map[string]any `json:"prompts,omitempty"`
	Elicitation   map[string]any `json:"elicitation,omitempty"`
	Logging       map[string]any `json:"logging,omitempty"`
	Subscriptions map[string]any `json:"subscriptions,omitempty"`
}

// Has reports whether the server advertised a capability.
func (c ServerCapabilities) Has(name string) bool {
	switch name {
	case "tools":
		return c.Tools != nil
	case "resources":
		return c.Resources != nil
	case "prompts":
		return c.Prompts != nil
	case "elicitation":
		return c.Elicitation != nil
	case "logging":
		return c.Logging != nil
	case "subscriptions":
		return c.Subscriptions != nil
	}
	return false
}

// InitializeResult is the legacy (2025-11-25) handshake result.
type InitializeResult struct {
	ProtocolVersion string                     `json:"protocolVersion"`
	Capabilities    map[string]json.RawMessage `json:"capabilities,omitempty"`
	ServerInfo      Implementation             `json:"serverInfo,omitempty"`
	Instructions    string                     `json:"instructions,omitempty"`
}

// ToolsListResult is the tools/list result (both revisions).
type ToolsListResult struct {
	Tools      []Tool         `json:"tools,omitempty"`
	NextCursor string         `json:"nextCursor,omitempty"`
	Meta       map[string]any `json:"_meta,omitempty"`
}

// ---- header mirroring (2026-07-28 transport) ----

// HeaderDecl is one property of an input schema declaring `x-mcp-header`.
type HeaderDecl struct {
	ParamName string
	Required  bool
	Ptr       string
}

// DeclaredHeaders walks inputSchema.properties for x-mcp-header annotations.
func DeclaredHeaders(schema json.RawMessage) []HeaderDecl {
	if len(schema) == 0 {
		return nil
	}
	var s struct {
		Properties map[string]struct {
			XMCPHeader string `json:"x-mcp-header"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(schema, &s); err != nil {
		return nil
	}
	req := map[string]bool{}
	for _, r := range s.Required {
		req[r] = true
	}
	var out []HeaderDecl
	for name, p := range s.Properties {
		if strings.TrimSpace(p.XMCPHeader) == "" {
			continue
		}
		out = append(out, HeaderDecl{ParamName: p.XMCPHeader, Required: req[name], Ptr: "/" + escapePtrToken(name)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ParamName < out[j].ParamName })
	return out
}

// HeaderValue encodes a parameter value for an HTTP header. A conforming server
// base64-round-trips the sentinel form, so non-ASCII values survive losslessly.
func HeaderValue(v string) string {
	if isHeaderSafe(v) {
		return v
	}
	return "=?base64?" + base64.StdEncoding.EncodeToString([]byte(v)) + "?="
}

func isHeaderSafe(s string) bool {
	for _, r := range s {
		if r > unicode.MaxASCII || unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// escapePtrToken escapes one JSON pointer reference token (RFC 6901).
func escapePtrToken(s string) string {
	s = strings.ReplaceAll(s, "~", "~0")
	return strings.ReplaceAll(s, "/", "~1")
}

// Ptr resolves a JSON pointer like "/path" or "/a~1b/c" against a decoded value.
func Ptr(v any, ptr string) (any, bool) {
	if ptr == "" {
		return v, true
	}
	parts := strings.Split(strings.TrimPrefix(ptr, "/"), "/")
	cur := v
	for _, p := range parts {
		p = strings.ReplaceAll(p, "~1", "/")
		p = strings.ReplaceAll(p, "~0", "~")
		switch t := cur.(type) {
		case map[string]any:
			x, ok := t[p]
			if !ok {
				return nil, false
			}
			cur = x
		case []any:
			i, err := strconv.Atoi(p)
			if err != nil || i < 0 || i >= len(t) {
				return nil, false
			}
			cur = t[i]
		default:
			return nil, false
		}
	}
	return cur, true
}
