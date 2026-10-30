package auth

import (
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"
)

var (
	ErrMissingToken = errors.New("missing token")
	ErrInvalidToken = errors.New("invalid token")
)

type Verifier struct {
	token string
}

func NewVerifier(token string) Verifier {
	return Verifier{token: token}
}

func (v Verifier) Available() bool {
	return v.token != ""
}

func (v Verifier) VerifyRequest(r *http.Request) error {
	if v.token == "" {
		return nil
	}
	got := ExtractToken(r)
	if got == "" {
		return ErrMissingToken
	}
	if subtle.ConstantTimeCompare([]byte(got), []byte(v.token)) != 1 {
		return ErrInvalidToken
	}
	return nil
}

func ExtractToken(r *http.Request) string {
	if authz := r.Header.Get("Authorization"); authz != "" {
		fields := strings.Fields(authz)
		if len(fields) == 2 && strings.EqualFold(fields[0], "Bearer") {
			return fields[1]
		}
	}
	if token := r.Header.Get("X-Knot-Token"); token != "" {
		return token
	}
	return ""
}

type OriginChecker struct {
	allowed map[string]struct{}
}

func NewOriginChecker(origins []string) OriginChecker {
	allowed := make(map[string]struct{}, len(origins))
	for _, origin := range origins {
		if origin == "" {
			continue
		}
		allowed[origin] = struct{}{}
	}
	return OriginChecker{allowed: allowed}
}

func (c OriginChecker) Allowed(origin string) bool {
	if origin == "" {
		return true
	}
	_, ok := c.allowed[origin]
	return ok
}
