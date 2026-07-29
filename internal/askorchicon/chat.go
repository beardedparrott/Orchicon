package askorchicon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/opencode"
)

// defaultTimeout is the default maximum duration for a single ChatStream
// response. When the provider or model stalls beyond this limit the stream
// terminates with a timeout error. Override via ORCHICON_ASK_TIMEOUT.
const defaultTimeout = 600 * time.Second

func askTimeout() time.Duration {
	if v := os.Getenv("ORCHICON_ASK_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return defaultTimeout
}

// opencodeEvent is a single JSON event from opencode's stdout.
type opencodeEvent struct {
	Type string         `json:"type"`
	Part map[string]any `json:"part"`
}

// streamCallback is called for each streaming event from opencode.
type streamCallback func(evt opencodeEvent) error

func (s *Service) ChatStream(ctx context.Context, req *connect.Request[apiv1.ChatStreamRequest], stream *connect.ServerStream[apiv1.ChatStreamResponse]) error {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return connect.NewError(connect.CodeInternal, err)
	}
	msg := strings.TrimSpace(req.Msg.Message)
	if msg == "" {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("message must not be empty"))
	}
	if req.Msg.ConversationId == "" {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("conversation_id must not be empty"))
	}

	// --- 1. Load conversation. ---
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return connect.NewError(connect.CodeInternal, err)
	}
	conv, err := db.GetConversation(ctx, ttx.Tx, tenantID, req.Msg.ConversationId)
	if err != nil {
		ttx.Rollback(ctx)
		if errors.Is(err, db.ErrNotFound) {
			return connect.NewError(connect.CodeNotFound, errors.New("conversation not found"))
		}
		return connect.NewError(connect.CodeInternal, err)
	}
	ttx.Rollback(ctx)

	// --- 2. Persist user message. ---
	userMsg := db.MessageRow{
		ID:              db.NewID(),
		TenantID:        tenantID,
		ConversationID:  req.Msg.ConversationId,
		Role:            "user",
		Content:         msg,
		ToolCalls:       []byte("[]"),
		ToolResults:     []byte("[]"),
		Attachments:     []byte("[]"),
		Metadata:        []byte("{}"),
	}
	ttx, err = s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return connect.NewError(connect.CodeInternal, err)
	}
	if _, err := db.CreateMessage(ctx, ttx.Tx, userMsg); err != nil {
		ttx.Rollback(ctx)
		return connect.NewError(connect.CodeInternal, fmt.Errorf("save user message: %w", err))
	}
	if err := db.UpdateConversationTimestamp(ctx, ttx.Tx, tenantID, req.Msg.ConversationId); err != nil {
		ttx.Rollback(ctx)
		return connect.NewError(connect.CodeInternal, err)
	}
	if err := ttx.Commit(ctx); err != nil {
		return connect.NewError(connect.CodeInternal, err)
	}

	// --- 3. Resolve model and build prompt. ---
	modelRef := s.modelRefOrFallback(ctx, tenantID, conv.ModelRef)
	if modelRef == "" {
		modelRef = "opencode/deepseek-v4-flash-free"
	}

	ttx, err = s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return connect.NewError(connect.CodeInternal, err)
	}
	prevMessages, _ := db.ListMessages(ctx, ttx.Tx, tenantID, req.Msg.ConversationId, 50, "")
	cfg, _ := db.GetAgentConfig(ctx, ttx.Tx, tenantID)
	ttx.Rollback(ctx)

	// Pre-execute: detect tool-callable intent and run matching read-only
	// tools. The results are injected into the prompt so the model has
	// real data to reason about. We also track which tools were executed
	// so the prompt can tell the model NOT to re-call them.
	toolResults, preExecNames := s.preExecuteTools(ctx, tenantID, msg)

	fullPrompt := buildLLMPrompt(cfg, s.toolRegistry, prevMessages, msg, req.Msg.Attachments, toolResults, preExecNames)

	// --- 4. Stream from opencode, stripping tool call markers.
	// The frontend shows a "thinking" indicator during the initial latency
	// (opencode startup + model first token). We'll send a progress update
	// if the model takes more than a few seconds.
	var fullResponse strings.Builder
	var firstTextMu sync.Mutex
	firstTextReceived := false

	// Goroutine: send periodic progress messages while waiting for the
	// model's first token.
	go func() {
		defer func() { recover() }() // protect against stream closed before goroutine exits
		intervals := []struct {
			delay time.Duration
			msg   string
		}{
			{3 * time.Second, "Looking into it…"},
			{4 * time.Second, "Still thinking…"},
			{4 * time.Second, "Gathering the details…"},
			{4 * time.Second, "Almost there…"},
		}
		for _, p := range intervals {
			time.Sleep(p.delay)
			// Check if the context is done before sending.
			if ctx.Err() != nil {
				return
			}
			firstTextMu.Lock()
			if firstTextReceived {
				firstTextMu.Unlock()
				return
			}
			firstTextMu.Unlock()
			stream.Send(&apiv1.ChatStreamResponse{
				Event: &apiv1.ChatStreamResponse_TextChunk{
					TextChunk: &apiv1.TextChunk{Content: "> *" + p.msg + "*"},
				},
			})
		}
	}()

	cb := func(evt opencodeEvent) error {
		switch evt.Type {
		case "text":
			firstTextMu.Lock()
			if !firstTextReceived {
				firstTextReceived = true
			}
			firstTextMu.Unlock()

			rawText, _ := evt.Part["text"].(string)
			if rawText == "" {
				return nil
			}
			// Accumulate the full response for persistence.
			fullResponse.WriteString(rawText)
			// Stream text in chunks that feel natural — small enough
			// to appear letter-by-letter but large enough to avoid
			// excessive network round-trips.
			const chunkSize = 30
			for i := 0; i < len(rawText); i += chunkSize {
				end := i + chunkSize
				if end > len(rawText) {
					end = len(rawText)
				}
				if err := stream.Send(&apiv1.ChatStreamResponse{
					Event: &apiv1.ChatStreamResponse_TextChunk{
						TextChunk: &apiv1.TextChunk{Content: rawText[i:end]},
					},
				}); err != nil {
					return err
				}
				time.Sleep(30 * time.Millisecond)
			}
			return nil

		case "tool_use":
			toolName, _ := evt.Part["tool"].(string)
			state, _ := evt.Part["state"].(map[string]any)
			if strings.HasPrefix(toolName, "orchicon_") {
				goTool := strings.TrimPrefix(toolName, "orchicon_")
				inRaw, _ := state["input"]
				inputJSON, _ := json.Marshal(inRaw)
				resultJSON, execErr := s.toolRegistry.Execute(ctx, s.pool, goTool, inputJSON)
				if execErr != nil {
					s.log.Warn("tool execution failed", "tool", goTool, "error", execErr)
				} else {
					stream.Send(&apiv1.ChatStreamResponse{
						Event: &apiv1.ChatStreamResponse_ToolCallResult{
							ToolCallResult: &apiv1.ToolCallResult{
								ToolCallId:  toolName,
								Output:      string(resultJSON),
								IsError:     execErr != nil,
							},
						},
					})
				}
			}
			return nil

		case "error":
			errMsg, _ := evt.Part["error"].(string)
			if errMsg != "" {
				s.log.Warn("opencode error", "error", errMsg)
			}
			return nil

		default:
			return nil
		}
	}

	msgID, elapsed, streamErr := s.runOpenCodeStream(ctx, modelRef, fullPrompt, msg, cb)

	elapsedMS := elapsed.Milliseconds()
	metaJSON, _ := json.Marshal(map[string]any{
		"model_ref":  modelRef,
		"latency_ms": elapsedMS,
	})

	// --- 5. Persist assistant response. ---
	aid := msgID
	if aid == "" {
		aid = db.NewID()
	}
	responseText := strings.TrimSpace(fullResponse.String())

	ttx, err = s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return connect.NewError(connect.CodeInternal, err)
	}
	assistantMsg := db.MessageRow{
		ID:              aid,
		TenantID:        tenantID,
		ConversationID:  req.Msg.ConversationId,
		Role:            "assistant",
		Content:         responseText,
		ToolCalls:       []byte("[]"),
		ToolResults:     []byte("[]"),
		Attachments:     []byte("[]"),
		Metadata:        metaJSON,
	}
	if _, err := db.CreateMessage(ctx, ttx.Tx, assistantMsg); err != nil {
		ttx.Rollback(ctx)
		return connect.NewError(connect.CodeInternal, fmt.Errorf("save assistant message: %w", err))
	}
	if err := db.UpdateConversationTimestamp(ctx, ttx.Tx, tenantID, req.Msg.ConversationId); err != nil {
		ttx.Rollback(ctx)
		return connect.NewError(connect.CodeInternal, err)
	}
	if conv.Title == "" {
		title := msg
		if len(title) > 80 {
			title = title[:80]
		}
		db.UpdateConversationTitle(ctx, ttx.Tx, tenantID, req.Msg.ConversationId, title)
	}
	if err := ttx.Commit(ctx); err != nil {
		return connect.NewError(connect.CodeInternal, err)
	}

	// --- 6. Send error chunk if there was a streaming error. ---
	if streamErr != nil {
		stream.Send(&apiv1.ChatStreamResponse{
			Event: &apiv1.ChatStreamResponse_Error{
				Error: &apiv1.ErrorChunk{Message: streamErr.Error()},
			},
		})
	}

	// --- 7. Send done signal. ---
	return stream.Send(&apiv1.ChatStreamResponse{
		Event: &apiv1.ChatStreamResponse_Done{
			Done: &apiv1.DoneSignal{
				AssistantMessageId: aid,
				Metadata: &apiv1.MessageMetadata{
					ModelRef:  modelRef,
					LatencyMs: elapsedMS,
				},
			},
		},
	})
}

