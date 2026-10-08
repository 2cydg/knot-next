package sftp

import (
	"knot-core/pkg/config"
	"knot-core/pkg/session"
)

// saveCredentials persists a verified credential candidate after the SFTP
// subsystem opened. It runs off the request path, and a failure never disturbs
// the working connection: the client is told through a sanitized warning.
func (s *Service) saveCredentials(sessionID string, serverID string, resp ChallengeResponse) {
	if serverID == "" || (resp.Password == "" && resp.KeyID == "") {
		return
	}
	choice := config.AuthChoice{}
	label := "authentication choice"
	switch {
	case resp.KeyID != "":
		choice.Method, choice.KeyID, label = config.AuthMethodKey, resp.KeyID, "key"
	case resp.Password != "":
		choice.Method, choice.Password, label = config.AuthMethodPassword, resp.Password, "password"
	}
	if err := s.config.RememberServerAuth(serverID, choice); err != nil {
		s.publishWarning(sessionID, session.WarningCredentialSaveFailed, label+" was not saved")
	}
}
