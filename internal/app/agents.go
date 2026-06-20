package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"
)

// agentSpec is the static catalogue entry for one supported agent.
// Detection and launch are described by data, not code, so adding
// Codex CLI / Gemini CLI later is just appending another entry to
// agentRegistry.
//
// Exactly one of AppName or Binary is set:
//   - AppName  → GUI agent shipped as a macOS .app bundle. Detected
//     under /Applications and ~/Applications, launched via
//     `open -a` so LaunchServices handles focus + single-
//     instance semantics.
//   - Binary   → CLI agent. Detected by walking a list of common
//     install locations + $PATH, launched in a fresh
//     Terminal window cd'd into the workspace via
//     AppleScript (`osascript`). The Terminal window
//     outlives whatskept just like the GUI case.
type agentSpec struct {
	ID          string // stable kebab-case identifier used in URLs
	Name        string // human-readable display name
	Description string // one-sentence pitch shown on the card
	AppName     string // GUI mode: ".app" basename under /Applications (empty for CLI)
	Binary      string // CLI mode: executable basename, e.g. "claude" (empty for GUI)

	// WinLauncher is the Windows CLI shim a GUI editor installs on PATH
	// (VS Code → "code", Cursor → "cursor", Windsurf → "windsurf"). On
	// Windows these editors aren't .app bundles, so detection/launch goes
	// through this command instead of AppName. Empty for CLI agents (which
	// use Binary cross-platform) and for GUI agents with no Windows shim.
	WinLauncher string

	// URLLaunch is a deep-link template used to open a GUI app that exposes a
	// custom URL scheme instead of a folder-argument launch (the Claude
	// desktop app: "claude://code/new?folder={folder}"). When set, the agent
	// is opened by handing the OS this URL (via openURLCmd) with {folder}
	// replaced by the url-encoded workspace path — not via `open -a`/Terminal.
	// Detection still keys off AppName (the .app bundle). Empty for agents
	// launched the ordinary way.
	URLLaunch string

	// IgnoreFile is the dotfile name this agent (or its inline assistant)
	// reads to know which workspace paths NOT to feed to a model. Written
	// into the workspace by postprocess.WriteAssets to keep multi-MB
	// `media/`, `voice/`, and `profiles/` trees out of token budgets.
	// Empty if the agent has no documented ignore mechanism.
	IgnoreFile string

	// InstallURL is the official download/install page, surfaced as a link
	// under the Agents-tab dropdown when the agent isn't installed.
	InstallURL string

	// Icon is the URL path to the agent's logo, served from the embedded
	// internal/app/web/icons/ directory and shown beside the name in the
	// Agents-tab dropdown. Empty renders no icon (name only).
	Icon string
}

// agentRegistry is the single source of truth for which agents the
// GUI knows about. Order is render order in the Agents tab.
var agentRegistry = []agentSpec{
	{
		ID:          "claude-desktop",
		Name:        "Claude",
		Description: "Anthropic's desktop app — opens this workspace in Claude Code.",
		AppName:     "Claude", // detect /Applications/Claude.app
		URLLaunch:   "claude://code/new?folder={folder}",
		InstallURL:  "https://claude.ai/download",
		Icon:        "/icons/claude.svg",
		// The read-only query workflow is pre-approved via the workspace's
		// .claude/settings.json permissions.allow list (written by
		// postprocess.WriteAssets) so the app doesn't prompt on every sqlite3
		// call — the desktop app ignores bypassPermissions from a project file.
		// Respects .gitignore.
	},
	{
		ID:          "codex",
		Name:        "Codex",
		Description: "OpenAI's coding agent — opens this workspace via the codex:// app.",
		AppName:     "Codex", // detect /Applications/Codex.app — confirm the bundle name
		URLLaunch:   "codex://new?path={folder}",
		InstallURL:  "https://developers.openai.com/codex/",
		Icon:        "/icons/codex.svg",
		// Approval prompts are pre-cleared via ~/.codex/config.toml
		// (approval_policy = "never", sandbox_mode), refreshed on every Sync by
		// postprocess.writeCodexConfig. That file is GLOBAL, not per-workspace.
	},
	{
		ID:          "vscode",
		Name:        "VS Code",
		Description: "Microsoft's editor — pair with GitHub Copilot or any AGENTS.md-aware extension.",
		AppName:     "Visual Studio Code",
		WinLauncher: "code",
		IgnoreFile:  ".copilotignore", // honoured by GitHub Copilot's content-exclusion plumbing
		InstallURL:  "https://code.visualstudio.com/Download",
		Icon:        "/icons/vscode.svg",
	},
	{
		ID:          "cursor",
		Name:        "Cursor",
		Description: "AI-first VS Code fork — Composer reads AGENTS.md and honours .cursorignore.",
		AppName:     "Cursor",
		WinLauncher: "cursor",
		IgnoreFile:  ".cursorignore",
		InstallURL:  "https://cursor.com/download",
		Icon:        "/icons/cursor.svg",
	},
}

// AgentIgnoreFiles returns the deduplicated list of dotfile names the
// known agents read to skip files. Consumed by postprocess.WriteAssets
// so the `media/`, `voice/`, and `profiles/` trees never make it into
// a model's token budget. Exported so the postprocess package can
// import it without depending on the rest of `app`.
func AgentIgnoreFiles() []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(agentRegistry))
	for _, a := range agentRegistry {
		if a.IgnoreFile == "" {
			continue
		}
		if _, dup := seen[a.IgnoreFile]; dup {
			continue
		}
		seen[a.IgnoreFile] = struct{}{}
		out = append(out, a.IgnoreFile)
	}
	return out
}

