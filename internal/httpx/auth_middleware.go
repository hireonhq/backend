package httpx

import (
	"context"
	"log/slog"
	"net/http"
	"strings"

	"github.com/hireonai/backend-hireon-go/internal/auth"
	"github.com/hireonai/backend-hireon-go/internal/domain"
)

// UserStore is the slice of the repository the auth middleware needs.
type UserStore interface {
	UpsertUserFromSupabase(ctx context.Context, sub, email, fullname, picture, provider string) (*domain.User, error)
}

// UserFrom returns the authenticated user stored by RequireAuth.
func UserFrom(ctx context.Context) *domain.User {
	if u, ok := ctx.Value(userKey).(*domain.User); ok {
		return u
	}
	return nil
}

// WithUser stores the authenticated user in the context.
func WithUser(ctx context.Context, u *domain.User) context.Context {
	return context.WithValue(ctx, userKey, u)
}

// bearerToken extracts the token from an Authorization header.
func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return "", false
	}
	token := strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
	return token, token != ""
}

// RequireAuth rejects requests without a valid token.
//
// The Node backend guarded every protected route with a per-route `pre` hook and
// declared the Authorization header as required at the Joi layer. That ordering
// meant a wholly absent header produced a 400 "Invalid request headers input"
// while a malformed token produced 401 -- an inconsistency clients had to learn.
// This middleware always answers 401 for an absent or invalid token.
func RequireAuth(v *auth.Verifier, users UserStore) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			user, err := authenticate(r, v, users)
			if err != nil {
				Fail(w, r, err)
				return
			}
			next.ServeHTTP(w, r.WithContext(WithUser(r.Context(), user)))
		})
	}
}

// OptionalAuth attaches the user when a valid token is present, and proceeds
// anonymously otherwise -- used by GET /jobs.
func OptionalAuth(v *auth.Verifier, users UserStore) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, ok := bearerToken(r); !ok {
				next.ServeHTTP(w, r)
				return
			}
			user, err := authenticate(r, v, users)
			if err != nil {
				// A token was supplied and is bad: that is an error, not a guest.
				Fail(w, r, err)
				return
			}
			next.ServeHTTP(w, r.WithContext(WithUser(r.Context(), user)))
		})
	}
}

func authenticate(r *http.Request, v *auth.Verifier, users UserStore) (*domain.User, error) {
	token, ok := bearerToken(r)
	if !ok {
		return nil, domain.ErrUnauthorized
	}
	claims, err := v.Verify(r.Context(), token)
	if err != nil {
		return nil, domain.ErrUnauthorized
	}
	user, err := users.UpsertUserFromSupabase(r.Context(),
		claims.Subject, claims.Email, claims.FullName, claims.Picture, claims.Provider)
	if err != nil {
		slog.Error("resolve user", "error", err, "request_id", RequestID(r.Context()))
		return nil, domain.ErrUnauthorized
	}
	return user, nil
}
