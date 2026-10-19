package i18n

import (
	"reflect"
	"strings"
	"testing"
	"text/template/parse"
)

func fields(node parse.Node, result map[string]bool) {
	switch n := node.(type) {
	case *parse.ListNode:
		for _, child := range n.Nodes {
			fields(child, result)
		}
	case *parse.ActionNode:
		fields(n.Pipe, result)
	case *parse.PipeNode:
		for _, command := range n.Cmds {
			fields(command, result)
		}
	case *parse.CommandNode:
		for _, arg := range n.Args {
			fields(arg, result)
		}
	case *parse.FieldNode:
		for _, ident := range n.Ident {
			result[ident] = true
		}
	}
}

func TestCatalogCoverageAndParameterAgreement(t *testing.T) {
	if len(catalogs[English]) != len(catalogs[Chinese]) {
		t.Fatal("catalog sizes differ")
	}
	for key := range catalogs[English] {
		if catalogs[Chinese][key] == "" {
			t.Errorf("missing Chinese key %s", key)
			continue
		}
		enFields, zhFields := map[string]bool{}, map[string]bool{}
		fields(compiled[English][key].Tree.Root, enFields)
		fields(compiled[Chinese][key].Tree.Root, zhFields)
		if !reflect.DeepEqual(enFields, zhFields) {
			t.Errorf("%s placeholders: %v vs %v", key, enFields, zhFields)
		}
		params := map[string]any{}
		for field := range enFields {
			params[field] = "value"
		}
		for _, language := range []Language{English, Chinese} {
			if _, err := New(language).Render(key, params); err != nil {
				t.Errorf("%s/%s: %v", language, key, err)
			}
		}
	}
}

func TestCatalogPreservesValuesAndReportsMissingData(t *testing.T) {
	value := "生产 [*?] /tmp/中文 {{.Injected}}"
	for _, language := range []Language{English, Chinese} {
		translator := New(language)
		message, err := translator.Render(InvalidArguments, map[string]any{"Argument": value})
		if err != nil {
			t.Fatal(err)
		}
		// Template values are literal; paths/aliases are not interpreted as templates.
		if !strings.Contains(message, value) {
			t.Fatalf("value changed: %s", message)
		}
		if _, err := translator.Render(InvalidArguments, nil); err == nil {
			t.Fatal("missing argument silently accepted")
		}
		if _, err := translator.Render("missing.key", nil); err == nil {
			t.Fatal("unknown key silently accepted")
		}
		if !strings.Contains(translator.Text("missing.key", nil), "missing.key") {
			t.Fatal("unknown key lost in fallback")
		}
	}
}
