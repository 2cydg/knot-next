package session

import (
	"strings"
	"time"

	"knot-core/pkg/config"
)

// saveCredentials saves authentication credentials after a successful
// connection. It runs asynchronously so a slow config write cannot delay the
// connection itself, and a failure leaves the working connection untouched: the
// client learns about it through a sanitized warning instead.
func (s *Service) saveCredentials(sessionID string, serverID string, resp ChallengeResponse) {
	if resp.Password != "" {
		if _, err := s.config.SetServerPassword(serverID, resp.Password); err != nil {
			s.publishWarning(sessionID, WarningCredentialSaveFailed, "password was not saved")
			return
		}
		return
	}
	if resp.KeyID != "" {
		// For KeyID, we need to update the server profile
		cfg, err := s.config.RuntimeConfig()
		if err != nil {
			s.publishWarning(sessionID, WarningCredentialSaveFailed, "key was not saved")
			return
		}

		server, ok := cfg.Servers[serverID]
		if !ok {
			return
		}

		profile := server
		profile.AuthMethod = config.AuthMethodKey
		profile.KeyID = resp.KeyID
		profile.Password = "" // Clear password when switching to key

		if _, err := s.config.UpdateServer(serverID, profile); err != nil {
			s.publishWarning(sessionID, WarningCredentialSaveFailed, "key was not saved")
			return
		}
	}
	// TODO: Handle passphrase saving when encrypted key support is added
}

// publishWarning emits a sanitized, observable warning about a non-fatal
// condition. A client can subscribe to it through the session event stream (or
// the service-level OnEvent callback once the session is gone), which is what
// lets it tell a client that a credential it asked to remember was not saved.
//
// Every argument in secrets is removed from the message before publication, so
// a config-writer error that echoes the credential back cannot turn the warning
// itself into a credential leak.
func (s *Service) publishWarning(sessionID string, kind string, message string, secrets ...string) {
	safe := sanitizeWarning(message, secrets...)
	now := time.Now().UTC()
	event := Event{
		Type:      "session.warning",
		SessionID: sessionID,
		Warning:   &Warning{Kind: kind, Message: safe},
		Time:      now,
	}

	s.mu.Lock()
	if session, ok := s.sessions[sessionID]; ok {
		event.State = session.State
		s.publishSessionLocked(session, event)
		s.mu.Unlock()
		return
	}
	callback := s.onEvent
	s.mu.Unlock()
	if callback != nil {
		callback(cloneEvent(event))
	}
}

// redacted replaces every occurrence of a credential with a fixed marker.
const redacted = "[redacted]"

// sanitizeWarning removes all occurrences of supplied secrets. Credential-save
// warnings additionally use fixed messages instead of arbitrary writer errors.
func sanitizeWarning(message string, secrets ...string) string {
	out := message
	for _, secret := range secrets {
		if secret != "" {
			out = strings.ReplaceAll(out, secret, redacted)
		}
	}
	return out
}
