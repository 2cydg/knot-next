package i18n

import (
	"context"
	"strings"
)

type Language string

const (
	English Language = "en"
	Chinese Language = "zh"
)

// LanguageProvider returns the current user's ordered preferences. Only the
// first preference is considered; unsupported first languages select English.
type LanguageProvider interface {
	PreferredLanguages(context.Context) ([]string, error)
}

type Selection struct {
	Language       Language
	DetectionError error
}

func Detect(ctx context.Context, provider LanguageProvider) Selection {
	languages, err := provider.PreferredLanguages(ctx)
	if err != nil {
		return Selection{Language: English, DetectionError: err}
	}
	if len(languages) == 0 {
		return Selection{Language: English}
	}
	return Selection{Language: Normalize(languages[0])}
}

func Normalize(locale string) Language {
	locale = strings.ToLower(strings.TrimSpace(locale))
	locale = strings.ReplaceAll(locale, "_", "-")
	if i := strings.IndexAny(locale, ".@"); i >= 0 {
		locale = locale[:i]
	}
	primary, _, _ := strings.Cut(locale, "-")
	if primary == "zh" {
		return Chinese
	}
	return English
}

// localePreferences implements GNU message-locale precedence. LANGUAGE only
// applies when an explicit, non-C/POSIX message locale is in effect.
func localePreferences(getenv func(string) string) []string {
	var locale string
	for _, key := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		if locale = strings.TrimSpace(getenv(key)); locale != "" {
			break
		}
	}
	if locale == "" {
		return nil
	}
	base := strings.ToLower(locale)
	if i := strings.IndexAny(base, ".@"); i >= 0 {
		base = base[:i]
	}
	if base == "c" || base == "posix" {
		return []string{locale}
	}
	for _, language := range strings.Split(getenv("LANGUAGE"), ":") {
		if language = strings.TrimSpace(language); language != "" {
			return []string{language}
		}
	}
	return []string{locale}
}