// agentInfo is what the React UI sees over JSON. Field naming uses
// snake_case to match the rest of the API surface.
type agentInfo struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	InstallURL  string `json:"install_url,omitempty"`
	Icon        string `json:"icon,omitempty"`
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

// describeAgents returns the JSON-ready view of the registry. We deliberately
// do NOT probe for installation: detection across install methods (Spotlight
// apps, Microsoft Store / MSIX packages, npm shims, custom URL schemes) is
// unreliable and produced both false negatives and stale false positives.
// Instead every agent is always offered, and handleOpenAgent surfaces a clear
// error (paired with the agent's install link in the UI) if the launch fails.
func describeAgents() []agentInfo {
	out := make([]agentInfo, 0, len(agentRegistry))
	for _, spec := range agentRegistry {
		out = append(out, agentInfo{
			ID:          spec.ID,
			Name:        spec.Name,
			Description: spec.Description,
			InstallURL:  spec.InstallURL,
			Icon:        spec.Icon,
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

// handleOpenAgent launches the requested agent with the given path as its
// working folder. There is no install pre-check: every agent is offered and we
// report a clear error if the launch fails. Two launch shapes:
//
//   - URL-scheme apps (Claude, Codex) are opened by handing the OS a deep link
//     carrying the workspace folder; openURLCmd routes custom schemes per
//     platform. (Note: on Windows the URL opener can't report a missing
//     handler — Windows shows its own "choose an app" dialog instead.)
//   - GUI editors (VS Code, Cursor) go through buildAgentCmd (agents_<os>.go):
//     `open -a <app>` on macOS, the editor's PATH shim on Windows.
//
// The launcher (open / rundll32 / cmd) hands off to the OS and exits promptly;
// the agent itself is detached and outlives whatskept. We wait briefly for the
// launcher and treat a non-zero exit as "app not found", echoing its stderr so
// the UI can pair the failure with the agent's install link.
func (s *server) handleOpenAgent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	spec := findAgent(id)
	if spec == nil {
		httpError(w, http.StatusNotFound, fmt.Sprintf("unknown agent %q", id))
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

	var cmd *exec.Cmd
	if spec.URLLaunch != "" {
		// The deep-link opener can't report a missing handler on every platform
		// (Windows rundll32 always exits 0), so check availability up front
		// there. Platforms whose opener DOES report failure (macOS `open`)
		// return true and we rely on the exit code below.
		scheme, _, _ := strings.Cut(spec.URLLaunch, "://")
		if !deepLinkAvailable(scheme) {
			httpError(w, http.StatusBadGateway, openFailMsg(spec.Name, ""))
			return
		}
		launchURL := strings.ReplaceAll(spec.URLLaunch, "{folder}", url.QueryEscape(target))
		cmd = openURLCmd(launchURL)
	} else {
		c, err := buildAgentCmd(*spec, target)
		if err != nil {
			httpError(w, http.StatusInternalServerError, openFailMsg(spec.Name, err.Error()))
			return
		}
		cmd = c
	}

	// Capture the launcher's stderr so a failure carries a useful reason
	// (e.g. macOS `open` prints "Unable to find application named 'Cursor'").
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		httpError(w, http.StatusInternalServerError, openFailMsg(spec.Name, err.Error()))
		return
	}

	// Wait briefly: a non-zero exit means the app / URL handler wasn't found.
	// If the launcher is still alive after the grace period (rare), assume
	// success and reap it in the background so we don't leak the process.
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			reason := strings.TrimSpace(stderr.String())
			if reason == "" {
				reason = err.Error()
			}
			httpError(w, http.StatusBadGateway, openFailMsg(spec.Name, reason))
			return
		}
	case <-time.After(3 * time.Second):
		go func() { <-done }()
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"agent":  spec.ID,
		"opened": target,
	})
}

// openFailMsg builds the user-facing error shown when an agent launch fails.
// The UI pairs this with the agent's "Install …" link.
func openFailMsg(name, reason string) string {
	msg := fmt.Sprintf("Couldn't open %s — it may not be installed.", name)
	if reason != "" {
		msg += " (" + reason + ")"
	}
	return msg
}


// handleOpenTerminal opens the workspace directory in macOS Terminal
// so the user can pick whatever CLI agent (claude, codex, etc.) they
// prefer. Same `open -a` mechanics as handleOpenAgent — detached, goes
// through LaunchServices, focuses an existing Terminal if running.
//
// Terminal.app ships with macOS at /System/Applications/Utilities/, so
// we don't bother with a detection step: `open -a Terminal <path>`
// either works or the user has done something exotic to their system,
// in which case the error from `open` is the right message to surface.
func (s *server) handleOpenTerminal(w http.ResponseWriter, r *http.Request) {
	var req openAgentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	target := strings.TrimSpace(req.Path)
	if target == "" {
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

	cmd, err := buildTerminalCmd(target)
	if err != nil {
		httpError(w, http.StatusInternalServerError, fmt.Sprintf("failed to launch terminal: %v", err))
		return
	}
	if err := cmd.Start(); err != nil {
		httpError(w, http.StatusInternalServerError, fmt.Sprintf("failed to launch terminal: %v", err))
		return
	}
	go cmd.Wait()

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"opened": target,
	})
}

// appleScriptQuote wraps s in double quotes for AppleScript string literals,
// escaping backslashes and double quotes. Order matters: backslashes must be
// doubled FIRST so the doubled escapes we add next aren't doubled again.
//
// Lives in shared code (not agents_darwin.go) because the macOS-only update
// handler in update.go also uses it; it's an inert pure string helper on
// non-macOS builds.
func appleScriptQuote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}
