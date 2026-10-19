package i18n

import (
	"errors"
	"io"
	"reflect"
	"testing"
)

func TestAppleLanguageArray(t *testing.T) {
	for _, tc := range []struct {
		output string
		want   []string
	}{
		{"(\n    \"fr-FR\",\n    \"zh-Hans-CN\"\n)\n", []string{"fr-FR", "zh-Hans-CN"}},
		{"(en, \"zh-Hant\", ja_JP)", []string{"en", "zh-Hant", "ja_JP"}},
		{"(zh_CN,)", []string{"zh_CN"}},
		{"()", nil},
	} {
		got, err := parseAppleLanguages(tc.output)
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("%q: %q, %v", tc.output, got, err)
		}
	}
	for _, output := range []string{"", "not found", "(\"zh-CN\"", "(en zh)", "(,en)", "(en,,zh)", "(\"\")", "(\"zh\") garbage", "(\"zh\\nCN\")", "(\"zh-CN)", "(\"zh;rm\")"} {
		if _, err := parseAppleLanguages(output); err == nil {
			t.Errorf("accepted invalid output %q", output)
		}
	}
}

func TestAppleQueryOutputBound(t *testing.T) {
	output := &cappedOutput{remaining: 4}
	if n, err := output.Write([]byte("ab")); n != 2 || err != nil {
		t.Fatal(n, err)
	}
	if n, err := output.Write([]byte("cdef")); n != 2 || !errors.Is(err, io.ErrShortWrite) {
		t.Fatal(n, err)
	}
	if output.String() != "abcd" {
		t.Fatal(output.String())
	}
	if n, err := output.Write([]byte("x")); n != 0 || !errors.Is(err, io.ErrShortWrite) {
		t.Fatal(n, err)
	}
}
