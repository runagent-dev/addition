// Package shared embeds the provider and toolkit catalog and the agent guide so the binary is self-contained.
package shared

import (
	"embed"
	"io/fs"
)

//go:embed providers toolkits operations guide
var data embed.FS

// FS holds the providers/, toolkits/ and operations/ directories.
var FS fs.FS = data

// Guide holds the agent guide: one Markdown file per topic, recipes under apps/.
var Guide = mustSub(data, "guide")

func mustSub(fsys fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(fsys, dir)
	if err != nil {
		panic(err)
	}
	return sub
}
