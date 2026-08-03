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

// etagForBlob derives the tag a restored version gets.
//
// The version row records the blob id, and a blob row is content-addressed — one
// row per SHA-256 — so the id identifies the bytes. Hashing it into the same shape
// as ETagForHash keeps the "an ETag is a quoted opaque string" contract intact for
// clients that follow it literally.
func etagForBlob(blobID string) string {
	var h [32]byte
	copy(h[:], blobID)
	return ETagForHash(h)
}
