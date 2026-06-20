package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"moonbridge/internal/config"
	"moonbridge/internal/format"
	"moonbridge/internal/protocol/anthropic"
	"moonbridge/internal/protocol/chat"
	"moonbridge/internal/protocol/google"
	openai "moonbridge/internal/protocol/openai"
	"moonbridge/internal/service/provider"
	"moonbridge/internal/service/runtime"
	"moonbridge/internal/service/server"
	"moonbridge/internal/service/stats"
)

// recordingUpstream is a mock upstream server that captures the JSON body
// of the last request so tests can assert on max_tokens / max_output_tokens.
type recordingUpstream struct {
	mu   sync.Mutex
	body []byte
	resp string
}

func (ru *recordingUpstream) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		ru.mu.Lock()
		ru.body = body
		ru.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, ru.resp)
	}
}

func (ru *recordingUpstream) lastBody() []byte {
	ru.mu.Lock()
	defer ru.mu.Unlock()
	return ru.body
}

// extractMaxTokens reads the upstream request body and extracts the
// max_tokens / max_output_tokens / max_completion_tokens field.
func extractMaxTokens(t *testing.T, body []byte) int {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("unmarshal upstream body: %v\nbody: %s", err, body)
	}
	for _, key := range []string{"max_tokens", "max_output_tokens", "max_completion_tokens", "maxOutputTokens"} {
		if v, ok := m[key]; ok {
			if n, ok := v.(float64); ok {
				return int(n)
			}
		}
	}
	// Google nests under generationConfig.
	if gc, ok := m["generationConfig"].(map[string]any); ok {
		if v, ok := gc["maxOutputTokens"]; ok {
			if n, ok := v.(float64); ok {
				return int(n)
			}
		}
	}
	return 0
}

// noopCacheMgr is a no-op anthropic.CacheManager for tests.
type noopCacheMgr struct{}

func (m *noopCacheMgr) PlanAndInject(_ context.Context, _ *anthropic.MessageRequest, _ *format.CoreRequest) (string, string) {
	return "", ""
}
func (m *noopCacheMgr) UpdateRegistry(_ context.Context, _, _ string, _ anthropic.Usage) {}

