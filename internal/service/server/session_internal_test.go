package server

import (
	"net/http"
	"testing"
	"time"

	deepseekv4 "moonbridge/internal/extension/deepseek_v4"
	"moonbridge/internal/extension/plugin"
	"moonbridge/internal/service/server/session"
)

// stubSessionConfig satisfies session.ConfigAccessor for tests.
type stubSessionConfig struct{}

func (stubSessionConfig) SessionTTL() time.Duration { return time.Hour }
func (stubSessionConfig) MaxSessions() int          { return 100 }

func newTestRequest(t *testing.T, headers map[string]string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "/v1/responses", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return req
}

func TestSessionKeyFromRequest(t *testing.T) {
	tests := []struct {
		name    string
		headers map[string]string
		want    string
	}{
		{
			name:    "Session_id header",
			headers: map[string]string{"Session_id": "abc-123"},
			want:    "session:abc-123",
		},
		{
			name:    "standalone X-Codex-Window-Id header",
			headers: map[string]string{"X-Codex-Window-Id": "win-1"},
			want:    "codex-window:win-1",
		},
		{
			name: "X-Codex-Turn-Metadata prefers window_id",
			headers: map[string]string{
				"X-Codex-Turn-Metadata": `{"installation_id":"i","session_id":"019ee386-503f-7d90-8666-1e79eb517047","thread_id":"019ee386-503f-7d90-8666-1e79eb517047","turn_id":"t1","window_id":"019ee386-503f-7d90-8666-1e79eb517047:0"}`,
			},
			want: "codex-window:019ee386-503f-7d90-8666-1e79eb517047:0",
		},
		{
			name: "X-Codex-Turn-Metadata falls back to session_id",
			headers: map[string]string{
				"X-Codex-Turn-Metadata": `{"session_id":"sess-9","turn_id":"t1"}`,
			},
			want: "codex-window:sess-9",
		},
		{
			name:    "no correlation headers yields empty key",
			headers: map[string]string{},
			want:    "",
		},
		{
			name:    "malformed metadata yields empty key",
			headers: map[string]string{"X-Codex-Turn-Metadata": `{not json`},
			want:    "",
		},
		{
			name:    "metadata without ids yields empty key",
			headers: map[string]string{"X-Codex-Turn-Metadata": `{"turn_id":"t1"}`},
			want:    "",
		},
		{
			name: "standalone X-Codex-Window-Id takes precedence over metadata",
			headers: map[string]string{
				"X-Codex-Window-Id":     "explicit-win",
				"X-Codex-Turn-Metadata": `{"window_id":"meta-win"}`,
			},
			want: "codex-window:explicit-win",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := sessionKeyFromRequest(newTestRequest(t, tc.headers))
			if got != tc.want {
				t.Fatalf("sessionKeyFromRequest = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestSessionForRequestSharesStateAcrossCodexTurns is the regression test for
// the original bug: Codex stopped sending the standalone X-Codex-Window-Id
// header and moved the window/session id into X-Codex-Turn-Metadata, so every
// turn got a fresh ephemeral session and the deepseek_v4 thinking cache never
// survived across turns. Two turns carrying the same metadata must now resolve
// to the same session (and thus the same extension state), while a request
// with no correlation headers must not.
func TestSessionForRequestSharesStateAcrossCodexTurns(t *testing.T) {
	registry := plugin.NewRegistry(nil)
	registry.Register(deepseekv4.NewPlugin())
	mgr := session.NewInMemoryManager(stubSessionConfig{}, registry)
	defer mgr.Stop()
	srv := &Server{sessionManager: mgr}

	const meta = `{"session_id":"019ee386-503f-7d90-8666-1e79eb517047","turn_id":"t1","window_id":"019ee386-503f-7d90-8666-1e79eb517047:0"}`

	sess1 := srv.sessionForRequest(newTestRequest(t, map[string]string{"X-Codex-Turn-Metadata": meta}))
	sess2 := srv.sessionForRequest(newTestRequest(t, map[string]string{"X-Codex-Turn-Metadata": meta}))
	if sess1 == nil || sess2 == nil {
		t.Fatalf("expected non-nil sessions, got %v / %v", sess1, sess2)
	}
	if sess1 != sess2 {
		t.Fatalf("expected the same session across codex turns, got %p vs %p", sess1, sess2)
	}

	st1, _ := sess1.ExtensionData["deepseek_v4"].(*deepseekv4.State)
	st2, _ := sess2.ExtensionData["deepseek_v4"].(*deepseekv4.State)
	if st1 == nil || st1 != st2 {
		t.Fatalf("expected shared deepseek_v4 state across turns, got %p / %p", st1, st2)
	}

	// A request without correlation headers must not reuse the codex session.
	ephemeral := srv.sessionForRequest(newTestRequest(t, nil))
	if ephemeral == sess1 {
		t.Fatalf("request without correlation headers should not reuse the codex session")
	}
}
