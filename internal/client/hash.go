package client

import (
	"crypto/sha256"
	"encoding/hex"

	"coderoom/internal/protocol"
)

// HashCode must match the server's store.HashCode: the hash is what lets the
// server skip unchanged snapshots and detect a tampered submission body.
func HashCode(code string) string {
	sum := sha256.Sum256([]byte(code))
	return hex.EncodeToString(sum[:])
}

// SourceExt is the file extension for a language, used when exporting answers.
func SourceExt(lang string) string {
	switch lang {
	case protocol.LangC:
		return "c"
	case protocol.LangPython:
		return "py"
	default:
		return "cpp"
	}
}
