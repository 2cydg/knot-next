package i18n

import (
	"errors"
	"io"
	"strconv"
	"strings"
	"text/scanner"
	"unicode"
)

// parseAppleLanguages accepts the small OpenStep array emitted by defaults,
// preserving preference order. It rejects partial or unexpected command output.
func parseAppleLanguages(output string) ([]string, error) {
	var s scanner.Scanner
	s.Init(strings.NewReader(output))
	s.Mode = scanner.ScanIdents | scanner.ScanStrings
	s.IsIdentRune = func(r rune, i int) bool {
		return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(i > 0 && (unicode.IsDigit(r) || r == '-' || r == '_'))
	}
	var scanErr error
	s.Error = func(*scanner.Scanner, string) { scanErr = errors.New("invalid AppleLanguages output") }
	invalid := errors.New("invalid AppleLanguages array")
	if s.Scan() != '(' {
		return nil, invalid
	}
	var languages []string
	token := s.Scan()
	for token != ')' {
		if token != scanner.Ident && token != scanner.String {
			return nil, invalid
		}
		value := s.TokenText()
		if token == scanner.String {
			var err error
			value, err = strconv.Unquote(value)
			if err != nil {
				return nil, invalid
			}
		}
		if value == "" || strings.IndexFunc(value, func(r rune) bool {
			return !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
				(r >= '0' && r <= '9') || r == '-' || r == '_')
		}) >= 0 {
			return nil, invalid
		}
		languages = append(languages, value)
		token = s.Scan()
		if token == ',' {
			token = s.Scan()
		} else if token != ')' {
			return nil, invalid
		}
	}
	if s.Scan() != scanner.EOF || scanErr != nil {
		return nil, invalid
	}
	return languages, nil
}

// cappedOutput bounds memory and makes the command fail if it emits more than
// the expected preference list. It is used only for the stdout pipe.
type cappedOutput struct {
	strings.Builder
	remaining int
}

func (b *cappedOutput) Write(p []byte) (int, error) {
	if len(p) > b.remaining {
		n, _ := b.Builder.Write(p[:b.remaining])
		b.remaining = 0
		return n, io.ErrShortWrite
	}
	b.remaining -= len(p)
	return b.Builder.Write(p)
}
