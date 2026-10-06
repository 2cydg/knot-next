package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net/http"
	"os"
	"strings"
)

const tokenBytes = 32

var (
	ErrMissingToken = errors.New("missing token")
	ErrInvalidToken = errors.New("invalid token")
)

type TokenStore struct {
	Path string
}

func (s TokenStore) LoadOrCreate() (string, error) {
	token, err := s.Load()
	if err == nil {
		return token, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	token, err = GenerateToken()
	if err != nil {
		return "", err
	}
	file, err := os.OpenFile(s.Path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return s.Load()
		}
		return "", err
	}
	defer file.Close()
	if _, err := file.WriteString(token + "\n"); err != nil {
		return "", err
	}
	return token, nil
}

func (s TokenStore) Load() (string, error) {
	raw, err := os.ReadFile(s.Path)
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return "", ErrInvalidToken
	}
	if err := os.Chmod(s.Path, 0o600); err != nil {
		return "", err
	}
	return token, nil
}

func GenerateToken() (string, error) {
	buf := make([]byte, tokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

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
