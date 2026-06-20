package visual

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"

	"log/slog"

	"moonbridge/internal/format"
)

// CoreUpstreamProvider wraps any CoreProvider to be used as the upstream
// (text-only model) in the visual orchestration loop.
type CoreUpstreamProvider interface {
	CreateCore(ctx context.Context, req *format.CoreRequest) (*format.CoreResponse, error)
}

// CoreOrchestrator implements the visual tool loop on protocol-agnostic Core types.
// It intercepts tool_use responses from the upstream model, routes visual tools
// (visual_brief, visual_qa) to the vision model, injects tool_results, and loops
// until the upstream model produces a non-tool_use response.
type CoreOrchestrator struct {
	upstream  CoreUpstreamProvider
	client    VisionClient
	maxRounds int
	observer  CoreTraceObserver
}

type CoreOrchestratorConfig struct {
	Upstream  CoreUpstreamProvider
	Client    VisionClient
	MaxRounds int
	Observer  CoreTraceObserver
}

type CoreTraceObserver interface {
	RecordVisualEvent(event CoreTraceEvent)
}

type CoreTraceEvent struct {
	Stage        string
	Round        int
	ImageCount   int
	Images       []CoreTraceImage
	Request      CoreTraceRequestSummary
	Response     CoreTraceResponseSummary
	ToolName     string
	ToolUseID    string
	PromptLength int
	ResultLength int
	Result       string
	OutputTypes  []string
}

type CoreTraceImage struct {
	Index    int    `json:"index"`
	MimeType string `json:"mime_type,omitempty"`
	Bytes    int    `json:"bytes,omitempty"`
	SHA256   string `json:"sha256,omitempty"`
	URL      string `json:"url,omitempty"`
}

type CoreTraceRequestSummary struct {
	MessageCount   int  `json:"message_count"`
	ToolCount      int  `json:"tool_count"`
	HasImageBlocks bool `json:"has_image_blocks"`
}

type CoreTraceResponseSummary struct {
	StopReason   string   `json:"stop_reason,omitempty"`
	Status       string   `json:"status,omitempty"`
	ContentTypes []string `json:"content_types,omitempty"`
	TextLength   int      `json:"text_length,omitempty"`
	ToolUseNames []string `json:"tool_use_names,omitempty"`
}

// NewCoreOrchestrator creates a CoreOrchestrator.
func NewCoreOrchestrator(cfg CoreOrchestratorConfig) *CoreOrchestrator {
	maxRounds := cfg.MaxRounds
	if maxRounds <= 0 {
		maxRounds = 4
	}
	return &CoreOrchestrator{
		upstream:  cfg.Upstream,
		client:    cfg.Client,
		maxRounds: maxRounds,
		observer:  cfg.Observer,
	}
}

