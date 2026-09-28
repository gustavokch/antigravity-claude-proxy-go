// Package ccusage is a Go port of ccusage (https://github.com/ccusage/ccusage),
// which derives Claude Code usage, cost and billing windows from the JSONL
// transcripts Claude Code writes under its config directory.
//
// The port follows the Rust implementation at ccusage commit bbbb9a1. It is a
// pure library: it imports no proxy code, and callers inject pricing and
// supply the entries they want counted.
package ccusage
