package storage

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
)

// digestStream reads r to completion and returns the number of bytes read
// and their hex SHA-256 digest. It produces the same values as hashing a
// fully buffered copy, without holding the content in memory.
func digestStream(r io.Reader) (int64, string, error) {
	hash := sha256.New()
	count, err := io.Copy(hash, r)
	if err != nil {
		return 0, "", err
	}
	return count, hex.EncodeToString(hash.Sum(nil)), nil
}
