// The upstream side of a shared MCP connection (Stage C, stump.wtf/burrow#4).
//
// burrow is the MCP server the agent talks to, and an MCP client towards the
// org's upstream servers: a member's search_capabilities answer carries their
// tools, and execute_capability on a tool runs it upstream with that member's
// credential. That is what makes Stage C a proxy rather than a list of local
// client entries (docs/client-contract.md, "Stage C"), and it means burrow makes
// outbound requests with credentials on behalf of whoever asked.
//
// Two rules follow from that, and both are enforced here rather than at the
// call sites:
//
//   - The URL an upstream request goes to is checked against the host allowlist
//     on every call, not only when the row is written. A connection row is data
//     an operator edits, and a row that names an arbitrary host would otherwise
//     send a stored key to it (the same reasoning ADR-0005 applies to a provider
//     row's baseURL).
//   - Nothing here retries, follows a redirect, or logs a credential. Redirects
//     are refused outright rather than followed across hosts: Go re-sends a
//     307/308 body untouched, and for the token endpoint that body is the code,
//     the verifier, or a refresh token, which must never reach a second host.
//     Authorize values are never logged; a refusal names the host and the
//     outcome.
//
// Governing: ADR-0001 (clean room), ADR-0005 (what a row may name), ADR-0010.
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ProtocolVersion is the newest MCP revision burrow speaks upstream — the same
// one it answers the desktop client with (docs/client-contract.md, "The MCP
// endpoint").
const ProtocolVersion = "2025-06-18"

// ClientName is what burrow calls itself when an upstream server asks.
const ClientName = "burrow"

// ErrUnauthorized marks an upstream server that refused the credential burrow
// presented. The caller answers needs_auth for that connection: the credential is
// wrong, expired, or revoked, and re-minting a burrow token cannot fix it.
var ErrUnauthorized = errors.New("upstream: refused by the MCP server")

// ErrHostNotAllowed marks a connection whose URL the host allowlist refuses.
var ErrHostNotAllowed = errors.New("upstream: host is outside BURROW_MCP_URL_HOSTS")

// JSONRPCError is an error the upstream server answered with, as opposed to a
// transport failure: the server was reached and said no. Its message is the
// server's own, which is what makes "that tool refused those arguments"
// readable to whoever asked.
type JSONRPCError struct {
	Code    int
	Message string
}

func (e *JSONRPCError) Error() string {
	return fmt.Sprintf("upstream: jsonrpc %d: %s", e.Code, e.Message)
}

// Tool is one tool an upstream server offers.
type Tool struct {
	Name        string
	Title       string
	Description string
	// InputSchema is the server's own JSON Schema, passed through untouched so a
	// caller can tell the agent what the tool takes.
	InputSchema json.RawMessage
}

// Policy is what a connection's URL must satisfy before burrow talks to it.
// Its zero value enforces nothing, which is what a test with no deployment
// behind it wants; production builds one from the config.
type Policy struct {
	enforce bool
	hosts   []string
}

// NewPolicy builds the enforcing policy from BURROW_MCP_URL_HOSTs. An empty list
// allows no host at all: the fail-closed default that keeps a written row from
// turning burrow into an arbitrary HTTP proxy.
func NewPolicy(hosts []string) Policy {
	p := Policy{enforce: true}
	for _, host := range hosts {
		if h := normalizeHost(host); h != "" {
			p.hosts = append(p.hosts, h)
		}
	}
	return p
}

// Unconstrained is the policy that checks nothing; production never uses it.
func Unconstrained() Policy { return Policy{} }

