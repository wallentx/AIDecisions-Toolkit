// Package aid embeds the tool catalog without linking tool implementations into
// the launcher. Adding a tool manifest and skill registers it at build time.
package aid

import "embed"

//go:embed tools/*/tool.json tools/*/SKILL.md
var Tools embed.FS

//go:embed TOOL-CRITERIA.md
var ToolCriteria []byte
