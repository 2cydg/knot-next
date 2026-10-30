package main

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/BurntSushi/toml"
	"knot-core/pkg/config"
)

// initializeConfig deliberately does not compute user paths or open services.
func initializeConfig(path string, output io.Writer) error {
	secret, err := randomSecret()
	if err != nil {
		return fmt.Errorf("generate Secret: %w", err)
	}
	var body bytes.Buffer
	settings := struct {
		Service config.ServiceSettings `toml:"service"`
	}{Service: config.ServiceSettings{Port: defaultPort, Secret: secret, AllowedOrigins: []string{}}}
	if err := toml.NewEncoder(&body).Encode(settings); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if errors.Is(err, os.ErrExist) {
		return errors.New("config.toml already exists; existing configuration was preserved")
	}
	if err != nil {
		return fmt.Errorf("create config.toml: %w", err)
	}
	_, writeErr := file.Write(body.Bytes())
	closeErr := file.Close()
	if writeErr != nil {
		return fmt.Errorf("write config.toml: %w", writeErr)
	}
	if closeErr != nil {
		return closeErr
	}
	_, err = fmt.Fprintln(output, "Created config.toml")
	return err
}

func randomSecret() (string, error) {
	const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	for {
		value := make([]byte, 32)
		lower, upper, digit := false, false, false
		for i := range value {
			// Rejection sampling avoids modulo bias across the 62 characters.
			var sample [1]byte
			for {
				if _, err := rand.Read(sample[:]); err != nil {
					return "", err
				}
				if sample[0] < 248 {
					break
				}
			}
			ch := alphabet[int(sample[0])%len(alphabet)]
			value[i] = ch
			lower = lower || ch >= 'a' && ch <= 'z'
			upper = upper || ch >= 'A' && ch <= 'Z'
			digit = digit || ch >= '0' && ch <= '9'
		}
		if lower && upper && digit {
			return string(value), nil
		}
	}
}
