package mcpserve

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// A server left on an old release keeps answering, so nothing tells
// its user a newer one exists. The server asks GitHub — at start and
// hourly after — and, while a newer release is out, ends every tool
// result with a notice for the agent to pass on. The request carries
// nothing about the user; a failed check changes nothing.

// releaseURL is GitHub's latest-release endpoint for this project.
var releaseURL = "https://api.github.com/repos/alkait/whatskept/releases/latest"

// latestRelease is the newest release tag GitHub last reported.
var latestRelease atomic.Value

// watchReleases checks for a newer release now and every hour until
// ctx is done. A source build has no release to compare with.
func watchReleases(ctx context.Context) {
	if Version == "dev" {
		return
	}
	for {
		checkRelease(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Hour):
		}
	}
}

// checkRelease asks GitHub for the latest release and remembers it,
// logging the first sight of each newer one.
func checkRelease(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, releaseURL, nil)
	if err != nil {
		return
	}
	req.Header.Set("User-Agent", "whatskept")
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	var release struct {
		Tag string `json:"tag_name"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&release) != nil || release.Tag == "" {
		return
	}
	if old, _ := latestRelease.Swap(release.Tag).(string); old != release.Tag && newer(release.Tag, Version) {
		fmt.Fprintf(os.Stderr, "Update:        %s is available (running %s)\n", release.Tag, Version)
	}
}

// newerRelease returns the release newer than the one running, or "".
func newerRelease() string {
	if latest, _ := latestRelease.Load().(string); newer(latest, Version) {
		return latest
	}
	return ""
}

// newer reports whether version a is later than b — dotted numbers
// after an optional v. Anything else is never newer.
func newer(a, b string) bool {
	x, y := versionNumbers(a), versionNumbers(b)
	if x == nil || y == nil {
		return false
	}
	for i := 0; i < len(x) && i < len(y); i++ {
		if x[i] != y[i] {
			return x[i] > y[i]
		}
	}
	return len(x) > len(y)
}

func versionNumbers(v string) []int {
	var out []int
	for _, part := range strings.Split(strings.TrimPrefix(v, "v"), ".") {
		n, err := strconv.Atoi(part)
		if err != nil {
			return nil
		}
		out = append(out, n)
	}
	return out
}
