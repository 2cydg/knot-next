// Package defaults owns this release's fixed frontend values, without a preferences file.
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