// buildMaxTokensServer constructs a server.Server wired with a mock upstream,
// adapter registry, provider manager, and runtime for the given protocol.
func buildMaxTokensServer(t *testing.T, protocol string, mockURL string, modelMaxOutput int) *server.Server {
	t.Helper()

	def := config.ProviderDef{
		BaseURL: mockURL,
		APIKey:  "test-key",
		Models: map[string]config.ModelMeta{
			"test-model": {
				MaxOutputTokens: modelMaxOutput,
			},
		},
		Offers: []config.OfferEntry{
			{Model: "test-model", UpstreamName: "test-model"},
		},
	}
	switch protocol {
	case config.ProtocolAnthropic:
		def.Protocol = config.ProtocolAnthropic
	case config.ProtocolOpenAIChat:
		def.Protocol = config.ProtocolOpenAIChat
	case config.ProtocolGoogleGenAI:
		def.Protocol = config.ProtocolGoogleGenAI
	default:
		t.Fatalf("unsupported protocol %q", protocol)
	}

	cfg := config.Config{
		Mode:             config.ModeTransform,
		DefaultMaxTokens: 65536,
		ProviderDefs:     map[string]config.ProviderDef{"upstream": def},
	}

	// Build provider manager from provider defs.
	providerCfg := config.ProviderFromGlobalConfig(&cfg)
	providerDefs := make(map[string]provider.ProviderConfig, len(providerCfg.Providers))
	for key, d := range providerCfg.Providers {
		modelNames := make([]string, 0, len(d.Models))
		models := make(map[string]provider.ModelMeta, len(d.Models))
		for name, meta := range d.Models {
			modelNames = append(modelNames, name)
			models[name] = provider.ModelMeta(meta)
		}
		providerDefs[key] = provider.ProviderConfig{
			BaseURL:    d.BaseURL,
			APIKey:     d.APIKey,
			Version:    d.Version,
			UserAgent:  d.UserAgent,
			Protocol:   d.Protocol,
			ModelNames: modelNames,
			Models:     models,
			Offers:     d.Offers,
		}
	}
	routes := make(map[string]provider.ModelRoute, len(providerCfg.Routes))
	for alias, route := range providerCfg.Routes {
		routes[alias] = provider.ModelRoute{Provider: route.Provider, Name: route.Model}
	}

	pm, err := provider.NewProviderManager(providerDefs, routes)
	if err != nil {
		t.Fatalf("NewProviderManager: %v", err)
	}

	// Build adapter registry.
	reg := format.NewRegistry()
	hooks := format.CorePluginHooks{}.WithDefaults()
	oaiAdapter := openai.NewOpenAIAdapter(hooks)
	if err := reg.RegisterClient(oaiAdapter); err != nil {
		t.Fatalf("RegisterClient: %v", err)
	}
	if err := reg.RegisterClientStream(oaiAdapter); err != nil {
		t.Fatalf("RegisterClientStream: %v", err)
	}
	noopCache := &noopCacheMgr{}
	anthAdapter := anthropic.NewAnthropicProviderAdapter(cfg.DefaultMaxTokens, noopCache, hooks)
	if err := reg.RegisterProvider(anthAdapter); err != nil {
		t.Fatalf("RegisterProvider(anthropic): %v", err)
	}
	if err := reg.RegisterProviderStream(anthAdapter); err != nil {
		t.Fatalf("RegisterProviderStream(anthropic): %v", err)
	}
	gemAdapter := google.NewGeminiProviderAdapter(cfg.DefaultMaxTokens, nil, hooks, nil, nil)
	if err := reg.RegisterProvider(gemAdapter); err != nil {
		t.Fatalf("RegisterProvider(google): %v", err)
	}
	if err := reg.RegisterProviderStream(gemAdapter); err != nil {
		t.Fatalf("RegisterProviderStream(google): %v", err)
	}
	chatAdapter := chat.NewChatProviderAdapter(cfg.DefaultMaxTokens, nil, hooks)
	if err := reg.RegisterProvider(chatAdapter); err != nil {
		t.Fatalf("RegisterProvider(chat): %v", err)
	}
	if err := reg.RegisterProviderStream(chatAdapter); err != nil {
		t.Fatalf("RegisterProviderStream(chat): %v", err)
	}

	// Wire chat/google clients for the provider.
	chatClients := map[string]any{}
	googleClients := map[string]any{}
	if protocol == config.ProtocolOpenAIChat {
		chatClients["upstream"] = chat.NewClient(chat.ClientConfig{
			BaseURL: mockURL,
			APIKey:  "test-key",
		})
	}
	if protocol == config.ProtocolGoogleGenAI {
		googleClients["upstream"] = google.NewClient(google.ClientConfig{
			BaseURL: mockURL,
			APIKey:  "test-key",
		})
	}

	rt := runtime.NewRuntime(cfg, pm, nil)
	sessionStats := stats.NewSessionStats()

	srv := server.New(server.Config{
		AdapterRegistry: reg,
		ProviderMgr:     pm,
		ChatClients:     chatClients,
		GoogleClients:   googleClients,
		Runtime:         rt,
		Stats:           sessionStats,
		AppConfig:       config.ServerConfig{},
	})
	return srv
}

