package cachebump

import "bytes"

// bytesEqual reports whether two recorded bodies are identical. Test-only
// helper kept out of record.go so production code carries no test kit.
func bytesEqual(a, b []byte) bool {
	return bytes.Equal(a, b)
}
