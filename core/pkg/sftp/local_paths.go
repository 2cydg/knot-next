package sftp

import (
	"os"
	"path/filepath"
	"strings"
)

func expandLocalHome(input string) (string, error) {
	if input == "~" {
		return os.UserHomeDir()
	}
	if strings.HasPrefix(input, "~"+string(os.PathSeparator)) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, input[2:]), nil
	}
	if os.PathSeparator == '\\' && strings.HasPrefix(input, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, filepath.FromSlash(input[2:])), nil
	}
	return input, nil
}

func localPathHasTrailingSeparator(input string) bool {
	return hasTrailingLocalSeparator(input) && !isLocalRoot(input)
}

func trimTrailingLocalSeparators(input string) string {
	for hasTrailingLocalSeparator(input) && !isLocalRoot(input) {
		input = input[:len(input)-1]
	}
	return input
}

func hasTrailingLocalSeparator(input string) bool {
	return strings.HasSuffix(input, "/") || strings.HasSuffix(input, `\`)
}

func isLocalRoot(input string) bool {
	if input == "/" || input == `\` {
		return true
	}
	if len(input) == 3 && isASCIIAlpha(input[0]) && input[1] == ':' && (input[2] == '/' || input[2] == '\\') {
		return true
	}
	return false
}

func localPathHasDotSuffix(input string) bool {
	return strings.HasSuffix(input, "/.") || strings.HasSuffix(input, `\.`)
}

func trimLocalDotSuffix(input string) string {
	if localPathHasDotSuffix(input) {
		return input[:len(input)-2]
	}
	return input
}

func localBase(input string) string {
	input = trimTrailingLocalSeparators(input)
	input = filepath.Clean(input)
	base := filepath.Base(input)
	if base != "." && base != string(filepath.Separator) {
		return base
	}

	trimmed := strings.TrimRight(input, `/\`)
	idx := strings.LastIndexAny(trimmed, `/\`)
	if idx >= 0 {
		return trimmed[idx+1:]
	}
	return trimmed
}

func isASCIIAlpha(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}
