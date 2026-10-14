package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"knot-core/internal/resourceid"
	"net"
	"os"
	"strconv"
	"strings"
)

// ForwardDefinition is the public configuration DTO; runtime state is separate.
type ForwardDefinition struct {
	ID      string `json:"id,omitempty"`
	Type    string `json:"type"`
	Bind    string `json:"bind"`
	Target  string `json:"target,omitempty"`
	Enabled bool   `json:"enabled"`
}

func (f ForwardConfig) Definition(serverID string, index int) ForwardDefinition {
	typ := f.Type
	switch typ {
	case "L":
		typ = "local"
	case "R":
		typ = "remote"
	case "D":
		typ = "dynamic"
	}
	bind := f.Bind
	if bind == "" {
		bind = net.JoinHostPort("127.0.0.1", strconv.Itoa(f.LocalPort))
	}
	target := f.Target
	if target == "" && typ != "dynamic" {
		target = f.RemoteAddr
	}
	enabled := true
	if f.Enabled != nil {
		enabled = *f.Enabled
	}
	id := f.ID
	if id == "" {
		// Legacy rules have no ID; derive a stable non-secret definition identity.
		hash := sha256.Sum256([]byte(fmt.Sprintf("%s/%d/%s/%s/%s", serverID, index, typ, bind, target)))
		id = "rule_" + hex.EncodeToString(hash[:8])
	}
	return ForwardDefinition{ID: id, Type: typ, Bind: bind, Target: target, Enabled: enabled}
}
func ValidateForward(d ForwardDefinition) error {
	if d.Type != "local" && d.Type != "remote" && d.Type != "dynamic" {
		return fieldFailure("forwards", "type", "forward type must be local, remote or dynamic")
	}
	if err := forwardAddress(d.Bind, true); err != nil {
		return fieldFailure("forwards", "bind", "invalid forward bind")
	}
	if d.Type == "dynamic" {
		if d.Target != "" {
			return fieldFailure("forwards", "target", "dynamic forward target must be empty")
		}
	} else if err := forwardAddress(d.Target, false); err != nil {
		return fieldFailure("forwards", "target", "invalid forward target")
	}
	if strings.ContainsAny(d.ID, "/\\ \t\r\n\x00") || len(d.ID) > 128 {
		return fieldFailure("forwards", "id", "invalid forward ID")
	}
	return nil
}
func forwardAddress(addr string, bind bool) error {
	host, p, err := net.SplitHostPort(addr)
	if err != nil || strings.ContainsAny(host, " /\t\r\n\x00") || host == "" {
		return ErrValidation
	}
	// Bind addresses are explicit IPs (including IPv6 zones) or localhost.
	ipHost := host
	if zone := strings.LastIndex(ipHost, "%"); zone >= 0 {
		ipHost = ipHost[:zone]
	}
	if bind && net.ParseIP(ipHost) == nil && host != "localhost" {
		return ErrValidation
	}
	if !bind && (len(host) > 255 || (strings.Contains(host, ":") && net.ParseIP(ipHost) == nil)) {
		return ErrValidation
	}
	n, err := strconv.Atoi(p)
	if err != nil || n < 0 || n > 65535 || (!bind && n == 0) {
		return ErrValidation
	}
	return nil
}
func ForwardDefinitions(server ServerProfile) []ForwardDefinition {
	out := make([]ForwardDefinition, 0, len(server.Forwards))
	for i, f := range server.Forwards {
		out = append(out, f.Definition(server.ID, i))
	}
	return out
}
func (s *Service) GetForwards(serverID string) ([]ForwardDefinition, error) {
	cfg, err := s.snapshot()
	if err != nil {
		return nil, err
	}
	server, ok := cfg.Servers[serverID]
	if !ok {
		return nil, ErrNotFound
	}
	return ForwardDefinitions(server), nil
}

// PutForwards atomically replaces definitions. It affects future transports;
// existing runtime instances retain their creation snapshot.
func (s *Service) PutForwards(serverID string, definitions []ForwardDefinition) ([]ForwardDefinition, error) {
	if len(definitions) > 128 {
		return nil, fieldFailure("forwards", "forwards", "at most 128 forward definitions")
	}
	var out []ForwardDefinition
	err := s.withConfig(func(cfg *Config) error {
		server, ok := cfg.Servers[serverID]
		if !ok {
			return ErrNotFound
		}
		rules := make([]ForwardConfig, 0, len(definitions))
		ids := map[string]bool{}
		for index, d := range definitions {
			if d.ID == "" {
				var err error
				d.ID, err = resourceid.Allocate("rule", nil, func(id string) bool { return ids[id] })
				if err != nil {
					return err
				}
			}
			if ids[d.ID] {
				return fieldFailure("forwards", fmt.Sprintf("forwards[%d].id", index), "duplicate forward ID")
			}
			ids[d.ID] = true
			if err := ValidateForward(d); err != nil {
				if fields, ok := err.(*ValidationFailure); ok {
					for i := range fields.Errors {
						fields.Errors[i].Field = fmt.Sprintf("forwards[%d].%s", index, fields.Errors[i].Field)
					}
				}
				return err
			}
			enabled := d.Enabled
			typ := map[string]string{"local": "L", "remote": "R", "dynamic": "D"}[d.Type]
			_, port, _ := net.SplitHostPort(d.Bind)
			n, _ := strconv.Atoi(port)
			rules = append(rules, ForwardConfig{ID: d.ID, Type: typ, Bind: d.Bind, Target: d.Target, Enabled: &enabled, LocalPort: n, RemoteAddr: d.Target})
		}
		// Preserve exact pre-extension TOML once, including ciphertext. Older knot
		// loaders cannot express all explicit bind/disabled semantics.
		if _, err := os.Stat(s.path + ".pre-forwards.bak"); os.IsNotExist(err) {
			if raw, err := os.ReadFile(s.path); err == nil {
				if err = s.writeFile(s.path+".pre-forwards.bak", raw, 0600); err != nil {
					return err
				}
			}
		}
		server.Forwards = rules
		cfg.Servers[serverID] = server
		out = ForwardDefinitions(server)
		return nil
	})
	return out, err
}

type ValidationFailure struct{ Errors []ValidationError }

func (e *ValidationFailure) Error() string {
	if len(e.Errors) == 0 {
		return ErrValidation.Error()
	}
	return ErrValidation.Error() + ": " + e.Errors[0].Message
}
func (e *ValidationFailure) Unwrap() error { return ErrValidation }

func fieldFailure(resource, field, message string) error {
	return &ValidationFailure{Errors: []ValidationError{{Resource: resource, Field: field, Message: message}}}
}
