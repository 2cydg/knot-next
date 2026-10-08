package config

import "testing"

func TestRegressionImportMappingCollision(t *testing.T) {
	source := defaultConfig()
	target := defaultConfig()
	target.Servers["b"] = ServerProfile{ID: "b", Alias: "x", Host: "target", User: "u", Port: 22, AuthMethod: AuthMethodAgent}
	source.Servers["a"] = ServerProfile{ID: "a", Alias: "x", Host: "source-a", User: "u", Port: 22, AuthMethod: AuthMethodAgent}
	source.Servers["b"] = ServerProfile{ID: "b", Alias: "y", Host: "source-b", User: "u", Port: 22, AuthMethod: AuthMethodAgent}
	next, items, mapping, err := mergeImport(source, target, "overwrite", false)
	if err == nil && !hasMigrationConflict(items) {
		t.Errorf("two distinct source servers silently mapped to same ID: %v final=%+v", mapping["servers"], next.Servers)
	}
}

func TestImportReservationAcrossPoliciesAndResourceTypes(t *testing.T) {
	for _, mode := range []string{"overwrite", "skip_existing", "fail_on_conflict"} {
		t.Run(mode, func(t *testing.T) {
			source, target := defaultConfig(), defaultConfig()
			source.Keys = map[string]KeyMetadata{"a": {ID: "a", Alias: "x"}, "b": {ID: "b", Alias: "y"}}
			target.Keys = map[string]KeyMetadata{"b": {ID: "b", Alias: "x"}}
			source.Proxies = map[string]ProxyProfile{"a": {ID: "a", Alias: "x"}, "b": {ID: "b", Alias: "y"}}
			target.Proxies = map[string]ProxyProfile{"b": {ID: "b", Alias: "x"}}
			source.SyncProviders = map[string]SyncProviderConfig{"a": {ID: "a", Alias: "x"}, "b": {ID: "b", Alias: "y"}}
			target.SyncProviders = map[string]SyncProviderConfig{"b": {ID: "b", Alias: "x"}}
			_, items, mappings, err := mergeImport(source, target, mode, false)
			if err != nil {
				t.Fatal(err)
			}
			if !hasMigrationConflict(items) {
				t.Fatal("mapping collision was not a conflict")
			}
			for kind, mapping := range mappings {
				seen := map[string]bool{}
				for _, id := range mapping {
					if seen[id] {
						t.Fatalf("%s maps two sources to %s", kind, id)
					}
					seen[id] = true
				}
			}
		})
	}
}
