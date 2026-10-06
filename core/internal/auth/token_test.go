package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestVerifierAcceptsBearerToken(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/v1/status", nil)
	req.Header.Set("Authorization", "Bearer secret-token")

	err := NewVerifier("secret-token").VerifyRequest(req)
	if err != nil {
		t.Fatalf("VerifyRequest returned error: %v", err)
	}
}

func TestVerifierAcceptsHeaderToken(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/v1/status", nil)
	req.Header.Set("X-Knot-Token", "secret-token")

	err := NewVerifier("secret-token").VerifyRequest(req)
	if err != nil {
		t.Fatalf("VerifyRequest returned error: %v", err)
	}
}

func TestVerifierRejectsQueryToken(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/v1/status?access_token=secret-token&token=secret-token", nil)

	err := NewVerifier("secret-token").VerifyRequest(req)
	if err != ErrMissingToken {
		t.Fatalf("VerifyRequest error = %v, want %v", err, ErrMissingToken)
	}
}

func TestVerifierRejectsMissingAndInvalidToken(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/v1/status", nil)
	if err := NewVerifier("secret-token").VerifyRequest(req); err != ErrMissingToken {
		t.Fatalf("missing token error = %v, want %v", err, ErrMissingToken)
	}

	req.Header.Set("X-Knot-Token", "wrong")
	if err := NewVerifier("secret-token").VerifyRequest(req); err != ErrInvalidToken {
		t.Fatalf("invalid token error = %v, want %v", err, ErrInvalidToken)
	}
}

func TestOriginCheckerAllowsEmptyOriginOnlyByDefault(t *testing.T) {
	checker := NewOriginChecker(nil)
	if !checker.Allowed("") {
		t.Fatal("empty origin should be allowed")
	}
	if checker.Allowed("http://example.test") {
		t.Fatal("unexpected origin should be rejected")
	}
}
