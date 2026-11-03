package daemon

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	_ "embed"
	"fmt"
	"net/http"
	"strings"
	"sync"
)

//go:embed ui/index.html
var indexHTML []byte

// indexGz and indexTag are computed once, on the first request: a panel that
// nobody opens costs nothing, and later requests only send ready bytes.
var indexGz = sync.OnceValues(func() ([]byte, string) {
	var b bytes.Buffer
	z, _ := gzip.NewWriterLevel(&b, gzip.BestCompression)
	z.Write(indexHTML)
	z.Close()
	return b.Bytes(), fmt.Sprintf(`"%x"`, sha256.Sum256(indexHTML))
})

func serveIndex(w http.ResponseWriter, r *http.Request) {
	gz, tag := indexGz()
	h := w.Header()
	h.Set("ETag", tag)
	h.Set("Cache-Control", "no-cache") // revalidate: a deploy must show at once
	h.Set("Vary", "Accept-Encoding")
	if r.Header.Get("If-None-Match") == tag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	h.Set("Content-Type", "text/html; charset=utf-8")
	if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		h.Set("Content-Encoding", "gzip")
		w.Write(gz)
		return
	}
	w.Write(indexHTML)
}
