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
	"strings"
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

	// Inject the tenant's enabled projects so the agent always has
	// up-to-date context about what it operates on (fresh per message).
	projectContext := s.fetchProjectContext(ctx, tenantID)

	fullPrompt := buildLLMPrompt(cfg, s.toolRegistry, prevMessages, msg, req.Msg.Attachments, projectContext)

	// --- 4. Stream from opencode, stripping tool call markers.
	// The frontend shows its own thinking indicator during the initial
	// latency (opencode startup + model first token). No backend progress
	// messages are needed — they would render as markdown blockquotes in
	// the streaming content and cause visual artifacts.
	var fullResponse strings.Builder

	cb := func(evt opencodeEvent) error {
		switch evt.Type {
		case "text":


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
			// opencode executed the tool itself (a round-trip through the
			// Orchicon MCP server for orchicon_* tools, or a user-configured
			// MCP server / built-in tool). This callback only relays the
			// call + result to the UI — it must NOT re-execute the tool,
			// which would double-run mutations.
			toolName, _ := evt.Part["tool"].(string)
			state, _ := evt.Part["state"].(map[string]any)
			inRaw, _ := state["input"]
			inputJSON, _ := json.Marshal(inRaw)
			outStr, _ := state["output"].(string)
			status, _ := state["status"].(string)
			s.log.Info("opencode tool use", "tool", toolName, "status", status, "input", string(inputJSON))
			stream.Send(&apiv1.ChatStreamResponse{
				Event: &apiv1.ChatStreamResponse_ToolCallResult{
					ToolCallResult: &apiv1.ToolCallResult{
						ToolCallId: toolName,
						Output:     outStr,
						IsError:    status == "error",
					},
				},
			})
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

	msgID, elapsed, streamErr := s.runOpenCodeStream(ctx, tenantID, modelRef, fullPrompt, msg, cb)

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

// buildLLMPrompt assembles the full text prompt sent to the LLM. The tools
// are exposed to the model natively through the Orchicon MCP server (registered
// in the opencode config), so this prompt only orients the model — it does not
// emulate a text tool-call protocol.
func buildLLMPrompt(cfg db.AgentConfigRow, registry *ToolRegistry, history []db.MessageRow, userMsg string, attachments []*apiv1.AttachmentInput, projectContext string) string {
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

	b.WriteString("## Enabled projects\n")
	if projectContext != "" {
		b.WriteString(projectContext)
		b.WriteString("\n")
	} else {
		b.WriteString("None yet — use the create_project tool to create one.\n")
	}
	b.WriteString("\n")

	b.WriteString("## Available tools\n")
	b.WriteString("Orchicon's tools are exposed to you as MCP tools named `orchicon_<tool>` — call them directly through your tool mechanism and the system executes them against Orchicon, returning real results. Mutating tools run only after user confirmation.\n\n")
	for _, td := range registry.List() {
		mutability := "read-only"
		if td.Mutating {
			mutability = "mutates data — requires user confirmation"
		}
		b.WriteString(fmt.Sprintf("- `orchicon_%s`: %s (%s)\n", td.Name, td.Description, mutability))
	}
	b.WriteString("\n")

	b.WriteString("## User's request\n")
	b.WriteString(userMsg + "\n\n")
	b.WriteString("Note: If the user refers to something mentioned earlier in this conversation (like a work item, project, or result), the details are in the conversation history above — use them directly.\n\n")

	return b.String()
}

// runOpenCodeStream spawns the opencode CLI subprocess and calls the callback
// for each JSON event as it arrives on stdout. A configurable hard timeout
// prevents hangs when the model or provider stalls. The Orchicon MCP server is
// registered in the injected config (tenant-scoped) so the model drives
// Orchicon tools natively through opencode's MCP integration.
func (s *Service) runOpenCodeStream(ctx context.Context, tenantID, modelRef, prompt, userMessage string, cb streamCallback) (msgID string, elapsed time.Duration, err error) {
	start := time.Now()
	msgID = db.NewID()

	cfgJSON := opencode.BuildConfigContent(opencode.ConfigOptions{
		AgentName:   "orchicon-assistant",
		AgentPrompt: prompt,
		ModelRef:    modelRef,
		TenantID:    tenantID,
		OrchiconMCP: true,
	})

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
	// Place the opencode subprocess in its own process group so that when
	// the parent server dies unexpectedly (e.g. SIGKILL during binary
	// replacement), the orphaned opencode and its MCP sidecar can be
	// found and cleaned up by the startup routine. The group leader PID
	// is the subprocess PID; we can kill the whole group with
	// syscall.Kill(-pgid, sig). Unix-only (Setpgid does not exist on
	// Windows) — see procattr_{unix,windows}.go.
	setChildProcessGroup(cmd)

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

// fetchProjectContext returns a compact, current list of the tenant's
// enabled projects (title, status, directory, goals) for injection into the
// prompt, so the agent always knows what it operates on. Best-effort — a
// failure yields an empty string rather than blocking the conversation.
func (s *Service) fetchProjectContext(ctx context.Context, tenantID string) string {
	raw, err := s.toolRegistry.Execute(ctx, s.pool, "list_projects", nil)
	if err != nil {
		return ""
	}
	var projects []map[string]any
	if err := json.Unmarshal(raw, &projects); err != nil || len(projects) == 0 {
		return ""
	}
	var b strings.Builder
	for _, p := range projects {
		name, _ := p["Name"].(string)
		if name == "" {
			name, _ = p["name"].(string)
		}
		status, _ := p["Status"].(string)
		if status == "" {
			status, _ = p["status"].(string)
		}
		dir, _ := p["ProjectDir"].(string)
		if dir == "" {
			dir, _ = p["project_dir"].(string)
		}
		goals, _ := p["Goals"].(string)
		if goals == "" {
			goals, _ = p["goals"].(string)
		}
		id, _ := p["ID"].(string)
		if id == "" {
			id, _ = p["id"].(string)
		}
		b.WriteString(fmt.Sprintf("- %s (ID %s, status %s", name, id, status))
		if dir != "" {
			b.WriteString(fmt.Sprintf(", dir %s", dir))
		}
		b.WriteString(")")
		if goals != "" {
			g := goals
			if len(g) > 160 {
				g = g[:160] + "..."
			}
			b.WriteString(fmt.Sprintf(" — %s", g))
		}
		b.WriteString("\n")
	}
	return b.String()
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
