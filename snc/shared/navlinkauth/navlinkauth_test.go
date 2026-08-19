// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

package navlinkauth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// newTestClient builds a Client pointed at a test server instead of the
// real navlink.net, by overriding http.Client.Transport to rewrite the
// scheme+host of every request to the test server's.
func newTestClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	c := New()
	base := srv.URL
	c.http.Transport = rewriteTransport{base: base}
	return c
}

type rewriteTransport struct{ base string }

func (rt rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	u, err := http.NewRequest(req.Method, rt.base+req.URL.Path, req.Body)
	if err != nil {
		return nil, err
	}
	u.Header = req.Header
	return http.DefaultTransport.RoundTrip(u)
}

func TestLoginSuccessThenFreeKey(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/account/login", func(w http.ResponseWriter, r *http.Request) {
		// Path must be "/" — the real navlink.net endpoint sets it explicitly
		// (setSessionCookie in snc-arbiter/get_key.go) precisely so the cookie
		// applies to /api/key/free too, not just /api/account/*.
		http.SetCookie(w, &http.Cookie{Name: "snc_session", Value: "abc123", Path: "/"})
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	})
	mux.HandleFunc("/api/key/free", func(w http.ResponseWriter, r *http.Request) {
		if _, err := r.Cookie("snc_session"); err != nil {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"not logged in"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"key": "OPAQUE_KEY_STRING", "key_id": "k1", "client_id": "c1"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTestClient(t, srv)
	ctx := context.Background()

	if err := c.Login(ctx, "user@example.com", "hunter22"); err != nil {
		t.Fatalf("Login: %v", err)
	}
	key, keyID, clientID, err := c.FreeKey(ctx)
	if err != nil {
		t.Fatalf("FreeKey: %v", err)
	}
	if key != "OPAQUE_KEY_STRING" || keyID != "k1" || clientID != "c1" {
		t.Fatalf("unexpected FreeKey result: %q %q %q", key, keyID, clientID)
	}
}

func TestLoginInvalidCredentials(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/account/login", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"Invalid email or password"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTestClient(t, srv)
	err := c.Login(context.Background(), "user@example.com", "wrong")
	if err == nil {
		t.Fatal("expected error")
	}
	lerr, ok := err.(*LoginError)
	if !ok {
		t.Fatalf("expected *LoginError, got %T: %v", err, err)
	}
	if lerr.StatusCode != http.StatusUnauthorized || lerr.Message != "Invalid email or password" {
		t.Fatalf("unexpected LoginError: %+v", lerr)
	}
}

func TestLoginUnconfirmedAccount(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/account/login", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"error":"Please confirm your email address first"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTestClient(t, srv)
	err := c.Login(context.Background(), "user@example.com", "correct")
	lerr, ok := err.(*LoginError)
	if !ok {
		t.Fatalf("expected *LoginError, got %T: %v", err, err)
	}
	if lerr.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", lerr.StatusCode)
	}
}

func TestProbeUnreachable(t *testing.T) {
	c := New()
	// Point at a port nothing listens on to simulate unreachable.
	c.http.Transport = rewriteTransport{base: "http://127.0.0.1:1"}
	if c.Probe(context.Background()) {
		t.Fatal("expected Probe to report unreachable")
	}
}

func TestProbeReachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound) // any status counts as reachable
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	if !c.Probe(context.Background()) {
		t.Fatal("expected Probe to report reachable")
	}
}
