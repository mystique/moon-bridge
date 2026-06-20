package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"moonbridge/internal/config"
	"moonbridge/internal/service/provider"
	"moonbridge/internal/service/runtime"
)

func TestWatchConfigFileReloadsRoutesFromBindMountedConfig(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yml")
	initial := watchTestFileConfig("qwen3.7-plus")
	writeWatchTestConfig(t, configPath, initial)

	cfg, err := config.LoadFromFile(configPath)
	if err != nil {
		t.Fatalf("LoadFromFile() initial error = %v", err)
	}
	rt := runtime.NewRuntime(cfg, mustWatchTestProviderManager(t, cfg), nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- watchConfigFile(ctx, watchConfigFileOptions{
			Path:         configPath,
			PollInterval: 20 * time.Millisecond,
			LoadOptions:  config.LoadOptions{},
			Runtime:      rt,
		})
	}()

	updated := watchTestFileConfig("qwen3.8-plus")
	writeWatchTestConfig(t, configPath, updated)

	deadline := time.After(2 * time.Second)
	for {
		select {
		case err := <-errCh:
			t.Fatalf("watchConfigFile() returned before cancel: %v", err)
		case <-deadline:
			t.Fatal("runtime route did not update before deadline")
		default:
			snap := rt.Current()
			if got := snap.Config.Routes["gpt-5.5"].Model; got == "qwen3.8-plus" {
				cancel()
				if err := <-errCh; err != nil {
					t.Fatalf("watchConfigFile() after cancel error = %v", err)
				}
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func watchTestFileConfig(routeModel string) config.FileConfig {
	return config.FileConfig{
		Mode: "Transform",
		Server: config.ServerFileConfig{
			Addr:       "127.0.0.1:0",
			SessionTTL: "24h",
		},
		Defaults: config.DefaultsFileConfig{
			Model: "gpt-5.5",
		},
		Models: map[string]config.ModelDefFileConfig{
			"qwen3.7-plus": {ContextWindow: 128000},
			"qwen3.8-plus": {ContextWindow: 128000},
		},
		Providers: map[string]config.ProviderDefFileConfig{
			"aliyun": {
				BaseURL:  "https://dashscope.aliyuncs.com/compatible-mode/v1",
				APIKey:   "test-key",
				Protocol: "openai-chat",
				Offers: []config.OfferFileConfig{
					{Model: "qwen3.7-plus"},
					{Model: "qwen3.8-plus"},
				},
			},
		},
		Routes: map[string]config.RouteFileConfig{
			"gpt-5.5": {
				Model:    routeModel,
				Provider: "aliyun",
			},
		},
		Cache: config.CacheFileConfig{Mode: "off"},
	}
}

func writeWatchTestConfig(t *testing.T, path string, fc config.FileConfig) {
	t.Helper()
	data, err := fc.MarshalYAML()
	if err != nil {
		t.Fatalf("MarshalYAML() error = %v", err)
	}
	if err := writeWatchTestFileAtomic(path, data, 0o600); err != nil {
		t.Fatalf("write test config: %v", err)
	}
}

func writeWatchTestFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace file: %w", err)
	}
	cleanup = false
	return nil
}

func mustWatchTestProviderManager(t *testing.T, cfg config.Config) *provider.ProviderManager {
	t.Helper()
	providerCfg := config.ProviderFromGlobalConfig(&cfg)
	pm, err := provider.NewProviderManager(
		provider.BuildProviderDefsFromConfig(providerCfg),
		provider.BuildModelRoutesFromConfig(providerCfg),
	)
	if err != nil {
		t.Fatalf("NewProviderManager() error = %v", err)
	}
	return pm
}
