package config

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/BurntSushi/toml"
	"knot-core/internal/paths"
)

const DefaultServicePort = 17898

// ServiceSettings are restart-only settings, excluded from business API views.
type ServiceSettings struct {
	Port           int      `toml:"port" json:"-"`
	Secret         string   `toml:"secret" json:"-"`
	AllowedOrigins []string `toml:"allowed_origins" json:"-"`
}

// LoadService reads only startup settings. Explicit files must already exist.
func LoadService(layout paths.Layout, explicit bool) (ServiceSettings, error) {
	result := ServiceSettings{Port: DefaultServicePort}
	raw, err := os.ReadFile(layout.ConfigurationFile())
	if errors.Is(err, os.ErrNotExist) && !explicit {
		return result, nil
	}
	if err != nil {
		return result, fmt.Errorf("read core configuration: %w", err)
	}
	disk := struct {
		Service ServiceSettings `toml:"service"`
	}{Service: result}
	if _, err := toml.Decode(string(raw), &disk); err != nil {
		return result, errors.New("invalid TOML core configuration")
	}
	result = disk.Service
	if result.Port < 1 || result.Port > 65535 {
		return result, errors.New("service.port must be between 1 and 65535")
	}
	if strings.ContainsAny(result.Secret, " \t\r\n") {
		return result, errors.New("service.secret cannot contain whitespace")
	}
	return result, nil
}
