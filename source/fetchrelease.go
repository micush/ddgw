package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The newest release fetched from GitHub, for Updates ▸ Upload a release when "Get the newest release from GitHub" is
// ticked.  It looks for the tag the way get.sh does: the newest published release, otherwise the highest v<number> tag,
// downloads that tag's source archive and hands it to the same staging as an uploaded file.  Variables only so tests can
// point them at a fake server.
var (
	ghRepo = "micush/ddgw"
	ghAPI  = "https://api.github.com"
	ghHost = "https://github.com"
)

const fetchTimeout = 3 * time.Minute

var (
	ghTagRe    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	ghNumTagRe = regexp.MustCompile(`^v?[0-9]+(\.[0-9]+)*$`)
)

func ghGet(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "ddgw/"+version())
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := (&http.Client{Timeout: fetchTimeout}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s answered %s", req.URL.Host, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("the download is larger than %d MB", limit>>20)
	}
	return b, nil
}

// numericTagLess orders v215 < v1000 and 1.2 < 1.10.
func numericTagLess(a, b string) bool {
	pa, pb := strings.Split(strings.TrimPrefix(a, "v"), "."), strings.Split(strings.TrimPrefix(b, "v"), ".")
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var x, y int
		if i < len(pa) {
			x, _ = strconv.Atoi(pa[i])
		}
		if i < len(pb) {
			y, _ = strconv.Atoi(pb[i])
		}
		if x != y {
			return x < y
		}
	}
	return false
}

// latestTag is the newest published release, or else the highest v<number> tag of the repository.
func latestTag(ctx context.Context) (string, error) {
	if b, err := ghGet(ctx, ghAPI+"/repos/"+ghRepo+"/releases/latest", 1<<20); err == nil {
		var r struct {
			Tag string `json:"tag_name"`
		}
		if json.Unmarshal(b, &r) == nil && r.Tag != "" && ghTagRe.MatchString(r.Tag) {
			return r.Tag, nil
		}
	}
	b, err := ghGet(ctx, ghAPI+"/repos/"+ghRepo+"/tags?per_page=100", 4<<20)
	if err != nil {
		return "", fmt.Errorf("cannot reach GitHub: %w", err)
	}
	var tags []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(b, &tags); err != nil {
		return "", errors.New("GitHub's answer was not understood")
	}
	best := ""
	for _, t := range tags {
		if ghNumTagRe.MatchString(t.Name) && (best == "" || numericTagLess(best, t.Name)) {
			best = t.Name
		}
	}
	if best == "" {
		return "", errors.New("no release or v<number> tag found in " + ghRepo + " (rate limit or no tags yet)")
	}
	return best, nil
}

// fetchRelease downloads the source archive of the newest tag.
func fetchRelease(ctx context.Context) (body []byte, tag string, err error) {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	tag, err = latestTag(ctx)
	if err != nil {
		return nil, "", err
	}
	body, err = ghGet(ctx, ghHost+"/"+ghRepo+"/archive/refs/tags/"+tag+".tar.gz", maxUploadBytes)
	if err != nil {
		return nil, tag, fmt.Errorf("download of %s failed: %w", tag, err)
	}
	return body, tag, nil
}

func (w *WebServer) handleUpdateFetch(rw http.ResponseWriter, r *http.Request, s *session) {
	body, tag, err := fetchRelease(r.Context())
	if err != nil {
		jsonError(rw, http.StatusUnprocessableEntity, err.Error())
		return
	}
	warnf("web: %q fetched release %s from GitHub (%d bytes, from %s)", s.user, tag, len(body), clientIP(r))
	ver, err := w.mg.UpdateUpload(body, s.user)
	if err != nil {
		jsonError(rw, http.StatusUnprocessableEntity, tag+": "+err.Error())
		return
	}
	writeJSON(rw, http.StatusOK, map[string]any{"ok": true, "data": map[string]any{"version": ver, "tag": tag}})
}
