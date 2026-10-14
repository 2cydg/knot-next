// Package resourceid allocates opaque public resource identities.
package resourceid

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
)

type Generator func() (string, error)

func Random() (string, error) {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

// Allocate must be called inside the registry's insertion lock.
func Allocate(prefix string, generate Generator, exists func(string) bool) (string, error) {
	if generate == nil {
		generate = Random
	}
	for i := 0; i < 64; i++ {
		raw, err := generate()
		if err != nil {
			return "", err
		}
		id := prefix + "_" + raw
		if !exists(id) {
			return id, nil
		}
	}
	return "", errors.New("resource identity collision limit reached")
}
