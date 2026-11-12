package mcpclient

// mcp_legacy_protocol.go — a LEGACY MCP SERVER IS NOT A BROKEN ONE.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// protocolVersionLegacy is the last version before the SEP-2575 `server/discover` gate.
const protocolVersionLegacy = "2025-11-25"

// protocolVersionAuto is the per-server override's "let the client decide" value. An operator who wants the
// probe to be skipped outright sets protocolVersionLegacy instead.
const protocolVersionAuto = "auto"

// pinnedProtocolVersion resolves a spec's pin, or "" when the client should negotiate normally.
func pinnedProtocolVersion(spec ServerSpec) string {
	v := strings.TrimSpace(spec.ProtocolVersion)
	if v == "" || strings.EqualFold(v, protocolVersionAuto) {
		return ""
	}
	return v
}

// THE GATE IS "DID THE CHILD RUN", which is what makes this cheap rather than a blanket
// double-timeout on every broken server.
func shouldRetryWithLegacy(spec ServerSpec, spawn *stdioSpawn, err error) bool {
	if err == nil || spawn == nil {
		return false
	}
	if pinned := pinnedProtocolVersion(spec); pinned != "" && pinned < protocolVersionGate {
		// Already pinned below the gate: this attempt did not use server/discover, so a protocol retry
		// would repeat exactly what just failed.
		return false
	}
	if spawn.cmd == nil || spawn.cmd.Process == nil {
		return false // never exec'd
	}
	return true
}

// protocolVersionGate mirrors the SDK's own boundary (mcp/shared.go: protocolVersion20260728).
const protocolVersionGate = "2026-07-28"

// connectWithLegacyRetry runs one connect attempt with the spec's version, and — when the child RAN
// and failed — a SECOND attempt on a fresh child with the legacy version pinned.
func (m *Manager) connectWithLegacyRetry(ctx context.Context, spec ServerSpec) (*serverConn, error) {
	pinned := pinnedProtocolVersion(spec)
	conn, spawn, firstErr := m.connectAttemptRaw(ctx, spec, pinned)
	if firstErr == nil {
		return conn, nil
	}
	// The spawn comes back WITH the error rather than being stashed on the Manager: it describes THIS
	// attempt, and shared mutable state would let two concurrent starts describe each other's children.
	if !shouldRetryWithLegacy(spec, spawn, firstErr) {
		return nil, firstErr
	}
	m.log.Info("mcp: a server failed the modern handshake — retrying once with the legacy protocol version",
		"server", spec.ID,
		"legacy_version", protocolVersionLegacy,
		"reason", "the server likely predates the server/discover probe (SEP-2575): a legacy server exits "+
			"when it receives a method it does not know",
		"first_error", firstErr)

	conn, _, secondErr := m.connectAttemptRaw(ctx, spec, protocolVersionLegacy)
	if secondErr != nil {
		// BOTH FAILED: report the legacy attempt, since it is the one that got furthest, and keep the first
		// error attached so a reader can see it was tried both ways.
		return nil, &legacyRetryError{first: firstErr, second: secondErr, server: spec.ID}
	}
	m.log.Info("mcp: the server connected with the LEGACY protocol version — it predates the modern "+
		"handshake", "server", spec.ID, "version", protocolVersionLegacy,
		"hint", "set this server's protocol_version to pin it and skip the failed first attempt")
	return conn, nil
}

// legacyRetryError reports both attempts, because the second alone hides that the first was made.
type legacyRetryError struct {
	server string
	first  error
	second error
}

func (e *legacyRetryError) Error() string {
	return "mcp server \"" + e.server + "\": failed with BOTH the modern and the legacy protocol " +
		"handshake — modern: " + e.first.Error() + "; legacy: " + e.second.Error()
}

func (e *legacyRetryError) Unwrap() error { return e.second }

// connectAttemptRaw is ONE connect at ONE protocol version: build a transport (and so, for stdio, a FRESH
// child), handshake, then discover the tool list.
func (m *Manager) connectAttemptRaw(ctx context.Context, spec ServerSpec, protocolVersion string) (*serverConn, *stdioSpawn, error) {
	timeout := connectTimeout(spec)
	// EACH ATTEMPT GETS ITS OWN BUDGET. A first attempt that burns the full window must not leave the retry
	// with nothing — that would turn "the modern probe timed out" into "the legacy handshake never ran".
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var transport mcp.Transport
	var spawn *stdioSpawn
	switch spec.TransportType() {
	case TypeHTTP:
		t, err := newHTTPTransport(spec)
		if err != nil {
			return nil, nil, err
		}
		transport = t
	default:
		sp, err := newStdioTransport(spec)
		if err != nil {
			return nil, nil, err
		}
		spawn = sp
		transport = sp.transport
	}

	client := mcp.NewClient(&mcp.Implementation{
		Name:    "orchicon",
		Version: "0.1.0",
	}, nil)

	// THE PIN. An empty ProtocolVersion makes the SDK use its latest, which probes with server/discover
	// before initialize — correct for a current server and fatal for a legacy one.
	var opts *mcp.ClientSessionOptions
	if protocolVersion != "" {
		opts = &mcp.ClientSessionOptions{ProtocolVersion: protocolVersion}
	}
	started := time.Now()
	session, err := client.Connect(cctx, transport, opts)
	if err != nil {
		// KILL THE CHILD WE JUST SPAWNED, because the SDK does not: mcp.connect returns the handshake error
		// without closing the transport.
		if spawn != nil {
			killStopsTheChild(spawn.cmd)
		}
		elapsed := time.Since(started).Round(time.Millisecond)
		detail := ""
		if spawn != nil {
			detail = truncateForError(spawn.diagnosis())
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, spawn, fmt.Errorf("mcp server %q: connect timed out after %s (%s transport, waited %s, protocol %s)%s: %w",
				spec.ID, timeout, spec.TransportType(), elapsed, versionLabel(protocolVersion), detail, err)
		}
		return nil, spawn, fmt.Errorf("mcp server %q: connect failed after %s (%s transport, protocol %s)%s: %w",
			spec.ID, elapsed, spec.TransportType(), versionLabel(protocolVersion), detail, err)
	}

	res, err := session.ListTools(cctx, &mcp.ListToolsParams{})
	if err != nil {
		_ = session.Close()
		detail := ""
		if spawn != nil {
			detail = truncateForError(spawn.diagnosis())
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, spawn, fmt.Errorf("mcp server %q: tool discovery timed out after %s%s: %w", spec.ID, timeout, detail, err)
		}
		return nil, spawn, fmt.Errorf("mcp server %q: tool discovery failed%s: %w", spec.ID, detail, err)
	}

	conn := &serverConn{spec: spec, client: client, session: session, toolName: make(map[string]string, len(res.Tools))}
	for _, t := range res.Tools {
		if t == nil || t.Name == "" {
			continue
		}
		local := ToolName(spec.ID, t.Name)
		conn.toolName[local] = t.Name
		def := ToolDef{Name: local, Description: t.Description}
		if t.InputSchema != nil {
			schema, serr := json.Marshal(t.InputSchema)
			if serr == nil {
				def.ParamsJSON = string(schema)
			}
		}
		conn.defs = append(conn.defs, def)
	}
	return conn, spawn, nil
}

// versionLabel names the protocol version in a log/error line without printing "protocol :" for an empty pin.
func versionLabel(v string) string {
	if strings.TrimSpace(v) == "" {
		return "negotiated"
	}
	return v
}
