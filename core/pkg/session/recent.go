package session

func recordUse(provider any, id string) {
	if p, ok := provider.(interface{ RecordUse(string) }); ok {
		p.RecordUse(id)
	}
}
