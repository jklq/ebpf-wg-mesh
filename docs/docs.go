// Package docs embeds the platform documentation so binaries that run
// outside the repository (the MCP server) can serve it for search and
// reading.
package docs

import "embed"

//go:embed *.md
var FS embed.FS