// CreateCore runs the visual orchestration loop on Core types.
// It strips image blocks from messages, passes them to the upstream model,
// intercepts visual tool calls, executes them via the vision client, and loops.
func (o *CoreOrchestrator) CreateCore(ctx context.Context, req *format.CoreRequest) (*format.CoreResponse, error) {
	if o == nil || o.upstream == nil {
		return nil, fmt.Errorf("visual upstream provider is nil")
	}
	if o.client == nil {
		return nil, fmt.Errorf("visual client is nil")
	}
	req = cloneCoreRequest(req)
	req, availableImages := prepareCoreRequestForVisual(req)
	o.recordVisualEvent(CoreTraceEvent{
		Stage:      "prepare_core_request",
		ImageCount: len(availableImages),
		Images:     summarizeCoreTraceImages(availableImages),
		Request:    summarizeCoreRequest(req),
	})
	log := slog.Default()
	aggregatedUsage := format.CoreUsage{}
	hasAggregatedUsage := false

	for round := 0; round < o.maxRounds; round++ {
		roundReq := cloneCoreRequest(req)
		resp, err := o.upstream.CreateCore(ctx, roundReq)
		if err != nil {
			return nil, err
		}
		if resp == nil {
			return nil, fmt.Errorf("upstream returned nil response")
		}
		if coreUsagePresent(resp.Usage) {
			hasAggregatedUsage = true
			aggregateCoreUsage(&aggregatedUsage, resp.Usage)
		}
		o.recordVisualEvent(CoreTraceEvent{
			Stage:    "main_model_round",
			Round:    round + 1,
			Request:  summarizeCoreRequest(roundReq),
			Response: summarizeCoreResponse(resp),
		})

		// If not a tool_use stop, we're done.
		if resp.StopReason != "tool_use" {
			resp = applyCoreUsageAggregation(resp, aggregatedUsage, hasAggregatedUsage)
			o.recordVisualEvent(CoreTraceEvent{
				Stage:       "completed",
				Result:      coreResponseResult(resp),
				OutputTypes: coreResponseOutputTypes(resp),
				Response:    summarizeCoreResponse(resp),
			})
			return resp, nil
		}

		// Find visual tool uses in the last assistant message.
		lastAssistant := findLastAssistantMessage(resp.Messages)
		if lastAssistant == nil {
			return applyCoreUsageAggregation(resp, aggregatedUsage, hasAggregatedUsage), nil
		}

		toolUses, nonVisual := coreSplitVisualToolUses(lastAssistant.Content)
		if len(toolUses) == 0 {
			return applyCoreUsageAggregation(resp, aggregatedUsage, hasAggregatedUsage), nil
		}
		if len(nonVisual) > 0 {
			// Execute visual tools, feed results back to model, and continue.
			// Non-visual tool_uses remain in the assistant message so the Bridge
			// can process them on the next round.
			toolResults := make([]format.CoreContentBlock, 0, len(toolUses))
			for _, toolUse := range toolUses {
				result := o.executeCoreVisualTool(ctx, toolUse, availableImages)
				toolResults = append(toolResults, format.CoreContentBlock{
					Type:              "tool_result",
					ToolUseID:         toolUse.ToolUseID,
					ToolResultContent: []format.CoreContentBlock{{Type: "text", Text: result}},
				})
			}
			req.Messages = append(req.Messages, *lastAssistant)
			req.Messages = append(req.Messages, format.CoreMessage{
				Role:    "tool",
				Content: toolResults,
			})
			if req.ToolChoice != nil && req.ToolChoice.Mode != "auto" {
				req.ToolChoice = &format.CoreToolChoice{Mode: "auto"}
			}
			log.Debug("Core visual mixed tool loop", "round", round+1, "visual_tools", len(toolUses), "non_visual", len(nonVisual))
			continue
		}

		// Execute each visual tool via the vision client.
		toolResults := make([]format.CoreContentBlock, 0, len(toolUses))
		for _, toolUse := range toolUses {
			result := o.executeCoreVisualTool(ctx, toolUse, availableImages)
			toolResults = append(toolResults, format.CoreContentBlock{
				Type:              "tool_result",
				ToolUseID:         toolUse.ToolUseID,
				ToolResultContent: []format.CoreContentBlock{{Type: "text", Text: result}},
			})
		}

		// Append assistant message and tool_result message for next round.
		req.Messages = append(req.Messages, *lastAssistant)
		req.Messages = append(req.Messages, format.CoreMessage{
			Role:    "tool",
			Content: toolResults,
		})

		// Reset tool_choice to auto for follow-up rounds.
		if req.ToolChoice != nil && req.ToolChoice.Mode != "auto" {
			req.ToolChoice = &format.CoreToolChoice{Mode: "auto"}
		}

		log.Debug("Core visual tool loop completed", "round", round+1, "tools_executed", len(toolUses))
	}

	return nil, fmt.Errorf("visual loop exceeded max rounds (%d)", o.maxRounds)
}

// prepareCoreRequestForVisual strips image blocks from the Core request and
// replaces them with text placeholders, returning the collected images.
func prepareCoreRequestForVisual(req *format.CoreRequest) (*format.CoreRequest, []ImageInput) {
	availableImages := make([]ImageInput, 0)
	for messageIndex := range req.Messages {
		content := req.Messages[messageIndex].Content
		if len(content) == 0 {
			continue
		}
		rewritten := make([]format.CoreContentBlock, 0, len(content))
		for _, block := range content {
			rewritten = append(rewritten, prepareCoreBlockForVisual(block, &availableImages))
		}
		req.Messages[messageIndex].Content = rewritten
	}
	return req, availableImages
}

