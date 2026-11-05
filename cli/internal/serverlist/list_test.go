package serverlist

import (
	"bytes"
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
