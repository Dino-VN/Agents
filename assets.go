// Package agentd embeds the default soul, role prompts and skills so a fresh
// install (`agentd setup`) can create ~/.agentd without the source tree.
package agentd

import "embed"

//go:embed souls roles skills
var Defaults embed.FS
