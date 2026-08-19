// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

// Package navlinkauth implements the "no key yet" login path: a direct
// (non-tunneled) HTTPS client for navlink.net's existing account-login and
// free-key-issuance endpoints.
//
// This deliberately does NOT reuse tunnel_cat/snc/core's Authenticator —
// that type dials through the SNC tunnel protocol (custom TLS fingerprint,
// channel framing) and has no cookie support. A client with no key yet has
// no tunnel to route through anyway (every platform's startup code only
// constructs a tunnel dialer after a key is loaded/entered), so the only
// way to log in before having a key is a plain HTTPS call straight to
// navlink.net using the OS's normal network stack — which is exactly what
// this package does. Keep it that way: never wire this client through a
// tunnel dialer, even if one becomes available later in the process.
package navlinkauth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"time"
)

const baseURL = "https://navlink.net"

const (
	probeTimeout = 4 * time.Second
	callTimeout  = 10 * time.Second
)

// Client performs the login + free-key-issuance sequence against navlink.net.
// A single Client instance must be reused across Login and FreeKey so the
// session cookie set by Login is carried into FreeKey.
type Client struct {
	http *http.Client
}

// New creates a Client with its own cookie jar and default (non-tunneled)
// transport.
func New() *Client {
	jar, _ := cookiejar.New(nil)
	return &Client{http: &http.Client{Jar: jar}}
}

// LoginError is returned by Login/FreeKey for a well-formed HTTP error
// response from navlink.net (as opposed to a network/transport failure).
type LoginError struct {
	StatusCode int
	Message    string
}

func (e *LoginError) Error() string {
	return fmt.Sprintf("navlink: %s (status %d)", e.Message, e.StatusCode)
}

// Probe reports whether navlink.net is reachable directly, right now, from
// this device. Any HTTP response (regardless of status code) counts as
// reachable — only a network/TLS-level failure or timeout counts as
// unreachable. Used to decide whether to offer the credential-login path at
// all: in regions where navlink.net is blocked, login can never succeed, so
// the UI should skip straight to manual key entry instead.
func (c *Client) Probe(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/", nil)
	if err != nil {
		return false
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return true
}

// Login authenticates an existing navlink.net account. On success the
// session cookie is retained in the Client's cookie jar for a subsequent
// FreeKey call.
func (c *Client) Login(ctx context.Context, email, password string) error {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()

	body, _ := json.Marshal(map[string]string{"email": email, "password": password})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/account/login", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("navlink: login request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return &LoginError{StatusCode: resp.StatusCode, Message: readErrMessage(resp.Body)}
	}
	return nil
}

// FreeKey issues a fresh key for the account authenticated by the preceding
// Login call, using the same Client (and thus the same session cookie).
func (c *Client) FreeKey(ctx context.Context) (keyStr, keyID, clientID string, err error) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/key/free", nil)
	if err != nil {
		return "", "", "", err
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return "", "", "", fmt.Errorf("navlink: free-key request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", "", "", &LoginError{StatusCode: resp.StatusCode, Message: readErrMessage(resp.Body)}
	}

	var out struct {
		Key      string `json:"key"`
		KeyID    string `json:"key_id"`
		ClientID string `json:"client_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", "", "", fmt.Errorf("navlink: decode free-key response: %w", err)
	}
	return out.Key, out.KeyID, out.ClientID, nil
}

// readErrMessage best-effort extracts {"error":"..."} from an error
// response body, falling back to a generic message.
func readErrMessage(body io.Reader) string {
	data, err := io.ReadAll(io.LimitReader(body, 4096))
	if err != nil || len(data) == 0 {
		return "request failed"
	}
	var e struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(data, &e); err == nil && e.Error != "" {
		return e.Error
	}
	return strings.TrimSpace(string(data))
}
