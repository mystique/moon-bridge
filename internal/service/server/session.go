package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"moonbridge/internal/service/api"
	"moonbridge/internal/session"
)

func (server *Server) ListSessions() []api.SessionInfo {
	if server.sessionManager == nil {
		return nil
	}
	return server.sessionManager.List()
}

func (server *Server) sessionForRequest(request *http.Request) *session.Session {
	if server.sessionManager == nil {
		return nil
	}
	key := sessionKeyFromRequest(request)
	if key == "" {
		return server.sessionManager.NewEphemeral()
	}
	return server.sessionManager.GetOrCreate(key, time.Now())
}

func sessionKeyFromRequest(request *http.Request) string {
	if value := strings.TrimSpace(request.Header.Get("Session_id")); value != "" {
		return "session:" + value
	}
	if value := strings.TrimSpace(request.Header.Get("X-Codex-Window-Id")); value != "" {
		return "codex-window:" + value
	}
	// Newer Codex clients no longer send the standalone X-Codex-Window-Id
	// header and instead embed the window/session id inside the
	// X-Codex-Turn-Metadata JSON header. Parse it so a multi-turn conversation
	// keeps resolving to the same bridge session — required for extension
	// session state (e.g. the deepseek_v4 thinking cache) to survive across
	// turns. Without this, every turn gets a fresh ephemeral session and the
	// thinking replay falls back to empty thinking blocks, degrading the model.
	if id := codexIDFromTurnMetadata(request.Header.Get("X-Codex-Turn-Metadata")); id != "" {
		return "codex-window:" + id
	}
	return ""
}

// codexIDFromTurnMetadata extracts a stable correlation id from Codex's
// X-Codex-Turn-Metadata header. It prefers window_id (matching the legacy
// X-Codex-Window-Id semantics) and falls back to session_id. It returns an
// empty string when the header is missing or malformed, letting the caller
// fall back to an ephemeral session.
func codexIDFromTurnMetadata(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	var meta struct {
		WindowID  string `json:"window_id"`
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal([]byte(raw), &meta); err != nil {
		return ""
	}
	if id := strings.TrimSpace(meta.WindowID); id != "" {
		return id
	}
	return strings.TrimSpace(meta.SessionID)
}
