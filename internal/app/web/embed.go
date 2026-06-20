// Package web embeds the static frontend assets.
//
// The vendor/ subdirectory contains pinned copies of React 18,
// ReactDOM 18, Babel-standalone (for in-browser JSX), and the Tailwind
// Play CDN runtime. Vendoring keeps the app self-contained so it works
// fully offline (no fetch from unpkg.com or cdn.tailwindcss.com on
// startup).
package web

import "embed"

//go:embed index.html vendor logo.png icons
var FS embed.FS
