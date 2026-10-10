// Package skills embeds the agent skills shipped with agent-nexus, one
// directory per skill in the Agent Skills layout (<name>/SKILL.md).
package skills

import "embed"

// FS holds the embedded skills.
//
//go:embed */SKILL.md
var FS embed.FS
