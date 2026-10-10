// Package auth verifies Supabase-issued access tokens against the project JWKS.
//
// The key set is cached in memory and refreshed in the background, so key
// rotation does not require a restart and no request pays a JWKS fetch.
package auth

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/lestrrat-go/jwx/v3/jwk"
	"github.com/lestrrat-go/jwx/v3/jwt"

	"github.com/hireonai/backend-hireon-go/internal/domain"
)

type Verifier struct {
	issuer  string
	jwksURL string
	mu      sync.RWMutex
	set     jwk.Set
	stop    chan struct{}
}

// NewVerifier fetches the JWKS once and starts a background refresh loop.
//
// Fetching at startup means a misconfigured SUPABASE_URL fails fast instead of
// surfacing on the first authenticated request (the Node backend logged a
// warning and booted anyway, then failed every request).
func NewVerifier(ctx context.Context, supabaseURL string) (*Verifier, error) {
	issuer := supabaseURL + "/auth/v1"
	jwksURL := issuer + "/.well-known/jwks.json"

	fetchCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	set, err := jwk.Fetch(fetchCtx, jwksURL)
	if err != nil {
		return nil, fmt.Errorf("fetch jwks from %s: %w", jwksURL, err)
	}

	v := &Verifier{issuer: issuer, jwksURL: jwksURL, set: set, stop: make(chan struct{})}
	go v.refreshLoop()
	return v, nil
}

// refreshLoop re-fetches the key set periodically so rotated or newly added keys
// are picked up without a restart.
func (v *Verifier) refreshLoop() {
	ticker := time.NewTicker(30 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-v.stop:
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			set, err := jwk.Fetch(ctx, v.jwksURL)
			cancel()
			if err != nil {
				continue // keep the last good set; verification still works
			}
			v.mu.Lock()
			v.set = set
			v.mu.Unlock()
		}
	}
}

// Close stops the background refresh.
func (v *Verifier) Close() { close(v.stop) }

// Claims is the subset of the Supabase token payload the API needs.
type Claims struct {
	Subject  string
	Email    string
	Picture  string
	FullName string
	Provider string
}

// Verify validates the token signature, issuer and expiry, and returns claims.
// All failures collapse to domain.ErrUnauthorized so callers cannot distinguish
// (and therefore cannot leak) why a token was rejected.
func (v *Verifier) Verify(_ context.Context, token string) (*Claims, error) {
	v.mu.RLock()
	set := v.set
	v.mu.RUnlock()

	parsed, err := jwt.Parse([]byte(token),
		jwt.WithKeySet(set),
		jwt.WithValidate(true),
		jwt.WithIssuer(v.issuer),
	)
	if err != nil {
		return nil, domain.ErrUnauthorized
	}

	sub, ok := parsed.Subject()
	if !ok || sub == "" {
		return nil, domain.ErrUnauthorized
	}

	c := &Claims{Subject: sub}
	var s string
	if err := parsed.Get("email", &s); err == nil {
		c.Email = s
	}
	var meta map[string]any
	if err := parsed.Get("user_metadata", &meta); err == nil {
		if c.Email == "" {
			if v, ok := meta["email"].(string); ok {
				c.Email = v
			}
		}
		if v, ok := meta["picture"].(string); ok {
			c.Picture = v
		}
		for _, key := range []string{"full_name", "fullName", "name"} {
			if v, ok := meta[key].(string); ok && v != "" {
				c.FullName = v
				break
			}
		}
	}
	var appMeta map[string]any
	if err := parsed.Get("app_metadata", &appMeta); err == nil {
		if v, ok := appMeta["provider"].(string); ok {
			c.Provider = v
		}
	}
	if c.Email == "" && c.FullName == "" {
		return nil, errors.New("token carries neither email nor name")
	}
	return c, nil
}
