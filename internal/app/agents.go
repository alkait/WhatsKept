package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
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

	// IgnoreFile is the dotfile name this agent (or its inline assistant)
	// reads to know which workspace paths NOT to feed to a model. Written
	// into the workspace by postprocess.WriteAssets to keep multi-MB
	// `media/`, `voice/`, and `profiles/` trees out of token budgets.
	// Empty if the agent has no documented ignore mechanism.
	IgnoreFile string
}

// agentRegistry is the single source of truth for which agents the
// GUI knows about. Order is render order in the Agents tab.
var agentRegistry = []agentSpec{
	{
		ID:          "windsurf",
		Name:        "Windsurf",
		Description: "Cascade-powered IDE that reads AGENTS.md natively.",
		AppName:     "Windsurf",
		WinLauncher: "windsurf",
		IgnoreFile:  ".windsurfignore",
	},
	{
		ID:          "vscode",
		Name:        "VS Code",
		Description: "Microsoft's editor — pair with GitHub Copilot or any AGENTS.md-aware extension.",
		AppName:     "Visual Studio Code",
		WinLauncher: "code",
		IgnoreFile:  ".copilotignore", // honoured by GitHub Copilot's content-exclusion plumbing
	},
	{
		ID:          "cursor",
		Name:        "Cursor",
		Description: "AI-first VS Code fork — Composer reads AGENTS.md and honours .cursorignore.",
		AppName:     "Cursor",
		WinLauncher: "cursor",
		IgnoreFile:  ".cursorignore",
	},
	{
		ID:          "claude-code",
		Name:        "Claude Code",
		Description: "Anthropic's terminal-native coding agent — reads CLAUDE.md from the workspace root.",
		Binary:      "claude",
		// No documented ignore-file convention; Claude Code respects .gitignore by default.
	},
	{
		ID:          "opencode",
		Name:        "opencode",
		Description: "Open-source terminal coding agent — reads AGENTS.md natively, BYO model provider.",
		Binary:      "opencode",
		// No documented ignore-file convention; opencode honours .gitignore by default.
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

// detectAgent returns (true, path) if the agent is installed. For GUI
// agents, path is the launch target (the .app bundle on macOS, the resolved
// CLI shim on Windows); for CLI agents, the resolved executable. Returns
// (false, "") if neither AppName nor Binary is set, or nothing was found.
//
// detectGUIAgent / detectCLIAgent are platform-specific (agents_<os>.go):
// where an agent lives and how it's launched differs by OS.
func detectAgent(spec agentSpec) (bool, string) {
	if spec.AppName != "" || spec.WinLauncher != "" {
		return detectGUIAgent(spec)
	}
	if spec.Binary != "" {
		return detectCLIAgent(spec)
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
// as its working folder. The launch mechanism depends on the agent
// kind (see agentSpec):
//
//   - GUI agents are launched via `/usr/bin/open -a <app> <path>` so
//     LaunchServices handles focus + single-instance semantics; a
//     bare exec on the bundle's MacOS binary would spawn a duplicate
//     instance and inherit our minimal environment.
//   - CLI agents are launched in a fresh Terminal window via
//     `/usr/bin/osascript` running a `tell application "Terminal"`
//     block that `cd`s into the workspace and `exec`s the binary.
//     We can't use `open -a Terminal <path>` because that only opens
//     the directory — it doesn't run the agent.
//
// In both cases the launched process is detached so the agent
// outlives whatskept.
func (s *server) handleOpenAgent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	spec := findAgent(id)
	if spec == nil {
		httpError(w, http.StatusNotFound, fmt.Sprintf("unknown agent %q", id))
		return
	}

	installed, resolved := detectAgent(*spec)
	if !installed {
		var where string
		switch {
		case spec.AppName != "":
			where = "/Applications and ~/Applications"
		case spec.Binary != "":
			where = "$PATH and the standard CLI install locations"
		default:
			where = "the standard install locations"
		}
		httpError(w, http.StatusNotFound, fmt.Sprintf("%s is not installed (looked under %s)", spec.Name, where))
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

	// Build the platform-specific launch command (GUI editor vs CLI agent
	// in a terminal). buildAgentCmd lives in agents_<os>.go because the
	// mechanics differ entirely per OS (LaunchServices + AppleScript on
	// macOS; the editor's CLI shim + Windows Terminal/cmd on Windows). The
	// launched process is detached so the agent outlives whatskept.
	cmd, err := buildAgentCmd(*spec, resolved, target)
	if err != nil {
		httpError(w, http.StatusInternalServerError, fmt.Sprintf("failed to launch %s: %v", spec.Name, err))
		return
	}
	if err := cmd.Start(); err != nil {
		httpError(w, http.StatusInternalServerError, fmt.Sprintf("failed to launch %s: %v", spec.Name, err))
		return
	}
	go cmd.Wait() // reap the child; ignore result

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"agent":    spec.ID,
		"app_path": resolved,
		"opened":   target,
	})
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
