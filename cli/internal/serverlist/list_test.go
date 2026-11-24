package serverlist

import (
	"bytes"
	"encoding/json"
	"knot-cli/internal/coreclient"
	"knot-cli/internal/i18n"
	"strings"
	"testing"
	"time"
)

func TestRenderFiltersSortsAndSanitizes(t *testing.T) {
	now := time.Now()
	servers := []coreclient.Server{{Alias: "z\nunsafe", Host: "host", User: "ops", Tags: []string{"prod"}}, {Alias: "recent", Host: "other", User: "ops", Tags: []string{"prod"}, LastUsed: &now}, {Alias: "hidden", Host: "else"}}
	var out bytes.Buffer
	if err := Render(&out, servers, "PROD", i18n.New(i18n.English)); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if strings.Contains(got, "hidden") || !strings.Contains(got, "z�unsafe") || strings.Index(got, "recent") > strings.Index(got, "z�unsafe") {
		t.Fatal(got)
	}
}

func TestRenderJSONFiltersAndSorts(t *testing.T) {
	now := time.Now()
	servers := []coreclient.Server{{Alias: "old", Tags: []string{"prod"}}, {Alias: "recent", Tags: []string{"prod"}, LastUsed: &now}, {Alias: "hidden"}}
	var out bytes.Buffer
	if err := RenderJSON(&out, servers, "PROD"); err != nil {
		t.Fatal(err)
	}
	var data []struct {
		Alias string   `json:"alias"`
		Tags  []string `json:"tags"`
	}
	if err := json.Unmarshal(out.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	if len(data) != 2 || data[0].Alias != "recent" || data[1].Alias != "old" {
		t.Fatalf("result: %s", out.String())
	}
	out.Reset()
	if err := RenderJSON(&out, servers, "absent"); err != nil || strings.TrimSpace(out.String()) != "[]" {
		t.Fatalf("empty result: %s %v", out.String(), err)
	}
}
