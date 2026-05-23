package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// agentSpec is the static catalogue entry for one supported agent.
// Detection and launch are described by data, not code, so adding
// Cursor / Claude Code / Codex CLI later is just appending another
// entry to agentRegistry.
type agentSpec struct {
	ID          string // stable kebab-case identifier used in URLs
	Name        string // human-readable display name
	Description string // one-sentence pitch shown on the card
	AppName     string // ".app" basename to look for under /Applications
}

// agentRegistry is the single source of truth for which agents the
// GUI knows about. Order is render order in the Agents tab.
var agentRegistry = []agentSpec{
	{
		ID:          "windsurf",
		Name:        "Windsurf",
		Description: "Cascade-powered IDE that reads AGENTS.md natively.",
		AppName:     "Windsurf",
	},
}

// agentInfo is what the React UI sees over JSON. Field naming uses
// snake_case to match the rest of the API surface.
type agentInfo struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Installed   bool   `json:"installed"`
	AppPath     string `json:"app_path,omitempty"`
}

// findAgent looks up a registry entry by ID.
func findAgent(id string) *agentSpec {
	for i := range agentRegistry {
		if agentRegistry[i].ID == id {
			return &agentRegistry[i]
		}
	}
	return nil
}

// detectAgent returns (true, path) if the agent's .app bundle is
// found in any of the standard macOS install locations. Checks the
// system-wide /Applications first, then the user-local
// ~/Applications, mirroring Spotlight's order.
func detectAgent(spec agentSpec) (bool, string) {
	candidates := []string{
		filepath.Join("/Applications", spec.AppName+".app"),
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, "Applications", spec.AppName+".app"))
	}
	for _, p := range candidates {
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			return true, p
		}
	}
	return false, ""
}

// describeAgents returns the JSON-ready view of the registry with
// live detection status filled in.
func describeAgents() []agentInfo {
	out := make([]agentInfo, 0, len(agentRegistry))
	for _, spec := range agentRegistry {
		installed, path := detectAgent(spec)
		out = append(out, agentInfo{
			ID:          spec.ID,
			Name:        spec.Name,
			Description: spec.Description,
			Installed:   installed,
			AppPath:     path,
		})
	}
	return out
}

// ---------------------------------------------------------------------------
// HTTP handlers
// ---------------------------------------------------------------------------

func (s *server) handleListAgents(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, describeAgents())
}

type openAgentRequest struct {
	Path string `json:"path"`
}

// handleOpenAgent launches the requested agent with the given path
// as its working folder. We shell out to `/usr/bin/open -a <app>
// <path>` rather than spawning the binary directly because:
//
//   - `open` goes through macOS LaunchServices, which (a) brings the
//     app to focus if it's already running, (b) handles single-
//     instance apps correctly, and (c) doesn't tie the agent's
//     lifetime to whatskept (we exit, the agent keeps running).
//   - A bare exec.Command("/Applications/Windsurf.app/Contents/MacOS/Windsurf")
//     would launch a *new* instance every time and inherit our
//     environment, both of which are wrong for an editor.
func (s *server) handleOpenAgent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	spec := findAgent(id)
	if spec == nil {
		httpError(w, http.StatusNotFound, fmt.Sprintf("unknown agent %q", id))
		return
	}

	installed, appPath := detectAgent(*spec)
	if !installed {
		httpError(w, http.StatusNotFound, fmt.Sprintf("%s is not installed (looked under /Applications and ~/Applications)", spec.Name))
		return
	}

	var req openAgentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	target := strings.TrimSpace(req.Path)
	if target == "" {
		// Fall back to the currently selected workspace.
		if cur := s.ws.get(); cur != "" {
			target = cur
		} else {
			httpError(w, http.StatusBadRequest, "path is required and no workspace is currently selected")
			return
		}
	}
	if _, err := os.Stat(target); err != nil {
		httpError(w, http.StatusBadRequest, fmt.Sprintf("path does not exist: %s", target))
		return
	}

	// Detached: open returns immediately once LaunchServices has the
	// request. We don't Wait() because we don't care about the agent's
	// exit code — it'll outlive whatskept.
	cmd := exec.Command("/usr/bin/open", "-a", appPath, target)
	if err := cmd.Start(); err != nil {
		httpError(w, http.StatusInternalServerError, fmt.Sprintf("failed to launch %s: %v", spec.Name, err))
		return
	}
	go cmd.Wait() // reap the child; ignore result

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"agent":    spec.ID,
		"app_path": appPath,
		"opened":   target,
	})
}
