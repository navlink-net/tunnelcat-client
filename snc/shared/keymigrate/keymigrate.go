// Package keymigrate silently upgrades a legacy (V1, unsigned) activation
// key to a fresh V2 arbiter-signed one.
//
// V1 keys have no ArbiterPubkey/Sig, so nothing about their embedded
// ControlNodes/Servers list is verifiable — core.ParseKeyString still
// accepts them (transition period; see core.KeyData's M1 doc comment), but
// a caller MUST NOT connect to a V1 key's own server list without first
// migrating it here. Any address in that list came from the key itself,
// which anyone can hand-craft: the wire-format encryption key is a fixed
// constant published in this repository, and a self-encrypted V1 key needs
// no arbiter involvement at all. If migration fails, the key must be
// treated as unauthenticated and rejected — falling back to dialing the V1
// key's own Servers/ControlNodes would defeat the point of this package.
package keymigrate

import (
	"context"
	"fmt"

	"shortnerdcat/snc/shared/navlinkauth"

	core "tunnel_cat/snc/core"
)

// Migrate logs in to navlink.net with the Username/Password embedded in a
// legacy key and requests a fresh V2 signed key for that account.
//
// This calls navlinkauth against navlink.net specifically — a fixed,
// trusted address — never anything derived from kd (kd.Nodes() is exactly
// what we don't yet trust). A real V1 key's Username/Password is a real
// navlink.net account and this succeeds; a hand-crafted key has no such
// account behind it and this fails, which is what makes migration safe to
// attempt unconditionally rather than only after some other check.
func Migrate(ctx context.Context, kd *core.KeyData) (keyStr string, newKD *core.KeyData, err error) {
	if kd.Username == "" || kd.Password == "" {
		return "", nil, fmt.Errorf("keymigrate: legacy key has no stored credentials to migrate with")
	}

	nc := navlinkauth.New()
	if err := nc.Login(ctx, kd.Username, kd.Password); err != nil {
		return "", nil, fmt.Errorf("keymigrate: login: %w", err)
	}
	ks, _, _, err := nc.FreeKey(ctx)
	if err != nil {
		return "", nil, fmt.Errorf("keymigrate: free key: %w", err)
	}
	parsed, err := core.ParseKeyString(ks)
	if err != nil {
		// Should be unreachable — navlink.net always issues valid V2 keys
		// (see snc-arbiter/admin_keygen.go's issueKey) — but if it ever
		// isn't, treat it the same as any other migration failure.
		return "", nil, fmt.Errorf("keymigrate: parse issued key: %w", err)
	}
	return ks, parsed, nil
}
