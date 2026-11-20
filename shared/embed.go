// Package shared embeds the provider and toolkit catalog so the binary is self-contained.
package shared

import (
	"embed"
	"io/fs"
)

//go:embed providers toolkits
var catalog embed.FS

// FS holds the providers/ and toolkits/ directories.
var FS fs.FS = catalog