func prepareCoreBlocksForVisual(blocks []format.CoreContentBlock, availableImages *[]ImageInput) []format.CoreContentBlock {
	rewritten := make([]format.CoreContentBlock, 0, len(blocks))
	for _, block := range blocks {
		rewritten = append(rewritten, prepareCoreBlockForVisual(block, availableImages))
	}
	return rewritten
}

func prepareCoreBlockForVisual(block format.CoreContentBlock, availableImages *[]ImageInput) format.CoreContentBlock {
	if block.Type == "tool_result" {
		block.ToolResultContent = prepareCoreBlocksForVisual(block.ToolResultContent, availableImages)
		return block
	}
	if block.Type != "image" {
		return block
	}
	image, ok := imageInputFromCoreBlock(block)
	if !ok {
		return format.CoreContentBlock{
			Type: "text",
			Text: "[Image was attached but could not be processed.]",
		}
	}
	*availableImages = append(*availableImages, image)
	return format.CoreContentBlock{
		Type: "text",
		Text: visualAttachmentText(len(*availableImages)),
	}
}

// imageInputFromCoreBlock converts a Core image content block to ImageInput.
func imageInputFromCoreBlock(block format.CoreContentBlock) (ImageInput, bool) {
	if block.ImageData == "" {
		return ImageInput{}, false
	}
	// If MediaType is explicitly set, treat as base64.
	if block.MediaType != "" {
		return ImageInput{Data: block.ImageData, MimeType: block.MediaType}, true
	}
	// Check for data: URL (contains embedded MIME type).
	if strings.HasPrefix(block.ImageData, "data:") {
		mediaType, raw := splitDataURL(block.ImageData)
		return ImageInput{Data: raw, MimeType: mediaType}, true
	}
	// URL-based image (ImageData holds the URL when MediaType is empty).
	url := strings.TrimSpace(block.ImageData)
	if isSupportedImageURL(url) {
		return ImageInput{URL: url}, true
	}
	// Fallback: treat as base64 with default MIME type rather than silently dropping.
	return ImageInput{Data: block.ImageData, MimeType: "image/png"}, true
}

// findLastAssistantMessage finds the last assistant message in a slice.
// Prefers assistant messages that contain tool_use blocks, searching backward
// so the most recent tool-use-bearing assistant message is returned.
func findLastAssistantMessage(messages []format.CoreMessage) *format.CoreMessage {
	var candidate *format.CoreMessage
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role != "assistant" {
			continue
		}
		// Check if this assistant message has any tool_use content blocks.
		hasToolUse := false
		for _, block := range messages[i].Content {
			if block.Type == "tool_use" {
				hasToolUse = true
				break
			}
		}
		if hasToolUse {
			return &messages[i]
		}
		// Fallback: remember the last assistant message if we don't find a better one.
		if candidate == nil {
			candidate = &messages[i]
		}
	}
	return candidate
}

// coreSplitVisualToolUses separates visual tool_use blocks from non-visual ones.
func coreSplitVisualToolUses(blocks []format.CoreContentBlock) (visualUses, nonVisualToolUses []format.CoreContentBlock) {
	for _, block := range blocks {
		if block.Type != "tool_use" {
			continue
		}
		if IsVisualTool(block.ToolName) {
			visualUses = append(visualUses, block)
		} else {
			nonVisualToolUses = append(nonVisualToolUses, block)
		}
	}
	return visualUses, nonVisualToolUses
}

