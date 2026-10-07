package runtime

import (
	"encoding/json"
	"fmt"
	"os"
)

// ReadInfo reads runtime info from a file.
func ReadInfo(path string) (Info, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Info{}, err
	}

	var info Info
	if err := json.Unmarshal(data, &info); err != nil {
		return Info{}, err
	}

	return info, nil
}

// ValidateOwnership checks if the runtime file belongs to the given instance ID.
func ValidateOwnership(path string, expectedInstanceID string) error {
	info, err := ReadInfo(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // No file, no conflict
		}
		return fmt.Errorf("read runtime info: %w", err)
	}

	if info.InstanceID != expectedInstanceID {
		return fmt.Errorf("runtime file belongs to another instance (found: %s, expected: %s)", info.InstanceID, expectedInstanceID)
	}

	return nil
}
