package store

import _ "embed" // the seed book arrives as a file, not as source

// seedDict is the shipped zstd dictionary: the ir.Entry JSON skeleton — field names,
// nesting, the shared vocabulary — trained from the release corpus by
//
//	blimport -gendict -dictout internal/store/seeddict.bin <data-dir>
//
// and committed here beside this file, so every build carries the book its frames
// will name. Regenerating it is a release act, not a runtime one: frames already
// written stamp this book's id in their headers and the reader needs it forever, so
// a new book ships alongside the old data's dictionary, it does not replace it.
//
// The corpus experiment that priced the book (S8.7): every one of 17 stores — none
// of it trained on — came out 12.3% smaller with a held-out dictionary than with
// none at all, and not one store got worse.
//
//go:embed seeddict.bin
var seedDict []byte
