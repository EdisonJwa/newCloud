package store

import "fmt"

// ETagForHash derives a strong entity tag from a content address.
//
// It lives in the metadata package because the ETag is a property of the row, not
// of the transport: every writer stores it and every reader compares it. There is
// one definition so a browser upload and a WebDAV PUT cannot disagree about what
// "the same content" means.
func ETagForHash(h [32]byte) string {
	return fmt.Sprintf(`"%x"`, h[:8])
}
