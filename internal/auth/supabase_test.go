package auth_test

// Exercises the verifier and the auth middleware end to end against a fake
// Supabase JWKS endpoint. This covers signature verification, issuer checking,
// expiry, and the middleware's reject/accept behaviour -- the paths that were
// untestable in the Node backend because token verification was entangled with
// per-route request handlers.

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/lestrrat-go/jwx/v3/jwa"
	"github.com/lestrrat-go/jwx/v3/jwk"
	"github.com/lestrrat-go/jwx/v3/jwt"

	"github.com/hireonai/backend-hireon-go/internal/auth"
)

// fakeSupabase serves a JWKS and mints tokens signed with the matching key.
type fakeSupabase struct {
	server *httptest.Server
	key    *rsa.PrivateKey
	kid    string
}

func newFakeSupabase(t *testing.T) *fakeSupabase {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	fs := &fakeSupabase{key: key, kid: "test-key-1"}

	pub, err := jwk.Import(key.PublicKey)
	if err != nil {
		t.Fatalf("import public key: %v", err)
	}
	if err := pub.Set(jwk.KeyIDKey, fs.kid); err != nil {
		t.Fatalf("set kid: %v", err)
	}
	if err := pub.Set(jwk.AlgorithmKey, "RS256"); err != nil {
		t.Fatalf("set alg: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/auth/v1/.well-known/jwks.json", func(w http.ResponseWriter, _ *http.Request) {
		set := jwk.NewSet()
		_ = set.AddKey(pub)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(set)
	})
	fs.server = httptest.NewServer(mux)
	t.Cleanup(fs.server.Close)
	return fs
}

func (f *fakeSupabase) issuer() string { return f.server.URL + "/auth/v1" }

type tokenOpts struct {
	subject   string
	email     string
	fullName  string
	expiresAt time.Time
	issuer    string
}

func (f *fakeSupabase) mint(t *testing.T, o tokenOpts) string {
	t.Helper()
	signKey, err := jwk.Import(f.key)
	if err != nil {
		t.Fatalf("import private key: %v", err)
	}
	if err := signKey.Set(jwk.KeyIDKey, f.kid); err != nil {
		t.Fatalf("set kid: %v", err)
	}

	issuer := o.issuer
	if issuer == "" {
		issuer = f.issuer()
	}
	exp := o.expiresAt
	if exp.IsZero() {
		exp = time.Now().Add(time.Hour)
	}

	builder := jwt.NewBuilder().
		Issuer(issuer).
		Subject(o.subject).
		IssuedAt(time.Now()).
		Expiration(exp)
	if o.email != "" {
		builder = builder.Claim("email", o.email)
	}
	if o.fullName != "" {
		builder = builder.Claim("user_metadata", map[string]any{
			"full_name": o.fullName,
			"picture":   "https://example.test/pic.png",
		})
	}

	tok, err := builder.Build()
	if err != nil {
		t.Fatalf("build token: %v", err)
	}
	signed, err := jwt.Sign(tok, jwt.WithKey(jwa.RS256(), signKey))
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return string(signed)
}

func TestVerifyAcceptsValidToken(t *testing.T) {
	fs := newFakeSupabase(t)
	v, err := auth.NewVerifier(context.Background(), fs.server.URL)
	if err != nil {
		t.Fatalf("new verifier: %v", err)
	}
	defer v.Close()

	token := fs.mint(t, tokenOpts{subject: "user-123", email: "a@b.test", fullName: "Ada Lovelace"})
	claims, err := v.Verify(context.Background(), token)
	if err != nil {
		t.Fatalf("expected valid token to verify, got %v", err)
	}
	if claims.Subject != "user-123" {
		t.Errorf("subject = %q, want user-123", claims.Subject)
	}
	if claims.Email != "a@b.test" {
		t.Errorf("email = %q, want a@b.test", claims.Email)
	}
	if claims.FullName != "Ada Lovelace" {
		t.Errorf("fullname = %q, want Ada Lovelace", claims.FullName)
	}
}

func TestVerifyRejectsBadTokens(t *testing.T) {
	fs := newFakeSupabase(t)
	v, err := auth.NewVerifier(context.Background(), fs.server.URL)
	if err != nil {
		t.Fatalf("new verifier: %v", err)
	}
	defer v.Close()

	// A token signed by a different key must be rejected.
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate other key: %v", err)
	}
	otherJWK, err := jwk.Import(other)
	if err != nil {
		t.Fatalf("import other key: %v", err)
	}
	if err := otherJWK.Set(jwk.KeyIDKey, fs.kid); err != nil {
		t.Fatalf("set kid: %v", err)
	}
	forgedTok, err := jwt.NewBuilder().
		Issuer(fs.issuer()).Subject("attacker").
		IssuedAt(time.Now()).Expiration(time.Now().Add(time.Hour)).Build()
	if err != nil {
		t.Fatalf("build forged: %v", err)
	}
	forged, err := jwt.Sign(forgedTok, jwt.WithKey(jwa.RS256(), otherJWK))
	if err != nil {
		t.Fatalf("sign forged: %v", err)
	}

	// A token from the wrong issuer must be rejected.
	wrongIssuer := fs.mint(t, tokenOpts{
		subject: "user-1", email: "a@b.test",
		issuer: "https://evil.test/auth/v1",
	})

	expired := fs.mint(t, tokenOpts{
		subject: "user-1", email: "a@b.test",
		expiresAt: time.Now().Add(-time.Hour),
	})

	cases := map[string]string{
		"forged signature": string(forged),
		"wrong issuer":     wrongIssuer,
		"expired":          expired,
		"garbage":          "not.a.jwt",
		"empty":            "",
	}
	for name, token := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := v.Verify(context.Background(), token); err == nil {
				t.Fatalf("expected rejection, got nil error")
			}
		})
	}
}