// executeCoreVisualTool runs the vision model and returns a formatted result string.
func (o *CoreOrchestrator) executeCoreVisualTool(ctx context.Context, toolUse format.CoreContentBlock, availableImages []ImageInput) string {
	request, err := coreAnalysisRequestFromToolUse(toolUse, availableImages)
	if err != nil {
		result := "Visual error: " + err.Error()
		o.recordVisualEvent(CoreTraceEvent{
			Stage:        "visual_tool",
			ToolName:     toolUse.ToolName,
			ToolUseID:    toolUse.ToolUseID,
			ImageCount:   0,
			Result:       "error",
			ResultLength: len(result),
		})
		return result
	}
	result, err := o.client.Analyze(ctx, request)
	if err != nil {
		slog.Default().Warn("Visual tool execution failed", "tool", toolUse.ToolName, "error", err)
		formatted := "Visual error: " + err.Error()
		o.recordVisualEvent(CoreTraceEvent{
			Stage:        "visual_tool",
			ToolName:     toolUse.ToolName,
			ToolUseID:    toolUse.ToolUseID,
			ImageCount:   len(request.Images),
			PromptLength: len(request.Prompt),
			Result:       "error",
			ResultLength: len(formatted),
		})
		return formatted
	}
	slog.Default().Info("Visual tool executed", "tool", toolUse.ToolName, "images", len(request.Images))
	formatted := strings.TrimSpace(result)
	switch toolUse.ToolName {
	case ToolVisualBrief:
		formatted = "Visual Brief result:\n" + formatted
	case ToolVisualQA:
		formatted = "Visual QA result:\n" + formatted
	}
	o.recordVisualEvent(CoreTraceEvent{
		Stage:        "visual_tool",
		ToolName:     toolUse.ToolName,
		ToolUseID:    toolUse.ToolUseID,
		ImageCount:   len(request.Images),
		PromptLength: len(request.Prompt),
		Result:       "ok",
		ResultLength: len(formatted),
	})
	return formatted
}

func (o *CoreOrchestrator) recordVisualEvent(event CoreTraceEvent) {
	if o != nil && o.observer != nil {
		o.observer.RecordVisualEvent(event)
	}
}

func summarizeCoreTraceImages(images []ImageInput) []CoreTraceImage {
	out := make([]CoreTraceImage, 0, len(images))
	for i, image := range images {
		item := CoreTraceImage{
			Index:    i + 1,
			MimeType: image.MimeType,
			URL:      image.URL,
		}
		if image.Data != "" {
			item.Bytes = len(image.Data)
			sum := sha256.Sum256([]byte(image.Data))
			item.SHA256 = fmt.Sprintf("%x", sum[:])
		}
		out = append(out, item)
	}
	return out
}

func summarizeCoreRequest(req *format.CoreRequest) CoreTraceRequestSummary {
	if req == nil {
		return CoreTraceRequestSummary{}
	}
	summary := CoreTraceRequestSummary{
		MessageCount: len(req.Messages),
		ToolCount:    len(req.Tools),
	}
	for _, msg := range req.Messages {
		for _, block := range msg.Content {
			if block.Type == "image" || blockHasImage(block) {
				summary.HasImageBlocks = true
				return summary
			}
		}
	}
	return summary
}

func blockHasImage(block format.CoreContentBlock) bool {
	for _, child := range block.ToolResultContent {
		if child.Type == "image" || blockHasImage(child) {
			return true
		}
	}
	return false
}

func summarizeCoreResponse(resp *format.CoreResponse) CoreTraceResponseSummary {
	if resp == nil {
		return CoreTraceResponseSummary{}
	}
	summary := CoreTraceResponseSummary{
		StopReason: resp.StopReason,
		Status:     resp.Status,
	}
	for _, msg := range resp.Messages {
		if msg.Role != "assistant" {
			continue
		}
		for _, block := range msg.Content {
			summary.ContentTypes = append(summary.ContentTypes, block.Type)
			if block.Type == "text" {
				summary.TextLength += len(block.Text)
			}
			if block.Type == "tool_use" {
				summary.ToolUseNames = append(summary.ToolUseNames, block.ToolName)
			}
		}
	}
	return summary
}

func coreResponseResult(resp *format.CoreResponse) string {
	types := coreResponseOutputTypes(resp)
	if len(types) == 0 {
		return "empty"
	}
	hasMessage := false
	hasFunctionCall := false
	hasReasoning := false
	for _, typ := range types {
		switch typ {
		case "message":
			hasMessage = true
		case "function_call":
			hasFunctionCall = true
		case "reasoning":
			hasReasoning = true
		}
	}
	switch {
	case hasFunctionCall:
		return "function_call"
	case hasMessage:
		return "message"
	case hasReasoning:
		return "reasoning_only"
	default:
		return "empty"
	}
}

