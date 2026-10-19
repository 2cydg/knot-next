package i18n

import (
	"context"
	"errors"
	"testing"
)

type fixedProvider struct {
	languages []string
	err       error
}

func (p fixedProvider) PreferredLanguages(context.Context) ([]string, error) {
	return p.languages, p.err
}

func TestPreferredLanguageOrderAndFallback(t *testing.T) {
	for _, tc := range []struct {
		languages []string
		err       error
		want      Language
	}{
		{[]string{"zh_CN.UTF-8", "en-US"}, nil, Chinese},
		{[]string{"fr-FR", "zh-CN"}, nil, English},
		{[]string{"", "zh-CN"}, nil, English},
		{nil, nil, English},
		{[]string{"zh-CN"}, errors.New("query failed"), English},
	} {
		got := Detect(context.Background(), fixedProvider{tc.languages, tc.err})
		if got.Language != tc.want || got.DetectionError != tc.err {
			t.Fatalf("%q: %+v", tc.languages, got)
		}
	}
}

func TestNormalizeLocale(t *testing.T) {
	for _, locale := range []string{"zh", "zh-CN", "zh-TW", "zh-HK", "zh-SG", "zh-Hans", "zh-Hant", "zh_CN.UTF-8", " ZH_tw.Big5@variant "} {
		if Normalize(locale) != Chinese {
			t.Errorf("%q did not select Chinese", locale)
		}
	}
	for _, locale := range []string{"en", "fr-FR", "ja_JP.UTF-8", "C", "C.UTF-8", "POSIX", "", "zho", "zhong"} {
		if Normalize(locale) != English {
			t.Errorf("%q did not select English", locale)
		}
	}
}

func TestMessageLocalePrecedence(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
		want Language
	}{
		{"LC_ALL wins", map[string]string{"LC_ALL": "fr_FR", "LC_MESSAGES": "zh_CN", "LANG": "zh_CN"}, English},
		{"LC_MESSAGES wins", map[string]string{"LC_MESSAGES": "zh_HK.UTF-8", "LANG": "en_US"}, Chinese},
		{"LANG", map[string]string{"LANG": "zh_TW.UTF-8"}, Chinese},
		{"LANGUAGE first", map[string]string{"LANG": "en_US.UTF-8", "LANGUAGE": "zh-Hant:en"}, Chinese},
		{"unsupported first", map[string]string{"LANG": "zh_CN", "LANGUAGE": "fr:zh"}, English},
		{"C overrides LANGUAGE", map[string]string{"LC_ALL": "C", "LANG": "zh_CN", "LANGUAGE": "zh"}, English},
		{"C UTF8 overrides LANGUAGE", map[string]string{"LC_MESSAGES": "C.UTF-8", "LANGUAGE": "zh"}, English},
		{"POSIX overrides LANGUAGE", map[string]string{"LANG": "POSIX", "LANGUAGE": "zh"}, English},
		{"no locale", map[string]string{"LANGUAGE": "zh:en"}, English},
		{"empty list segments", map[string]string{"LANG": "en_US", "LANGUAGE": ": :zh_CN:en"}, Chinese},
		{"empty LANGUAGE", map[string]string{"LANG": "zh_CN", "LANGUAGE": ":"}, Chinese},
	} {
		t.Run(tc.name, func(t *testing.T) {
			languages := localePreferences(func(key string) string { return tc.env[key] })
			got := Detect(context.Background(), fixedProvider{languages: languages})
			if got.Language != tc.want {
				t.Fatalf("%q -> %s", languages, got.Language)
			}
		})
	}
}

func TestSystemProviderHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := (SystemProvider{}).PreferredLanguages(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled query: %v", err)
	}
}
