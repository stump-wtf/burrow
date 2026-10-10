// Package oidc is burrow's client for the configured OIDC provider
// (authorization code flow with PKCE, Pocket ID first, any issuer by config).
//
// Governing: ADR-0001 (OIDC sign-in). The Client interface is deliberately
// narrow so httpapi tests can drive the full sign-in flow against a fake
// issuer without this package.
package oidc

import (
	"context"
	"fmt"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// Claims is what burrow needs from an ID token.
type Claims struct {
	Subject string
	Email   string
	Name    string
}

// Client performs the authorization-code-with-PKCE dance against the issuer.
type Client struct {
	oauth    oauth2.Config
	verifier *oidc.IDTokenVerifier
}

// New discovers the issuer and builds the client. The issuer must be
// reachable at startup.
func New(ctx context.Context, issuerURL, clientID, clientSecret, redirectURL string) (*Client, error) {
	provider, err := oidc.NewProvider(ctx, issuerURL)
	if err != nil {
		return nil, fmt.Errorf("oidc: discover issuer %s: %w", issuerURL, err)
	}
	endpoint := provider.Endpoint()
	// A public PKCE client carries no secret: pin the in-params auth
	// style so the first token request is the RFC 7636 public-client
	// form (client_id in the body, no Authorization header). Left on
	// auto-detect, x/oauth2 probes header style first and sends a
	// username-only Basic header (base64(clientID:)) — one failed
	// round trip per process before the fallback wins.
	if clientSecret == "" {
		endpoint.AuthStyle = oauth2.AuthStyleInParams
	}
	return &Client{
		oauth: oauth2.Config{
			ClientID:     clientID,
			ClientSecret: clientSecret,
			Endpoint:     endpoint,
			RedirectURL:  redirectURL,
			Scopes:       []string{oidc.ScopeOpenID, "email", "profile"},
		},
		verifier: provider.Verifier(&oidc.Config{ClientID: clientID}),
	}, nil
}

// AuthCodeURL builds the authorization redirect for one flow, binding the
// S256 code challenge derived from verifier.
func (c *Client) AuthCodeURL(state, codeVerifier string) string {
	return c.oauth.AuthCodeURL(state, oauth2.S256ChallengeOption(codeVerifier))
}

// Exchange trades the authorization code (plus PKCE verifier) for tokens and
// returns the raw ID token.
func (c *Client) Exchange(ctx context.Context, code, codeVerifier string) (string, error) {
	tok, err := c.oauth.Exchange(ctx, code, oauth2.SetAuthURLParam("code_verifier", codeVerifier))
	if err != nil {
		return "", fmt.Errorf("oidc: exchange: %w", err)
	}
	rawIDToken, ok := tok.Extra("id_token").(string)
	if !ok || rawIDToken == "" {
		return "", fmt.Errorf("oidc: token response carried no id_token")
	}
	return rawIDToken, nil
}

// Verify checks the ID token's signature and audience and extracts claims.
func (c *Client) Verify(ctx context.Context, rawIDToken string) (Claims, error) {
	idToken, err := c.verifier.Verify(ctx, rawIDToken)
	if err != nil {
		return Claims{}, fmt.Errorf("oidc: verify id token: %w", err)
	}
	var payload struct {
		Sub               string `json:"sub"`
		Email             string `json:"email"`
		Name              string `json:"name"`
		PreferredUsername string `json:"preferred_username"`
	}
	if err := idToken.Claims(&payload); err != nil {
		return Claims{}, fmt.Errorf("oidc: parse claims: %w", err)
	}
	name := payload.Name
	if name == "" {
		name = payload.PreferredUsername
	}
	return Claims{Subject: payload.Sub, Email: payload.Email, Name: name}, nil
}
