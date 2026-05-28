package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// repoSlug is the GitHub owner/name the app self-updates from. It must
// match REPO in build/install.sh — the installer we re-run on update —
// and the release-asset URLs baked into .github/workflows/release.yml.
const repoSlug = "alkait/WhatsKept"

// installerURL is the canonical curl|bash installer. releases/latest
// always resolves to the newest *tagged* (non-pre-release) release, so
// this URL never goes stale. curl-downloaded files don't carry the
// com.apple.quarantine xattr, which is the whole reason the installer
// is the supported update path (see build/install.sh).
const installerURL = "https://github.com/" + repoSlug + "/releases/latest/download/install.sh"

// localDevVersion is the ldflags default baked into a plain `make build`
// with no VERSION= override. We treat it as "not a distributed build"
// and never nag it about updates — a developer running their own build
// shouldn't see an "update available" pill for every published release.
// CI dev builds differ (0.0.0-dev.<run>+<sha>) and do get prompted.
const localDevVersion = "0.0.0-dev"

// handleMeta returns the running build's version and source repo. No
// network access — the UI calls this on mount to render the version
// label even when GitHub is unreachable.
func (s *server) handleMeta(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"version": s.version,
		"repo":    repoSlug,
	})
}

// updateCheckResponse is the shape the UI consumes. On any failure to
// reach or parse the GitHub release we still return 200 with
// update_available=false and a populated `error`, so the UI stays quiet
// (shows the plain version label) rather than surfacing a scary banner
// for what is, after all, an optional background check.
type updateCheckResponse struct {
	Current         string `json:"current"`
	Latest          string `json:"latest"`
	UpdateAvailable bool   `json:"update_available"`
	NotesURL        string `json:"notes_url"`
	Error           string `json:"error,omitempty"`
}

func (s *server) handleUpdateCheck(w http.ResponseWriter, r *http.Request) {
	resp := updateCheckResponse{Current: s.version}

	// A plain local build never resolves to a distributable artifact, so
	// there's nothing to update it to. Skip the network call entirely.
	if s.version == localDevVersion || s.version == "" {
		writeJSON(w, http.StatusOK, resp)
		return
	}

	tag, notesURL, err := latestRelease(r.Context())
	if err != nil {
		resp.Error = err.Error()
		writeJSON(w, http.StatusOK, resp)
		return
	}
	resp.Latest = tag
	resp.NotesURL = notesURL
	resp.UpdateAvailable = semverLess(s.version, tag)
	writeJSON(w, http.StatusOK, resp)
}

// latestRelease asks the GitHub API for the most recent published,
// non-pre-release release. The /releases/latest endpoint already
// excludes the rolling `dev-*` pre-releases, so this only ever reports
// stable tags. Returns (tag, html_url, err).
func latestRelease(ctx context.Context) (string, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()

	url := "https://api.github.com/repos/" + repoSlug + "/releases/latest"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("contact GitHub: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("GitHub returned %s", res.Status)
	}

	var body struct {
		TagName string `json:"tag_name"`
		HTMLURL string `json:"html_url"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		return "", "", fmt.Errorf("decode release: %w", err)
	}
	if body.TagName == "" {
		return "", "", fmt.Errorf("release has no tag")
	}
	return body.TagName, body.HTMLURL, nil
}

// handleUpdateRun opens a Terminal window running the official installer
// and returns immediately. The UI quits the app right after (the running
// binary can't replace itself, and the installer relaunches us when it
// finishes — see build/install.sh). We run it visibly in Terminal rather
// than in the background because once the app quits there's no UI left to
// show progress; the installer prints its own steps and the relaunch.
func (s *server) handleUpdateRun(w http.ResponseWriter, _ *http.Request) {
	// `clear` keeps the window tidy on a reused shell; the heredoc-free
	// curl|bash is the exact command the README documents.
	shellCmd := "clear && echo 'Updating WhatsKept — this window will close itself shortly.' && " +
		"/bin/bash -c \"$(curl -fsSL " + installerURL + ")\""
	quoted := appleScriptQuote(shellCmd)

	// Same cold-launch window-reuse dance as handleOpenAgent: if Terminal
	// isn't already running, the `tell` block launches it and it opens an
	// empty default-profile window; reuse that window instead of leaving
	// it orphaned beside a second one.
	ascript := "tell application \"Terminal\"\n" +
		"set wasRunning to running\n" +
		"activate\n" +
		"if wasRunning then\n" +
		"do script " + quoted + "\n" +
		"else\n" +
		"repeat 20 times\n" +
		"if (count of windows) > 0 then exit repeat\n" +
		"delay 0.05\n" +
		"end repeat\n" +
		"if (count of windows) > 0 then\n" +
		"do script " + quoted + " in window 1\n" +
		"else\n" +
		"do script " + quoted + "\n" +
		"end if\n" +
		"end if\n" +
		"end tell"

	cmd := exec.Command("/usr/bin/osascript", "-e", ascript)
	if err := cmd.Start(); err != nil {
		httpError(w, http.StatusInternalServerError, fmt.Sprintf("failed to launch installer: %v", err))
		return
	}
	go cmd.Wait() // reap; it outlives us

	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleOpenURL opens an https URL in the user's default browser via
// LaunchServices. The UI uses it for the "What's new" release-notes link
// — a bare <a> would navigate the WKWebView itself away from the app. We
// only allow https so a malformed request can't be coerced into opening
// a file:// or arbitrary-scheme handler.
func (s *server) handleOpenURL(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if !strings.HasPrefix(req.URL, "https://") {
		httpError(w, http.StatusBadRequest, "only https URLs may be opened")
		return
	}
	cmd := exec.Command("/usr/bin/open", req.URL)
	if err := cmd.Start(); err != nil {
		httpError(w, http.StatusInternalServerError, fmt.Sprintf("failed to open URL: %v", err))
		return
	}
	go cmd.Wait()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// semverLess reports whether version `a` is strictly older than `b`,
// comparing only the MAJOR.MINOR.PATCH core. A leading "v" and any
// -prerelease / +build suffix are ignored, which is exactly what we want
// here: every published release is a clean stable tag (vX.Y.Z), and a dev
// build like 0.0.0-dev.12+sha should compare as its 0.0.0 core — below
// any real release. Unparseable input compares as not-less (stay quiet).
func semverLess(a, b string) bool {
	am, an, ap, ok1 := semverCore(a)
	bm, bn, bp, ok2 := semverCore(b)
	if !ok1 || !ok2 {
		return false
	}
	switch {
	case am != bm:
		return am < bm
	case an != bn:
		return an < bn
	default:
		return ap < bp
	}
}

// semverCore extracts the three numeric fields from a version string,
// tolerating a leading "v" and trailing -prerelease / +build metadata.
func semverCore(v string) (maj, min, pat int, ok bool) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return 0, 0, 0, false
	}
	var err error
	if maj, err = strconv.Atoi(parts[0]); err != nil {
		return 0, 0, 0, false
	}
	if min, err = strconv.Atoi(parts[1]); err != nil {
		return 0, 0, 0, false
	}
	if pat, err = strconv.Atoi(parts[2]); err != nil {
		return 0, 0, 0, false
	}
	return maj, min, pat, true
}