// preExecuteTools detects tool-callable intents in the user's message,
// executes matching read-only tools, and returns formatted results for
// injection into the prompt. Mutating tools are NOT auto-executed.
// The second return value lists the tool names that were executed, so
// the prompt can tell the model not to re-call them.
func (s *Service) preExecuteTools(ctx context.Context, tenantID, msg string) (string, []string) {
	intents := s.toolRegistry.DetectToolIntents(msg)
	if len(intents) == 0 {
		return "", nil
	}

	var b strings.Builder
	executed := make([]string, 0, len(intents))
	for _, intent := range intents {
		// Scheduling: detect "set number 3 to scheduled" patterns.
		if intent.ToolName == "list_work_items" && isScheduleIntent(msg) {
			s.handleScheduleIntent(ctx, &b, msg)
			executed = append(executed, intent.ToolName)
			continue
		}
		result, err := s.toolRegistry.Execute(ctx, s.pool, intent.ToolName, intent.Args)
		if err != nil {
			b.WriteString(fmt.Sprintf("[%s: error — %s]\n", intent.ToolName, err))
			continue
		}
		resultStr := s.formatToolResult(intent.ToolName, result)
		if len(resultStr) > 32000 {
			resultStr = resultStr[:32000] + "\n...(truncated)"
		}
		b.WriteString(fmt.Sprintf("[%s result]\n%s\n\n", intent.ToolName, resultStr))
		executed = append(executed, intent.ToolName)
	}
	return b.String(), executed
}

