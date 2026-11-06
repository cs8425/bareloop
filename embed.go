// Package bareloop is the module root, and it holds nothing but what goes inside the binary.
//
// The web UI is plain files in web/, compiled in below, so one file is a whole deployment: copy the
// binary to a box and the UI comes with it. The -web flag (and the config's "web" key) still points the
// server at a directory on disk, which is how you iterate on the UI without rebuilding — see
// docs/design.md, "Embedding the UI".
package bareloop

import "embed"

// WebUI is the web/ directory as it stood when this binary was built, addressed with a "web/" prefix
// (fs.Sub(bareloop.WebUI, "web") hands the server a filesystem rooted at the UI itself).
//
// The all: prefix is what also includes names starting with "." or "_"; nothing in web/ uses them
// today, and writing it is the habit that keeps that from becoming a surprise later.
//
//go:embed all:web
var WebUI embed.FS
