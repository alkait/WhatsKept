// Package web embeds the static frontend assets.
package web

import "embed"

//go:embed index.html
var FS embed.FS