func isScheduleIntent(msg string) bool {
	lower := strings.ToLower(msg)
	scheduleWords := []string{"schedule", "scheduled", "set time", "set to scheduled", "schedule for", "scheduled_start"}
	for _, w := range scheduleWords {
		if strings.Contains(lower, w) {
			return true
		}
	}
	return false
}

func extractNumberRef(msg string) int {
	re := regexp.MustCompile(`(?i)(?:number|item|#)\s*(\d+)`)
	matches := re.FindStringSubmatch(msg)
	if len(matches) >= 2 {
		n, err := strconv.Atoi(matches[1])
		if err == nil && n > 0 {
			return n
		}
	}
	return 0
}

func extractTimeStr(msg string) string {
	lower := strings.ToLower(msg)
	// First, normalize word numbers to digits so "five minutes from now"
	// becomes "5 minutes from now".
	normalized := replaceWordNumbers(lower)

	// Look for "N [minutes|hours] from now" patterns (digit or word).
	re := regexp.MustCompile(`(?i)(\d+\s*(?:minutes?|min|m|hours?|hr|h)\s*(?:from\s*now)?)`)
	matches := re.FindStringSubmatch(normalized)
	if len(matches) >= 2 {
		return matches[1]
	}
	// Look for ISO 8601 or time-like patterns.
	isoRe := regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}`)
	if isoRe.MatchString(msg) {
		return isoRe.FindString(msg)
	}
	return ""
}

var wordNumberMap = map[string]string{
	"zero": "0", "one": "1", "two": "2", "three": "3", "four": "4",
	"five": "5", "six": "6", "seven": "7", "eight": "8", "nine": "9",
	"ten": "10", "eleven": "11", "twelve": "12",
}

func replaceWordNumbers(s string) string {
	// Replace word numbers that precede a time unit, e.g. "five minutes" → "5 minutes".
	re := regexp.MustCompile(`(?i)(` + wordNumberPattern() + `)\s*(minutes?|min|m|hours?|hr|h)`)
	return re.ReplaceAllStringFunc(s, func(match string) string {
		parts := re.FindStringSubmatch(match)
		if len(parts) >= 3 {
			if digit, ok := wordNumberMap[strings.ToLower(parts[1])]; ok {
				return digit + " " + parts[2]
			}
		}
		return match
	})
}

func wordNumberPattern() string {
	words := make([]string, 0, len(wordNumberMap))
	for w := range wordNumberMap {
		words = append(words, w)
	}
	sort.Strings(words)
	return strings.Join(words, "|")
}

func (s *Service) handleScheduleIntent(ctx context.Context, b *strings.Builder, msg string) {
	// 1. List work items to get current data.
	itemsRaw, err := s.toolRegistry.Execute(ctx, s.pool, "list_work_items", nil)
	if err != nil {
		b.WriteString(fmt.Sprintf("[list_work_items: error — %s]\n", err))
		return
	}
	// 2. Try to identify the target work item by number reference ("#3", "number 3", "item 3").
	var targetItem map[string]any
	targetNumber := extractNumberRef(msg)

	// 3. Try to parse a time offset ("5 minutes from now").
	timeStr := extractTimeStr(msg)

	if targetNumber > 0 {
		var items []map[string]any
		if err := json.Unmarshal(itemsRaw, &items); err == nil {
			if targetNumber > 0 && targetNumber <= len(items) {
				targetItem = items[targetNumber-1]
			}
		}
	}

	if targetItem != nil {
		// We found the item — execute scheduling directly.
		id, _ := targetItem["ID"].(string)
		title, _ := targetItem["Title"].(string)

		args := map[string]any{"id": id}
		if timeStr != "" {
			args["scheduled_time"] = timeStr
		}
		argsJSON, _ := json.Marshal(args)
		result, execErr := s.toolRegistry.Execute(ctx, s.pool, "schedule_work_item", argsJSON)
		if execErr != nil {
			b.WriteString(fmt.Sprintf("[schedule_work_item error — %s]\n", execErr))
			return
		}
		var resultData map[string]any
		json.Unmarshal(result, &resultData)
		status, _ := resultData["status"].(string)
		b.WriteString(fmt.Sprintf("[schedule_work_item result]\nItem '%s' (%s) has been %s", title, id, status))
		if timeStr != "" {
			b.WriteString(fmt.Sprintf(" with scheduled time: %s", timeStr))
		}
		b.WriteString(".\n\n")
		return
	}

	// Couldn't identify the item — list the data so the model can help.
	resultStr := s.formatToolResult("list_work_items", itemsRaw)
	if len(resultStr) > 32000 {
		resultStr = resultStr[:32000] + "\n...(truncated)"
	}
	b.WriteString(fmt.Sprintf("[list_work_items result]\n%s\n\n", resultStr))
	b.WriteString("The user wants to schedule one of these work items. ")
	b.WriteString("Identify which item they're referring to and call schedule_work_item with its ID.\n\n")
}

// formatToolResult converts raw JSON tool output into a compact, readable
// summary suitable for injection into the LLM prompt. This avoids dumping
// large JSONB blobs (like PromptContext, Conversation, Results) that eat
// up context and get truncated.
func (s *Service) formatToolResult(toolName string, raw json.RawMessage) string {
	switch toolName {
	case "list_work_items", "get_work_item", "create_work_item", "update_work_item":
		return formatWorkItemsSummary(raw)
	case "list_executions", "get_execution":
		return formatExecutionsSummary(raw)
	case "diagnose_failure":
		return formatDiagnosisSummary(raw)
	default:
		str := string(raw)
		if len(str) > 4000 {
			str = str[:4000] + "\n...(truncated)"
		}
		return str
	}
}

// formatWorkItemsSummary strips large JSONB blobs from work item data.
func formatWorkItemsSummary(raw json.RawMessage) string {
	var items []map[string]any
	if err := json.Unmarshal(raw, &items); err != nil {
		// Maybe it's a single item, not a slice.
		var item map[string]any
		if err2 := json.Unmarshal(raw, &item); err2 != nil {
			return string(raw)
		}
		items = []map[string]any{item}
	}
	if len(items) == 0 {
		return "No work items found."
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("Total: %d work items\n\n", len(items)))
	for i, item := range items {
		id, _ := item["ID"].(string)
		title, _ := item["Title"].(string)
		kind, _ := item["Kind"].(string)
		status, _ := item["Status"].(string)
		desc, _ := item["Description"].(string)
		priority, _ := item["Priority"].(float64)
		projectID, _ := item["ProjectID"].(string)

		parentID := "-"
		if item["ParentID"] != nil {
			if pStr, ok := item["ParentID"].(string); ok && pStr != "" {
				parentID = pStr
			}
		}
		b.WriteString(fmt.Sprintf("%d. %s [%s] — %s\n", i+1, title, kind, status))
		b.WriteString(fmt.Sprintf("   ID: %s\n", id))
		b.WriteString(fmt.Sprintf("   Project: %s\n", projectID))
		b.WriteString(fmt.Sprintf("   Parent: %s\n", parentID))
		b.WriteString(fmt.Sprintf("   Priority: %d\n", int(priority)))
		if desc != "" {
			if len(desc) > 200 {
				desc = desc[:200] + "..."
			}
			b.WriteString(fmt.Sprintf("   Description: %s\n", desc))
		}
		b.WriteString("\n")
	}
	return b.String()
}

// formatExecutionsSummary strips large conversation blobs from execution data.
func formatExecutionsSummary(raw json.RawMessage) string {
	var items []map[string]any
	if err := json.Unmarshal(raw, &items); err != nil {
		var item map[string]any
		if err2 := json.Unmarshal(raw, &item); err2 != nil {
			return string(raw)
		}
		items = []map[string]any{item}
	}
	if len(items) == 0 {
		return "No executions found."
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("Total: %d executions\n\n", len(items)))
	for i, item := range items {
		id, _ := item["ID"].(string)
		status, _ := item["Status"].(string)
		health, _ := item["HealthState"].(string)
		workerID, _ := item["WorkerID"].(string)
		taskID, _ := item["TaskID"].(string)
		errorMsg, _ := item["ErrorMessage"].(string)

		b.WriteString(fmt.Sprintf("%d. Execution %s\n", i+1, id))
		b.WriteString(fmt.Sprintf("   Status: %s | Health: %s\n", status, health))
		b.WriteString(fmt.Sprintf("   Worker: %s | Task: %s\n", workerID, taskID))
		if errorMsg != "" {
			if len(errorMsg) > 300 {
				errorMsg = errorMsg[:300] + "..."
			}
			b.WriteString(fmt.Sprintf("   Error: %s\n", errorMsg))
		}
		b.WriteString("\n")
	}
	return b.String()
}

// formatDiagnosisSummary formats diagnosis results compactly.
func formatDiagnosisSummary(raw json.RawMessage) string {
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		return string(raw)
	}
	var b strings.Builder
	if exec, ok := data["execution"].(map[string]any); ok {
		id, _ := exec["id"].(string)
		status, _ := exec["status"].(string)
		errorMsg, _ := exec["error"].(string)
		b.WriteString(fmt.Sprintf("Execution %s\n", id))
		b.WriteString(fmt.Sprintf("  Status: %s\n", status))
		if errorMsg != "" {
			if len(errorMsg) > 500 {
				errorMsg = errorMsg[:500] + "..."
			}
			b.WriteString(fmt.Sprintf("  Error: %s\n", errorMsg))
		}
	}
	if analysis, ok := data["failure_analysis"].([]any); ok {
		b.WriteString("  Analysis:\n")
		for _, a := range analysis {
			if s, ok := a.(string); ok {
				b.WriteString(fmt.Sprintf("    - %s\n", s))
			}
		}
	}
	if failed, ok := data["workflow_executions"].([]any); ok && len(failed) > 0 {
		b.WriteString(fmt.Sprintf("  Failed executions in workflow: %d\n", len(failed)))
	}
	return b.String()
}

// buildLLMPrompt assembles the full text prompt sent to the LLM.
// preExecNames lists tools that were already executed by preExecuteTools —
// the prompt tells the model not to call them again.
func buildLLMPrompt(cfg db.AgentConfigRow, registry *ToolRegistry, history []db.MessageRow, userMsg string, attachments []*apiv1.AttachmentInput, toolResults string, preExecNames []string) string {
	var b strings.Builder

	b.WriteString(BuildSystemPrompt(cfg, registry))
	b.WriteString("\n\n")

	b.WriteString("## Conversation history\n")
	// history is in DESC (newest-first) order from the DB. Reverse it so
	// we can take the LAST N items (which are the most recent in a
	// chronologically-ordered slice).
	for i, j := 0, len(history)-1; i < j; i, j = i+1, j-1 {
		history[i], history[j] = history[j], history[i]
	}
	start := 0
	if len(history) > 10 {
		start = len(history) - 10
	}
	for _, h := range history[start:] {
		if h.Content == "" {
			continue
		}
		roleLabel := "User"
		if h.Role == "assistant" {
			roleLabel = "Orchicon"
		}
		b.WriteString(fmt.Sprintf("%s: %s\n", roleLabel, h.Content))
	}
	b.WriteString("\n")

	if len(attachments) > 0 {
		b.WriteString("## Attachments\n")
		for _, a := range attachments {
			b.WriteString(fmt.Sprintf("File: %s (%s, %d bytes)\n", a.Name, a.MimeType, len(a.Data)))
			if strings.HasPrefix(a.MimeType, "text/") || strings.HasPrefix(a.MimeType, "application/json") || strings.HasSuffix(a.Name, ".md") {
				b.WriteString("```\n" + string(a.Data) + "\n```\n")
			}
		}
		b.WriteString("\n")
	}

	b.WriteString("## Available tools\n")
	for _, td := range registry.List() {
		mutability := "read-only"
		if td.Mutating {
			mutability = "mutates data — requires user confirmation"
		}
		b.WriteString(fmt.Sprintf("- `%s`: %s (%s)\n", td.Name, td.Description, mutability))
	}
	b.WriteString("\nTo call a tool, emit a tool_call in your output with the tool name and JSON arguments. The system will execute it and return the result.\n\n")

	b.WriteString("## User's request\n")
	b.WriteString(userMsg + "\n\n")
	b.WriteString("Note: If the user refers to something mentioned earlier in this conversation (like a work item, project, or result), the details are in the conversation history above — use them directly.\n\n")

	// Pre-executed tool results — injected AFTER the user's request so the
	// model reads the request first, then sees the data was already fetched.
	if toolResults != "" {
		b.WriteString("## Data already retrieved for this request\n")
		b.WriteString("The following data was fetched automatically. Present ALL of it to the user — do not summarize, filter, or omit any entries. If the user asked for a list, list every single item.\n")
		b.WriteString("Items are numbered starting from 1 in the order shown. If the user refers to an item by number (like \"number 3\" or \"#3\"), that refers to the Nth item in this list.\n\n")
		b.WriteString(toolResults)
		b.WriteString("\n")
		// Tell the model which tools were already called so it doesn't
		// re-invoke them through the tool_call mechanism.
		if len(preExecNames) > 0 {
			b.WriteString("These tools have already been called and their results are shown above. ")
			b.WriteString("DO NOT call any of these tools again — use the data already provided:\n")
			for _, name := range preExecNames {
				b.WriteString(fmt.Sprintf("- %s\n", name))
			}
			b.WriteString("\n")
		}
	}

	return b.String()
}

// runOpenCodeStream spawns the opencode CLI subprocess and calls the callback
// for each JSON event as it arrives on stdout. A 120-second hard timeout
// prevents hangs when the model or provider stalls.
func (s *Service) runOpenCodeStream(ctx context.Context, modelRef, prompt, userMessage string, cb streamCallback) (msgID string, elapsed time.Duration, err error) {
	start := time.Now()
	msgID = db.NewID()

	cfgJSON := opencode.BuildConfigContent("orchicon-assistant", prompt, modelRef)

	args := []string{
		"run",
		"--format", "json",
		"--model", modelRef,
		"--agent", "orchicon-assistant",
		"--auto",
		userMessage,
	}

	// Use a configurable timeout so a hanging model never blocks the
	// conversation indefinitely. Default 300s, override via ORCHICON_ASK_TIMEOUT.
	runCtx, cancel := context.WithTimeout(ctx, askTimeout())
	defer cancel()

	cmd := exec.CommandContext(runCtx, "opencode", args...)
	cmd.Env = append(cmd.Environ(),
		"OPENCODE_CONFIG_CONTENT="+cfgJSON,
	)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return msgID, 0, fmt.Errorf("opencode stdout pipe: %w", err)
	}
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		return msgID, 0, fmt.Errorf("opencode start: %w", err)
	}

	scanner := bufio.NewScanner(stdout)
	const maxScannerToken = 512 * 1024 // 512KB — opencode JSON events can be large
	scanner.Buffer(make([]byte, maxScannerToken), maxScannerToken)
	for scanner.Scan() {
		line := scanner.Text()
		var evt opencodeEvent
		if err := json.Unmarshal([]byte(line), &evt); err != nil {
			continue
		}
		if err := cb(evt); err != nil {
			cmd.Process.Kill()
			return msgID, time.Since(start), nil
		}
	}

	scanErr := scanner.Err()
	waitErr := cmd.Wait()
	elapsed = time.Since(start)

	if waitErr != nil {
		if errors.Is(waitErr, context.DeadlineExceeded) || errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			return msgID, elapsed, fmt.Errorf("request timed out after %s — the model may be overloaded or unavailable", askTimeout())
		}
		stderrText := strings.TrimSpace(stderrBuf.String())
		if stderrText != "" {
			return msgID, elapsed, fmt.Errorf("opencode: %s", stderrText)
		}
		return msgID, elapsed, fmt.Errorf("opencode exited: %w", waitErr)
	}
	if scanErr != nil {
		return msgID, elapsed, fmt.Errorf("opencode stdout scan: %w", scanErr)
	}

	return msgID, elapsed, nil
}

func (s *Service) modelRefOrFallback(ctx context.Context, tenantID, convModelRef string) string {
	if convModelRef != "" {
		return convModelRef
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return ""
	}
	defer ttx.Rollback(ctx)
	settings, err := db.GetTenantSettings(ctx, ttx.Tx, tenantID)
	if err != nil {
		return ""
	}
	return settings.DefaultAskOrchiconModel
}
