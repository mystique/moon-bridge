package format

import "context"

type coreHookContextKey string

const coreHookModelAliasKey coreHookContextKey = "moonbridge.core_hook_model_alias"

type requestUserAgentKey = struct{}

// WithRequestUserAgent returns a context carrying a per-request User-Agent
// override. When set, protocol clients prefer this value over their
// statically configured userAgent for the lifetime of the request.
func WithRequestUserAgent(ctx context.Context, ua string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, requestUserAgentKey{}, ua)
}

// RequestUserAgentFromContext extracts a per-request User-Agent override
// previously set via WithRequestUserAgent. Returns empty string when unset.
func RequestUserAgentFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	ua, _ := ctx.Value(requestUserAgentKey{}).(string)
	return ua
}

// WithCoreHookModelAlias tags a context with the model alias used by Core hooks.
func WithCoreHookModelAlias(ctx context.Context, modelAlias string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, coreHookModelAliasKey, modelAlias)
}

// ModelAliasFromCoreHookContext extracts the model alias previously tagged
// by WithCoreHookModelAlias. Returns empty string when unset.
func ModelAliasFromCoreHookContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	model, _ := ctx.Value(coreHookModelAliasKey).(string)
	return model
}
