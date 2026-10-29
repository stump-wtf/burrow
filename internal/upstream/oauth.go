// The OAuth dance behind a shared MCP connection (Stage C, stump.wtf/burrow#4).
//
// The client's side of this is two moves and nothing else: it opens the
// authorizeUrl that GET /v1/mcp-connections/:id/connect/start hands it, and then
// polls the list (docs/client-contract.md, "Stage C"). So burrow owns the rest:
// it starts the dance, the browser lands on burrow's callback with a single-use
// state and nothing else, burrow exchanges the code, and the next poll is what
// tells the client it worked.
//
// burrow is a public PKCE client towards an upstream server too. It carries no
// client secret for any connection, which is what lets a connection be written
// without anyone pasting a credential into a row, and it is the same shape its
// own sign-in uses.
//
// Governing: ADR-0010, ADR-0005 (the token endpoint is a host burrow sends a
// credential to, so it passes the same allowlist as the MCP URL).
package upstream

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Token is what an upstream authorization server answers with.
type Token struct {
	AccessToken  string
	RefreshToken string
	ExpiresIn    time.Duration // zero means the server said nothing about expiry
}

// NeedsRefresh reports whether an access token taken at obtainedAt has come due,
// minus a skew so burrow refreshes just before an upstream server would refuse
// it. A token whose server said nothing about expiry is never refreshed on the
// clock alone.
func NeedsRefresh(obtainedAt, now time.Time, expiresIn, skew time.Duration) bool {
	if expiresIn == 0 {
		return false
	}
	return !now.Before(obtainedAt.Add(expiresIn - skew))
}

// PKCEVerifier is the random value behind the S256 challenge. It is stored with
// the connect's state and never put in a URL.
func PKCEVerifier() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("upstream: pkce verifier: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// AuthorizeURL builds the URL the client opens. redirectURI is burrow's own
// callback for this connection, so the browser that finishes the dance comes
// back to burrow rather than to anything the row names.
func AuthorizeURL(policy Policy, authURL, clientID, redirectURI, state, verifier, scopes string) (string, error) {
	endpoint, err := policy.CheckURL(authURL)
	if err != nil {
		return "", fmt.Errorf("upstream: authorize url: %w", err)
	}
	if strings.TrimSpace(clientID) == "" {
		return "", errors.New("upstream: an oauth connection needs a client id")
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("upstream: parse authorize url: %w", err)
	}
	q := parsed.Query()
	q.Set("response_type", "code")
	q.Set("client_id", clientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("state", state)
	q.Set("code_challenge", pkceChallenge(verifier))
	q.Set("code_challenge_method", "S256")
	if s := strings.TrimSpace(scopes); s != "" {
		q.Set("scope", s)
	}
	parsed.RawQuery = q.Encode()
	return parsed.String(), nil
}

// ExchangeCode turns the code a callback carried into a token. The verifier is
// what proves the code came from the connect burrow started, so a code someone
// else intercepted cannot be spent here.
func ExchangeCode(ctx context.Context, client *http.Client, policy Policy, tokenURL, code, verifier, clientID, redirectURI string) (Token, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"code_verifier": {verifier},
		"client_id":     {clientID},
		"redirect_uri":  {redirectURI},
	}
	return postToken(ctx, client, policy, tokenURL, form)
}

// Refresh swaps a refresh token for a fresh access token. A server that answers
// a new refresh token too gets it kept: rotating refresh tokens are common, and
// dropping the successor would strand the connection the next time around.
func Refresh(ctx context.Context, client *http.Client, policy Policy, tokenURL, refreshToken, clientID string) (Token, error) {
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {clientID},
	}
	return postToken(ctx, client, policy, tokenURL, form)
}

// postToken calls the token endpoint as a public client: form-encoded, no
// authorization header, no client_assertion. An error answer is refused with its
// OAuth code, which is what makes "the user denied it" readable in a log.
func postToken(ctx context.Context, client *http.Client, policy Policy, tokenURL string, form url.Values) (Token, error) {
	endpoint, err := policy.CheckURL(tokenURL)
	if err != nil {
		return Token{}, fmt.Errorf("upstream: token url: %w", err)
	}
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return Token{}, fmt.Errorf("upstream: build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return Token{}, fmt.Errorf("upstream: token request failed: %w", err)
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Token{}, fmt.Errorf("upstream: read token response: %w", err)
	}
	var answered struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    any    `json:"expires_in"`
		Error        string `json:"error"`
	}
	if err := json.Unmarshal(payload, &answered); err != nil {
		return Token{}, fmt.Errorf("upstream: decode token response: %w", err)
	}
	if answered.AccessToken == "" {
		if answered.Error != "" {
			return Token{}, fmt.Errorf("upstream: token endpoint refused: %s", answered.Error)
		}
		return Token{}, fmt.Errorf("upstream: token endpoint returned no access token (http %d)", resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Token{}, fmt.Errorf("upstream: token endpoint answered http %d", resp.StatusCode)
	}
	token := Token{AccessToken: answered.AccessToken, RefreshToken: answered.RefreshToken}
	if seconds := tokenExpiry(answered.ExpiresIn); seconds > 0 {
		token.ExpiresIn = time.Duration(seconds) * time.Second
	}
	return token, nil
}

// tokenExpiry reads expires_in, which servers send as a number and some send as
// a string.
func tokenExpiry(raw any) int64 {
	switch v := raw.(type) {
	case float64:
		return int64(v)
	case string:
		n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err == nil {
			return n
		}
	}
	return 0
}
