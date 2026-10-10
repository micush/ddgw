package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestNumericTagLess(t *testing.T) {
	if !numericTagLess("v215", "v1000") || numericTagLess("v1000", "v215") || !numericTagLess("1.2", "1.10") || numericTagLess("v5", "v5") {
		t.Fatal("tags are not ordered by number")
	}
}

// fakeGitHub serves the two places fetchRelease asks: the API (release/tag lists) and the archive.
func fakeGitHub(t *testing.T, release string, tags string, archives map[string][]byte) *atomic.Int32 {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		switch {
		case r.URL.Path == "/repos/micush/ddgw/releases/latest":
			if release == "" {
				http.NotFound(w, r)
				return
			}
			w.Write([]byte(`{"tag_name":"` + release + `"}`))
		case r.URL.Path == "/repos/micush/ddgw/tags":
			w.Write([]byte(tags))
		case strings.HasPrefix(r.URL.Path, "/micush/ddgw/archive/refs/tags/"):
			tag := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/micush/ddgw/archive/refs/tags/"), ".tar.gz")
			if b, ok := archives[tag]; ok {
				w.Write(b)
				return
			}
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	oa, oh := ghAPI, ghHost
	ghAPI, ghHost = srv.URL, srv.URL
	t.Cleanup(func() { srv.Close(); ghAPI, ghHost = oa, oh })
	return &n
}

func TestLatestTag(t *testing.T) {
	fakeGitHub(t, "v300", `[]`, nil)
	if tag, err := latestTag(context.Background()); err != nil || tag != "v300" {
		t.Fatalf("release: %q %v", tag, err)
	}
	fakeGitHub(t, "", `[{"name":"v99"},{"name":"v1000"},{"name":"nightly"},{"name":"v215"}]`, nil)
	if tag, err := latestTag(context.Background()); err != nil || tag != "v1000" {
		t.Fatalf("tags: %q %v", tag, err)
	}
	fakeGitHub(t, "", `[{"name":"nightly"}]`, nil)
	if _, err := latestTag(context.Background()); err == nil {
		t.Fatal("no usable tag was accepted")
	}
}

func TestUpdateFetchFromGitHub(t *testing.T) {
	e := newWebEnv(t)
	e.login("alice", "pw")
	fakeGitHub(t, "v7", `[]`, map[string][]byte{"v7": fakeSourceTarball(t, "7")})
	r := e.do("POST", "/api/update/fetch", map[string]any{}, withAuth(e, true))
	if r.code != 200 {
		t.Fatalf("fetch: %d %s", r.code, r.raw)
	}
	if d := r.body["data"].(map[string]any); d["version"] != "7" || d["tag"] != "v7" {
		t.Fatalf("fetch answer: %s", r.raw)
	}
	st := e.do("GET", "/api/update", nil, withAuth(e, false))
	if st.body["data"].(map[string]any)["source_version"] != "7" {
		t.Fatalf("not staged: %s", st.raw)
	}
	// GitHub unreachable or the tag missing: a clear error, nothing staged over the good one
	fakeGitHub(t, "v8", `[]`, nil)
	if r := e.do("POST", "/api/update/fetch", map[string]any{}, withAuth(e, true)); r.code != 422 || !strings.Contains(string(r.raw), "v8") {
		t.Fatalf("missing archive: %d %s", r.code, r.raw)
	}
	fakeGitHub(t, "v9", `[]`, map[string][]byte{"v9": []byte("not an archive")})
	if r := e.do("POST", "/api/update/fetch", map[string]any{}, withAuth(e, true)); r.code != 422 {
		t.Fatalf("junk archive: %d %s", r.code, r.raw)
	}
}
