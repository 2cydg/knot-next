// Package defaults owns fixed frontend values; language preferences live separately.
package defaults

import "path/filepath"

type Values struct {
	ForwardAgent   bool
	ClearOnConnect bool
	LocalDirectory string
}

func FromStartupDirectory(directory string) Values {
	return Values{
		ForwardAgent:   true,
		ClearOnConnect: true,
		LocalDirectory: filepath.Clean(directory),
	}
}