func coreResponseOutputTypes(resp *format.CoreResponse) []string {
	if resp == nil {
		return nil
	}
	seen := make(map[string]struct{})
	out := make([]string, 0, 3)
	add := func(typ string) {
		if typ == "" {
			return
		}
		if _, ok := seen[typ]; ok {
			return
		}
		seen[typ] = struct{}{}
		out = append(out, typ)
	}
	for _, msg := range resp.Messages {
		if msg.Role != "assistant" {
			continue
		}
		for _, block := range msg.Content {
			switch block.Type {
			case "text", "image":
				add("message")
			case "tool_use":
				add("function_call")
			case "reasoning":
				add("reasoning")
			}
		}
	}
	return out
}

// coreAnalysisRequestFromToolUse parses a tool_use CoreContentBlock into an
// AnalysisRequest for the vision client. The structure mirrors the old
// anthropic-specific version but uses CoreContentBlock fields.
func coreAnalysisRequestFromToolUse(toolUse format.CoreContentBlock, availableImages []ImageInput) (AnalysisRequest, error) {
	switch toolUse.ToolName {
	case ToolVisualBrief:
		var input briefInput
		if err := json.Unmarshal(toolUse.ToolInput, &input); err != nil {
			return AnalysisRequest{}, fmt.Errorf("parse visual_brief input: %w", err)
		}
		images := normalizeImages(input.ImageURL, input.ImageURLs, input.Images, input.ImageRefs, availableImages)
		if len(images) == 0 {
			return AnalysisRequest{}, fmt.Errorf("visual_brief requires valid image URLs/data/images or attached images")
		}
		return AnalysisRequest{
			Tool:   ToolVisualBrief,
			Prompt: buildBriefPrompt(input),
			Images: images,
		}, nil
	case ToolVisualQA:
		var input qaInput
		if err := json.Unmarshal(toolUse.ToolInput, &input); err != nil {
			return AnalysisRequest{}, fmt.Errorf("parse visual_qa input: %w", err)
		}
		if strings.TrimSpace(input.Question) == "" {
			return AnalysisRequest{}, fmt.Errorf("visual_qa requires question")
		}
		return AnalysisRequest{
			Tool:   ToolVisualQA,
			Prompt: buildQAPrompt(input),
			Images: normalizeImages(input.ImageURL, input.ImageURLs, input.Images, input.ImageRefs, availableImages),
		}, nil
	default:
		return AnalysisRequest{}, fmt.Errorf("unknown visual tool %q", toolUse.ToolName)
	}
}

func coreUsagePresent(usage format.CoreUsage) bool {
	return usage.InputTokens > 0 || usage.OutputTokens > 0 || usage.CachedInputTokens > 0 || usage.TotalTokens > 0
}

func aggregateCoreUsage(total *format.CoreUsage, usage format.CoreUsage) {
	total.InputTokens += usage.InputTokens
	total.OutputTokens += usage.OutputTokens
	total.CachedInputTokens += usage.CachedInputTokens
	total.TotalTokens = total.InputTokens + total.OutputTokens
}

func applyCoreUsageAggregation(resp *format.CoreResponse, usage format.CoreUsage, hasUsage bool) *format.CoreResponse {
	if resp == nil || !hasUsage {
		return resp
	}
	resp.Usage = usage
	resp.Usage.TotalTokens = resp.Usage.InputTokens + resp.Usage.OutputTokens
	return resp
}

func cloneCoreRequest(req *format.CoreRequest) *format.CoreRequest {
	if req == nil {
		return nil
	}

	data, err := json.Marshal(req)
	if err != nil {
		cloned := *req
		return &cloned
	}

	var cloned format.CoreRequest
	if err := json.Unmarshal(data, &cloned); err != nil {
		cloned = *req
		return &cloned
	}
	return &cloned
}