// TestMaxOutputTokensFallback_Anthropic verifies that when Codex does not send
// max_output_tokens, the model's max_output_tokens metadata is used instead of
// the global defaults.max_tokens for the anthropic protocol.
func TestMaxOutputTokensFallback_Anthropic(t *testing.T) {
	ru := &recordingUpstream{
		resp: `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"test-model","stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":3}}`,
	}
	mock := httptest.NewServer(http.HandlerFunc(ru.handler()))
	defer mock.Close()

	srv := buildMaxTokensServer(t, config.ProtocolAnthropic, mock.URL, 32000)

	// Codex-style request: no max_output_tokens field.
	body := bytes.NewBufferString(`{"model":"test-model(upstream)","input":"hi"}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", body)
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	got := extractMaxTokens(t, ru.lastBody())
	if got != 32000 {
		t.Fatalf("anthropic upstream max_tokens = %d, want 32000 (model metadata)", got)
	}
}

// TestMaxOutputTokensFallback_Chat verifies the same behavior for openai-chat.
func TestMaxOutputTokensFallback_Chat(t *testing.T) {
	ru := &recordingUpstream{
		resp: `{"id":"chatcmpl_1","object":"chat.completion","created":1,"model":"test-model","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`,
	}
	mock := httptest.NewServer(http.HandlerFunc(ru.handler()))
	defer mock.Close()

	srv := buildMaxTokensServer(t, config.ProtocolOpenAIChat, mock.URL, 16000)

	body := bytes.NewBufferString(`{"model":"test-model(upstream)","input":"hi"}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", body)
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	got := extractMaxTokens(t, ru.lastBody())
	if got != 16000 {
		t.Fatalf("chat upstream max_completion_tokens = %d, want 16000 (model metadata)", got)
	}
}

// TestMaxOutputTokensFallback_Google verifies the same behavior for google-genai.
func TestMaxOutputTokensFallback_Google(t *testing.T) {
	ru := &recordingUpstream{
		resp: `{"candidates":[{"content":{"parts":[{"text":"ok"}],"role":"model"},"finishReason":"STOP","index":0}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":3,"totalTokenCount":8}}`,
	}
	mock := httptest.NewServer(http.HandlerFunc(ru.handler()))
	defer mock.Close()

	srv := buildMaxTokensServer(t, config.ProtocolGoogleGenAI, mock.URL, 8000)

	body := bytes.NewBufferString(`{"model":"test-model(upstream)","input":"hi"}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", body)
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	got := extractMaxTokens(t, ru.lastBody())
	if got != 8000 {
		t.Fatalf("google upstream maxOutputTokens = %d, want 8000 (model metadata)", got)
	}
}

// TestMaxOutputTokensFallback_GlobalDefault verifies that when the model has
// no max_output_tokens metadata, the global defaults.max_tokens is used.
func TestMaxOutputTokensFallback_GlobalDefault(t *testing.T) {
	ru := &recordingUpstream{
		resp: `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"test-model","stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":3}}`,
	}
	mock := httptest.NewServer(http.HandlerFunc(ru.handler()))
	defer mock.Close()

	// modelMaxOutput = 0 -> should fall back to global default 65536.
	srv := buildMaxTokensServer(t, config.ProtocolAnthropic, mock.URL, 0)

	body := bytes.NewBufferString(`{"model":"test-model(upstream)","input":"hi"}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", body)
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	got := extractMaxTokens(t, ru.lastBody())
	if got != 65536 {
		t.Fatalf("anthropic upstream max_tokens = %d, want 65536 (global default)", got)
	}
}

// TestMaxOutputTokensFallback_RequestWins verifies that an explicit
// max_output_tokens in the client request takes priority over both the
// model metadata and the global default.
func TestMaxOutputTokensFallback_RequestWins(t *testing.T) {
	ru := &recordingUpstream{
		resp: `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"test-model","stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":3}}`,
	}
	mock := httptest.NewServer(http.HandlerFunc(ru.handler()))
	defer mock.Close()

	srv := buildMaxTokensServer(t, config.ProtocolAnthropic, mock.URL, 32000)

	// Client explicitly sends max_output_tokens = 4096.
	body := bytes.NewBufferString(`{"model":"test-model(upstream)","input":"hi","max_output_tokens":4096}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", body)
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	got := extractMaxTokens(t, ru.lastBody())
	if got != 4096 {
		t.Fatalf("anthropic upstream max_tokens = %d, want 4096 (explicit request)", got)
	}
}

// TestMaxOutputTokensFallback_Streaming verifies the model metadata fallback
// works on the streaming path for the anthropic protocol.
func TestMaxOutputTokensFallback_Streaming(t *testing.T) {
	ru := &recordingUpstream{
		resp: "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"test-model\",\"usage\":{\"input_tokens\":5,\"output_tokens\":0}}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":3}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	}
	mock := httptest.NewServer(http.HandlerFunc(ru.handler()))
	defer mock.Close()

	srv := buildMaxTokensServer(t, config.ProtocolAnthropic, mock.URL, 24000)

	body := bytes.NewBufferString(`{"model":"test-model(upstream)","input":"hi","stream":true}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", body)
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	got := extractMaxTokens(t, ru.lastBody())
	if got != 24000 {
		t.Fatalf("anthropic streaming upstream max_tokens = %d, want 24000 (model metadata)", got)
	}
}
