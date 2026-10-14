package web

import (
	"net/http"
	"testing"
)

func TestStaticFilesRevalidate(t *testing.T) {
	srv, _ := newTestServer(t, false)
	for _, p := range []string{"/", "/app.js", "/app.css"} {
		resp, err := http.Get(srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		etag := resp.Header.Get("ETag")
		if resp.StatusCode != 200 || resp.Header.Get("Cache-Control") != "no-cache" || etag == "" {
			t.Fatalf("%s: status %d cache-control %q etag %q", p, resp.StatusCode, resp.Header.Get("Cache-Control"), etag)
		}

		// Unchanged file: the browser's revalidation gets a cheap 304.
		req, _ := http.NewRequest(http.MethodGet, srv.URL+p, nil)
		req.Header.Set("If-None-Match", etag)
		resp, err = http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotModified {
			t.Fatalf("%s revalidate: %d", p, resp.StatusCode)
		}

		// A stale copy (old ETag) gets the current file.
		req.Header.Set("If-None-Match", `"stale"`)
		resp, _ = http.DefaultClient.Do(req)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("%s stale: %d", p, resp.StatusCode)
		}
	}
}

func TestClearCacheEndpoint(t *testing.T) {
	srv, _ := newTestServer(t, false)
	resp, err := http.Post(srv.URL+"/ui/clear-cache", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent || resp.Header.Get("Clear-Site-Data") != `"cache"` {
		t.Fatalf("status %d Clear-Site-Data %q", resp.StatusCode, resp.Header.Get("Clear-Site-Data"))
	}

	// Another site can't trigger it.
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/ui/clear-cache", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.Header.Set("Origin", "https://evil.example")
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || resp.Header.Get("Clear-Site-Data") != "" {
		t.Fatalf("cross-site: %d %q", resp.StatusCode, resp.Header.Get("Clear-Site-Data"))
	}
}
