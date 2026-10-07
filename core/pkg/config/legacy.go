package config

// Import conflict policy preserves the legacy archive/configsync modes.
func migrationAction(resource, id, alias string, conflicts bool, mode string) string {
	if !conflicts {
		return "create"
	}
	switch mode {
	case "overwrite":
		return "overwrite"
	case "fail_on_conflict":
		return "conflict"
	default:
		return "skip"
	}
}
func hasCurrentSettings(settings Settings) bool { return settings != defaultConfig().Settings }
func hasMigrationConflict(items []MigrationItem) bool {
	for _, item := range items {
		if item.Action == "conflict" || item.Action == "error" {
			return true
		}
	}
	return false
}