// CheckURL reports whether burrow may send requests to raw, and returns the
// cleaned URL to use. Only http(s) with a host is accepted, and credentials in
// the URL are refused outright: they would be sent as basic auth to whatever
// host the row names.
func (p Policy) CheckURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("upstream: an mcp connection needs a url")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("upstream: parse mcp connection url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("upstream: mcp connection url must be http(s), got %q", u.Scheme)
	}
	if u.User != nil {
		return "", errors.New("upstream: an mcp connection url may not carry credentials")
	}
	host := normalizeHost(u.Host)
	if host == "" {
		return "", errors.New("upstream: mcp connection url has no host")
	}
	if p.enforce {
		allowed := false
		for _, want := range p.hosts {
			if host == want {
				allowed = true
				break
			}
		}
		if !allowed {
			return "", fmt.Errorf("%w: %s", ErrHostNotAllowed, host)
		}
	}
	return u.Scheme + "://" + host + u.RequestURI(), nil
}

func normalizeHost(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	return strings.TrimSuffix(host, ".")
}

// NewHTTPClient builds the transport every burrow-to-upstream call uses: one
// timeout, and no redirect following. A 3xx is returned to the caller as the
// response it is, so a credential-bearing request is never replayed to a second
// host (Go re-sends a 307/308 body as-is, and for the token endpoint that body
// carries the code, the verifier, or a refresh token).
func NewHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// Client talks Streamable HTTP to one upstream server. It is safe to share: it
// holds only an *http.Client and the policy every URL passes.
type Client struct {
	http   *http.Client
	policy Policy
	// Version goes into clientInfo so an upstream log can name the burrow build.
	Version string
}

// NewClient builds a client over an *http.Client whose timeout bounds every
// upstream call, with the policy every connection URL is checked against.
func NewClient(httpClient *http.Client, policy Policy, version string) *Client {
	if httpClient == nil {
		httpClient = NewHTTPClient(30 * time.Second)
	}
	return &Client{http: httpClient, policy: policy, Version: version}
}

// HTTP is the transport this client uses, so the OAuth calls that belong to a
// connection go over the same one and inherit its timeout.
func (c *Client) HTTP() *http.Client { return c.http }

// request is one JSON-RPC call. id is nil for a notification.
type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  any             `json:"params,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// Headers carries the credential burrow presents upstream. It is built by the
// caller, which is the only place that knows whether this is a shared key, a
// member's own key, or an OAuth access token.
type Headers map[string]string

// ListTools initializes against raw and returns the server's tools.
func (c *Client) ListTools(ctx context.Context, rawURL string, headers Headers) ([]Tool, error) {
	result, err := c.call(ctx, rawURL, headers, "tools/list", map[string]any{})
	if err != nil {
		return nil, err
	}
	var payload struct {
		Tools []struct {
			Name         string          `json:"name"`
			Title        string          `json:"title"`
			Description  string          `json:"description"`
			InputSchema  json.RawMessage `json:"inputSchema"`
			InputSchema2 json.RawMessage `json:"input_schema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(result, &payload); err != nil {
		return nil, fmt.Errorf("upstream: decode tools/list: %w", err)
	}
	tools := make([]Tool, 0, len(payload.Tools))
	for _, t := range payload.Tools {
		if strings.TrimSpace(t.Name) == "" {
			continue
		}
		schema := t.InputSchema
		if len(schema) == 0 {
			schema = t.InputSchema2
		}
		tools = append(tools, Tool{Name: t.Name, Title: t.Title, Description: t.Description, InputSchema: schema})
	}
	return tools, nil
}

// CallTool runs one upstream tool and returns its result payload verbatim, so
// the caller can put its content in front of the agent unchanged.
func (c *Client) CallTool(ctx context.Context, rawURL string, headers Headers, name string, args map[string]any) (json.RawMessage, error) {
	if strings.TrimSpace(name) == "" {
		return nil, errors.New("upstream: a tool call needs a name")
	}
	if args == nil {
		args = map[string]any{}
	}
	return c.call(ctx, rawURL, headers, "tools/call", map[string]any{"name": name, "arguments": args})
}

