package config

import (
	"reflect"
	"testing"

	"github.com/BurntSushi/toml"
)

func FuzzLegacyDecode(f *testing.F) {
	f.Add("[servers.a]\nalias='prod'\nhost='host'\nuser='u'\nport=22\n")
	f.Add("[settings]\nrecent_limit=5\n")
	f.Fuzz(func(t *testing.T, raw string) {
		if len(raw) > 1<<20 {
			return
		}
		var disk diskConfig
		if _, err := toml.Decode(raw, &disk); err != nil {
			return
		}
		cfg := configFromDisk(disk)
		_ = applyDiskDefaults(&cfg)
		_ = validate(cfg)
	})
}
func FuzzImportDestinationReservation(f *testing.F) {
	f.Add("a", "b", "x", "y")
	f.Add("a", "b", "x", "x")
	f.Fuzz(func(t *testing.T, a, b, x, y string) {
		if a == "" || b == "" || a == b || len(a)+len(b)+len(x)+len(y) > 2048 {
			return
		}
		source := map[string]ServerProfile{a: {ID: a, Alias: x}, b: {ID: b, Alias: y}}
		target := map[string]ServerProfile{b: {ID: b, Alias: x}}
		merge := func() (map[string]ServerProfile, map[string]string, []MigrationItem) {
			o, m, i, _ := mergeResources(source, target, "overwrite", "servers", func(v ServerProfile) (string, string) { return v.ID, v.Alias }, func(v ServerProfile, id string) ServerProfile { v.ID = id; return v })
			return o, m, i
		}
		out, mapping, items := merge()
		again, m2, i2 := merge()
		if !reflect.DeepEqual(out, again) || !reflect.DeepEqual(mapping, m2) || !reflect.DeepEqual(items, i2) {
			t.Fatal("non-deterministic import")
		}
		destinations := map[string]bool{}
		for _, id := range mapping {
			if destinations[id] {
				t.Fatal("distinct source resources share destination")
			}
			destinations[id] = true
		}
	})
}
