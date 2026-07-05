package webchat

import "embed"

//go:embed web/src/chat.js
var assetsFS embed.FS

// AssetFS exposes the bundled chat.js (Alpine component + markdown
// processor, concatenated) so hosts can serve it from their own asset
// pipeline if they prefer. [Server.Mount] already wires it up at
// {prefix}/assets/chat.js — most hosts just use that.
//
// The example template (webchat/examples/chat.html) is intentionally
// NOT embedded or served by webchat. Hosts copy it into their own
// template tree (where their build tools can scan it) and serve it
// themselves. See README "Mounting" section.
var AssetFS = assetsFS
