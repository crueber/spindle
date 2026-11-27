package actions

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// makeTar builds a gzip tarball with the given root prefix and files.
func makeTar(t *testing.T, root string, files map[string]string) []byte {
	t.Helper()
	var entries []tarEntry
	for name, body := range files {
		entries = append(entries, tarEntry{name: root + name, body: body})
	}
	return makeTarRaw(t, entries)
}

type tarEntry struct {
	name     string
	body     string
	link     string
	typeflag byte
}

func makeTarRaw(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		flag := e.typeflag
		if flag == 0 {
			flag = tar.TypeReg
		}
		hdr := &tar.Header{Name: e.name, Mode: 0o644, Typeflag: flag, Size: int64(len(e.body))}
		if flag == tar.TypeSymlink {
			hdr.Linkname = e.link
			hdr.Size = 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if flag == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// newTLSServerBytes serves one body for every request (archive tests).
func newTLSServerBytes(t *testing.T, body []byte) *httptest.Server {
	t.Helper()
	return httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(body)
	}))
}
