package httpx_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hireonai/backend-hireon-go/internal/domain"
	"github.com/hireonai/backend-hireon-go/internal/httpx"
)

// fakeUserStore stands in for the repository so middleware behaviour can be
// tested without a database.
type fakeUserStore struct {
	got   *domain.User
	calls int
}

func (f *fakeUserStore) UpsertUserFromSupabase(_ context.Context, sub, email, fullname, picture, provider string) (*domain.User, error) {
	f.calls++
	f.got = &domain.User{ID: "local-1", SupabaseID: sub, Email: email}
	return f.got, nil
}

// TestFailMapsSentinels locks in the status mapping, which is the single place
// that translates service errors into HTTP. It replaces the Node backend's three
// separate response-assembly sites.
func TestFailMapsSentinels(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
	}{
		{"not found", domain.ErrNotFound, http.StatusNotFound},
		{"unauthorized", domain.ErrUnauthorized, http.StatusUnauthorized},
		{"forbidden", domain.ErrForbidden, http.StatusForbidden},
		{"conflict", domain.ErrConflict, http.StatusConflict},
		{"upstream", domain.ErrUpstream, http.StatusBadGateway},
		{"upstream slow", domain.ErrUpstreamSlow, http.StatusGatewayTimeout},
		{"api error 404", domain.NewAPIError(404, "CV not found.", nil), http.StatusNotFound},
		{"api error 400", domain.NewAPIError(400, "Invalid job id.", nil), http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/x", nil)
			httpx.Fail(rec, req, tc.err)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d", rec.Code, tc.status)
			}
			body := rec.Body.String()
			if len(body) == 0 || body[0] != '{' {
				t.Fatalf("expected an envelope, got %q", body)
			}
		})
	}
}

// TestEnvelopeShape pins the response contract the frontend depends on.
func TestEnvelopeShape(t *testing.T) {
	rec := httptest.NewRecorder()
	httpx.Success(rec, map[string]any{"a": 1}, "ok")
	want := `{"statusCode":200,"success":true,"message":"ok","data":{"a":1}}` + "\n"
	if rec.Body.String() != want {
		t.Fatalf("body = %q, want %q", rec.Body.String(), want)
	}

	rec = httptest.NewRecorder()
	httpx.Error(rec, http.StatusNotFound, "nope")
	want = `{"statusCode":404,"success":false,"message":"nope","error":"Not Found"}` + "\n"
	if rec.Body.String() != want {
		t.Fatalf("body = %q, want %q", rec.Body.String(), want)
	}
}

// TestInternalErrorDoesNotLeakCause proves an internal error message never
// reaches the client -- the Node backend returned str(e) verbatim.
func TestInternalErrorDoesNotLeakCause(t *testing.T) {
	secret := "pq: password authentication failed for user \"admin\""
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	httpx.Fail(rec, req, domain.NewAPIError(500, "Internal Server Error", errString(secret)))

	if body := rec.Body.String(); contains(body, secret) {
		t.Fatalf("response leaked the internal cause: %s", body)
	}
}

type errString string

func (e errString) Error() string { return string(e) }

func contains(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}
