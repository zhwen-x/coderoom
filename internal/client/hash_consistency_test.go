package client_test

import (
	"testing"

	"coderoom/internal/client"
	"coderoom/internal/store"
)

// TestHashAgreesWithServer guards the one piece of logic that is deliberately
// duplicated on both sides of the wire. If these ever diverge, the server
// would store every snapshot as "changed" and reject every submission as
// tampered.
func TestHashAgreesWithServer(t *testing.T) {
	for _, code := range []string{
		"", "int main(){}", "print('你好')", "a\r\nb\n", "x" + string(rune(0)) + "y",
	} {
		if client.HashCode(code) != store.HashCode(code) {
			t.Errorf("hash mismatch for %q: client=%s server=%s",
				code, client.HashCode(code), store.HashCode(code))
		}
	}
	for _, lang := range []string{"cpp", "c", "python", "weird"} {
		if client.SourceExt(lang) != store.SourceExt(lang) {
			t.Errorf("extension mismatch for %q: client=%s server=%s",
				lang, client.SourceExt(lang), store.SourceExt(lang))
		}
	}
}
