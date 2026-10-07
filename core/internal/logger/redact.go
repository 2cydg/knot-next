package logger

import (
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
)

const Redacted = "[redacted]"

var (
	privateBlock = regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?(?:-----END [A-Z0-9 ]*PRIVATE KEY-----|$)`)
	authHeader   = regexp.MustCompile(`(?i)(authorization\s*[:=]\s*)(?:bearer|basic)\s+[^\s",;]+`)
	credentials  = regexp.MustCompile(`(?i)((?:password|passphrase|token|secret_access_key|session_token)\s*[=:]\s*)[^\s",;]+`)
	proxyURL     = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://)[^\s/@]+@`)
)

// Registry is bounded. If requests exhaust its budget, free-text diagnostics
// fail closed instead of dropping old secrets and accidentally exposing them.
type Redactor struct {
	mu        sync.RWMutex
	secrets   map[string]struct{}
	bytes     int
	saturated bool
}

func NewRedactor() *Redactor { return &Redactor{secrets: map[string]struct{}{}} }
func sensitive(key string) bool {
	key = strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(key, "_", ""), "-", ""))
	switch key {
	case "password", "passphrase", "privatekey", "token", "authorization", "secretaccesskey", "sessiontoken", "accesskeyid":
		return true
	}
	return false
}
func (r *Redactor) Register(value any) {
	var walk func(reflect.Value, string)
	walk = func(v reflect.Value, key string) {
		if !v.IsValid() {
			return
		}
		if v.Kind() == reflect.Interface || v.Kind() == reflect.Pointer {
			if !v.IsNil() {
				walk(v.Elem(), key)
			}
			return
		}
		switch v.Kind() {
		case reflect.String:
			if sensitive(key) {
				r.Add(v.String())
			}
		case reflect.Struct:
			for i := 0; i < v.NumField(); i++ {
				if v.Type().Field(i).IsExported() {
					walk(v.Field(i), v.Type().Field(i).Name)
				}
			}
		case reflect.Map:
			iter := v.MapRange()
			for iter.Next() {
				walk(iter.Value(), fmt.Sprint(iter.Key()))
			}
		case reflect.Slice, reflect.Array:
			for i := 0; i < v.Len(); i++ {
				walk(v.Index(i), key)
			}
		}
	}
	walk(reflect.ValueOf(value), "")
}
func (r *Redactor) Add(secret string) {
	if secret == "" || strings.HasPrefix(secret, "ENC:") {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.secrets[secret]; ok {
		return
	}
	if len(r.secrets) >= 4096 || r.bytes+len(secret) > 4<<20 {
		r.saturated = true
		return
	}
	r.secrets[secret] = struct{}{}
	r.bytes += len(secret)
}
func (r *Redactor) Text(value string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.saturated {
		return Redacted
	}
	secrets := make([]string, 0, len(r.secrets))
	for s := range r.secrets {
		secrets = append(secrets, s)
	}
	sort.Slice(secrets, func(i, j int) bool { return len(secrets[i]) > len(secrets[j]) })
	for _, secret := range secrets {
		value = strings.ReplaceAll(value, secret, Redacted)
	}
	value = privateBlock.ReplaceAllString(value, Redacted)
	value = authHeader.ReplaceAllString(value, "${1}"+Redacted)
	value = credentials.ReplaceAllString(value, "${1}"+Redacted)
	return proxyURL.ReplaceAllString(value, "${1}"+Redacted+"@")
}
func (r *Redactor) Saturated() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.saturated
}
func (r *Redactor) Value(key string, value any) any {
	if sensitive(key) {
		return Redacted
	}
	if err, ok := value.(error); ok {
		return r.Text(err.Error())
	}
	raw, err := json.Marshal(value)
	if err != nil {
		// Formatting an unmarshalable map could expose unregistered sensitive keys.
		return Redacted
	}
	var data any
	if json.Unmarshal(raw, &data) != nil {
		return Redacted
	}
	var walk func(any) any
	walk = func(v any) any {
		switch x := v.(type) {
		case string:
			return r.Text(x)
		case map[string]any:
			for k, v := range x {
				x[k] = r.Value(k, v)
			}
			return x
		case []any:
			for i, v := range x {
				x[i] = walk(v)
			}
			return x
		default:
			return x
		}
	}
	return walk(data)
}

var currentMu sync.RWMutex
var current = NewRedactor()

func UseRedactor(r *Redactor)    { currentMu.Lock(); current = r; currentMu.Unlock() }
func DefaultRedactor() *Redactor { currentMu.RLock(); defer currentMu.RUnlock(); return current }
func RegisterSecrets(value any)  { DefaultRedactor().Register(value) }
func Redact(value string) string { return DefaultRedactor().Text(value) }