// call does the initialize handshake and then one request, in one session. An
// MCP server is allowed to require the handshake before it answers anything
// else, and holding the session id for the pair is one header on the second
// request.
func (c *Client) call(ctx context.Context, rawURL string, headers Headers, method string, params any) (json.RawMessage, error) {
	endpoint, err := c.check(rawURL)
	if err != nil {
		return nil, err
	}
	initResult, sessionID, err := c.post(ctx, endpoint, headers, "", request{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`1`),
		Method:  "initialize",
		Params: map[string]any{
			"protocolVersion": ProtocolVersion,
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": ClientName, "version": c.Version},
		},
	})
	if err != nil {
		return nil, err
	}
	if len(initResult) == 0 {
		return nil, errors.New("upstream: initialize returned no result")
	}
	// The initialized notification is required by the transport before the
	// server will answer a request; it is a notification, so a 202 with an
	// empty body is the success case.
	if _, _, err := c.post(ctx, endpoint, headers, sessionID, request{
		JSONRPC: "2.0",
		Method:  "notifications/initialized",
	}); err != nil {
		return nil, err
	}
	result, _, err := c.post(ctx, endpoint, headers, sessionID, request{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`2`),
		Method:  method,
		Params:  params,
	})
	return result, err
}

// check is where the host allowlist is enforced. A Client without a policy was
// built for a caller that policed the URL itself (a test); production always
// sets one.
func (c *Client) check(rawURL string) (string, error) {
	if c.policy.enforce {
		return c.policy.CheckURL(rawURL)
	}
	return rawURL, nil
}

// post sends one message and decodes the answer, which arrives either as JSON
// or as one SSE message (docs/client-contract.md describes both for the client's
// side, and an upstream server picks whichever it likes). It returns the result
// payload, and any Mcp-Session-Id the server handed out.
func (c *Client) post(ctx context.Context, endpoint string, headers Headers, sessionID string, msg request) (json.RawMessage, string, error) {
	body, err := json.Marshal(msg)
	if err != nil {
		return nil, "", fmt.Errorf("upstream: encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, "", fmt.Errorf("upstream: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if sessionID != "" {
		req.Header.Set("Mcp-Session-Id", sessionID)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("upstream: request failed: %w", err)
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, "", fmt.Errorf("upstream: read response: %w", err)
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, "", fmt.Errorf("%w: http %d", ErrUnauthorized, resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", fmt.Errorf("upstream: http %d", resp.StatusCode)
	}
	if len(msg.ID) == 0 || len(bytes.TrimSpace(payload)) == 0 {
		return nil, resp.Header.Get("Mcp-Session-Id"), nil
	}
	decoded, err := decodeMessage(resp.Header.Get("Content-Type"), payload, msg.ID)
	if err != nil {
		return nil, "", err
	}
	if decoded.Error != nil {
		return nil, "", &JSONRPCError{Code: decoded.Error.Code, Message: decoded.Error.Message}
	}
	return decoded.Result, resp.Header.Get("Mcp-Session-Id"), nil
}

// decodeMessage pulls the JSON-RPC answer out of a JSON body or an SSE stream.
func decodeMessage(contentType string, payload, id json.RawMessage) (response, error) {
	if strings.Contains(strings.ToLower(contentType), "text/event-stream") {
		for _, line := range strings.Split(string(payload), "\n") {
			line = strings.TrimRight(strings.TrimSpace(line), "\r")
			data, ok := strings.CutPrefix(line, "data:")
			if !ok {
				continue
			}
			data = strings.TrimSpace(data)
			if data == "" || !json.Valid([]byte(data)) {
				continue
			}
			var msg response
			if err := json.Unmarshal([]byte(data), &msg); err != nil {
				continue
			}
			if len(msg.ID) == 0 || string(msg.ID) == string(id) {
				return msg, nil
			}
		}
		return response{}, errors.New("upstream: no matching message in the event stream")
	}
	var msg response
	if err := json.Unmarshal(payload, &msg); err != nil {
		return response{}, fmt.Errorf("upstream: decode response: %w", err)
	}
	return msg, nil
}
