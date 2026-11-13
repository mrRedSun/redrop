// Package webui embeds the built React dashboard (web/ builds into dist/).
package webui

import "embed"

//go:embed all:dist
var Dist embed.FS
