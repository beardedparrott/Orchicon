package main

import (
	"compress/gzip"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// spa_compression_test.go — THE UI MUST NOT ARRIVE UNCOMPRESSED.
//
// REPORTED: "refreshing a page takes several seconds of staring at a blank canvas and then all items load
// in". The frontend is one ~2.8 MB JavaScript bundle, and the plane served it RAW: http.FileServer over an
// embed.FS has no encoder, and it ignored Accept-Encoding entirely.
//
// This measures what a browser actually receives — negotiated encoding, the header a shared cache needs, and
// a Content-Type that survives gzip (a sniffed gzip lands as application/x-gzip and the browser refuses to
// execute the script).
func TestSPAServesGzippedAssets(t *testing.T) {
	h := withFrontend(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
	}), slog.New(slog.DiscardHandler))

	srv := httptest.NewServer(h)
	defer srv.Close()

	// Find the bundle's real name from the served index.
	res, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	idx, _ := io.ReadAll(res.Body)
	res.Body.Close()
	i := strings.Index(string(idx), "/assets/index-")
	if i < 0 {
		t.Skip("this build embeds no frontend")
	}
	rest := string(idx)[i:]
	bundle := rest[:strings.IndexByte(rest, '"')]

	// WITH gzip — what every browser sends.
	req, _ := http.NewRequest("GET", srv.URL+bundle, nil)
	req.Header.Set("Accept-Encoding", "gzip, deflate, br")
	res2, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res2.Body.Close()
	if got := res2.Header.Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip — the bundle ships uncompressed, which is the reported "+
			"slow load", got)
	}
	if !strings.Contains(res2.Header.Get("Vary"), "Accept-Encoding") {
		t.Error("no Vary: Accept-Encoding — a shared cache can hand gzipped bytes to a client that did not ask")
	}
	if ct := res2.Header.Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Errorf("Content-Type = %q, want javascript — a sniffed gzip lands as application/x-gzip and the "+
			"browser refuses to execute it", ct)
	}
	gz, _ := io.ReadAll(res2.Body)
	zr, err := gzip.NewReader(strings.NewReader(string(gz)))
	if err != nil {
		t.Fatalf("the body is not valid gzip: %v", err)
	}
	plain, _ := io.ReadAll(zr)
	if len(plain) < len(gz) {
		t.Fatalf("decompressed %d bytes from %d compressed — the encoding is wrong", len(plain), len(gz))
	}
	t.Logf("bundle %s: %d raw -> %d gzipped (%.0f%% smaller)",
		bundle, len(plain), len(gz), 100-float64(len(gz))*100/float64(len(plain)))

	// WITHOUT gzip — must still be the raw bytes, not gzip mislabelled.
	res3, err := http.Get(srv.URL + bundle)
	if err != nil {
		t.Fatal(err)
	}
	defer res3.Body.Close()
	if got := res3.Header.Get("Content-Encoding"); got != "" {
		t.Errorf("a client that did not offer gzip got Content-Encoding %q", got)
	}
}
