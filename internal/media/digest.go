package media

import (
	"crypto/sha256"
	"encoding/hex"
	"hash"
	"io"
)

func DigestBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func CombineDigest(parts ...string) string {
	hash := sha256.New()
	for _, part := range parts {
		hash.Write([]byte(part))
		hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil))
}

// HashWriter passes every written byte through to the wrapped writer while
// accumulating a SHA-256 digest and byte count. It lets the service compute
// the same digests it used to compute on finished buffers while streaming
// output directly to its destination.
type HashWriter struct {
	writer io.Writer
	hash   hash.Hash
	count  int64
}

func NewHashWriter(writer io.Writer) *HashWriter {
	return &HashWriter{writer: writer, hash: sha256.New()}
}

func (w *HashWriter) Write(p []byte) (int, error) {
	n, err := w.writer.Write(p)
	if n > 0 {
		w.hash.Write(p[:n])
		w.count += int64(n)
	}
	return n, err
}

// Digest returns the hex SHA-256 of every byte written so far. It matches
// DigestBytes over the equivalent buffered content.
func (w *HashWriter) Digest() string {
	return hex.EncodeToString(w.hash.Sum(nil))
}

// Bytes returns the number of bytes written so far.
func (w *HashWriter) Bytes() int64 {
	return w.count
}
