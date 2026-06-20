package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"moonbridge/internal/config"
	"moonbridge/internal/service/runtime"
	"moonbridge/internal/service/store"
)

const defaultConfigWatchPollInterval = time.Second

type watchConfigFileOptions struct {
	Path         string
	PollInterval time.Duration
	LoadOptions  config.LoadOptions
	Runtime      *runtime.Runtime
	Store        store.ConfigStore
}

func watchConfigFile(ctx context.Context, opts watchConfigFileOptions) error {
	if opts.Path == "" {
		return errors.New("config watch path is required")
	}
	if opts.Runtime == nil {
		return errors.New("config watch runtime is required")
	}
	interval := opts.PollInterval
	if interval <= 0 {
		interval = defaultConfigWatchPollInterval
	}

	lastDigest, err := digestFile(opts.Path)
	if err != nil {
		return fmt.Errorf("read initial config digest: %w", err)
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			digest, err := digestFile(opts.Path)
			if err != nil {
				slog.Warn("配置文件监听读取失败", "path", opts.Path, "error", err)
				continue
			}
			if bytes.Equal(digest[:], lastDigest[:]) {
				continue
			}
			lastDigest = digest
			reloadConfigFile(ctx, opts)
		}
	}
}

func reloadConfigFile(ctx context.Context, opts watchConfigFileOptions) {
	cfg, err := config.LoadFromFileWithOptions(opts.Path, opts.LoadOptions)
	if err != nil {
		slog.Warn("配置文件变更未生效：加载失败", "path", opts.Path, "error", err)
		return
	}
	if err := opts.Runtime.ValidateCandidate(cfg); err != nil {
		slog.Warn("配置文件变更未生效：运行时校验失败", "path", opts.Path, "error", err)
		return
	}
	if err := opts.Runtime.Reload(cfg); err != nil {
		slog.Warn("配置文件变更未生效：运行时重载失败", "path", opts.Path, "error", err)
		return
	}
	if opts.Store != nil {
		if _, err := opts.Store.SaveConfig(ctx, &cfg); err != nil {
			slog.Warn("配置文件已热加载，但同步到配置存储失败", "path", opts.Path, "error", err)
		}
	}
	slog.Info("配置文件已热加载", "path", opts.Path, "routes", len(cfg.Routes))
}

func digestFile(path string) ([sha256.Size]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	return sha256.Sum256(data), nil
}
