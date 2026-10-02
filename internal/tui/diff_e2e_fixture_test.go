// Package tui — diff_e2e_fixture_test.go — the fixture ledger and disposable
// plane for diff_e2e_pty_test.go, plus a minimal terminal emulator that turns
// the live process's byte stream into the SCREEN GRID the operator actually
// sees.
//
// The emulator is what makes the geometry claims in the acceptance review
// honest: a claim like "the long line's tail is reachable" or "the pane is N
// cells wide" is asserted on the replayed grid, not on a struct field and not
// by a substring search over raw escape bytes (bubbletea's line-skip renderer
// makes a full frame unreliable to substring-match).
package tui

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"connectrpc.com/connect"
	"github.com/charmbracelet/x/ansi"
	"github.com/creack/pty"

	v1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1/apiv1connect"
)

// ---------------------------------------------------------------------------
// Fixture ledger
// ---------------------------------------------------------------------------

// diffE2EFile is one fixture file: a path plus a long, multi-hunk unified diff.
type diffE2EFile struct {
	Path string
	// Marker is a short contiguous token painted by this file's diff body.
	Marker string
	Diff   string
	// Tail is the final rune-run of the diff's deliberately long added line —
	// the substring defect 3 asserts is REACHABLE. A wrapping (fixed) pane
	// keeps it; a truncating (broken) pane destroys it.
	Tail string
}

// diffE2EBuildDiff builds a unified diff with hunkCount hunks, each adding
// lineCount lines. Hunk 0 carries two witness lines NEAR THE TOP so a fit
// assertion can read them off the initial frame without scrolling:
//
//   - diffE2EMarker: a short contiguous token (no wrap) proving the Diff body
//     is showing this file's diff — the click-through witness;
//   - the long (>100 column) tail line proving nothing is truncated.
func diffE2EBuildDiff(path, marker string, hunkCount, lineCount int) (string, string) {
	var b strings.Builder
	fmt.Fprintf(&b, "--- a/%s\n+++ b/%s\n", path, path)
	longTail := "TAILWITNESS_" + strings.Repeat("0123456789", 11) + "TAILEND"
	for h := 0; h < hunkCount; h++ {
		start := h*100 + 1
		fmt.Fprintf(&b, "@@ -%d,%d +%d,%d @@\n", start, 2, start, lineCount+2)
		b.WriteString(" context-line-alpha\n")
		b.WriteString(" context-line-beta\n")
		if h == 0 {
			fmt.Fprintf(&b, "+%s\n", marker)
			fmt.Fprintf(&b, "+%s\n", longTail)
		}
		for i := 0; i < lineCount; i++ {
			fmt.Fprintf(&b, "+added-%s-h%02d-l%03d\n", pathBase(path), h, i)
		}
	}
	return b.String(), longTail
}

// diffE2EMarkers are short, per-file contiguous tokens. Each is deliberately
// narrower than the minimum pane body so it never wraps — the reliable witness
// that the Diff tab is showing THAT file's diff (a wrapped path or a long
// content line is not).
var diffE2EMarkers = map[string]string{
	"cmd/first.go":       "WITFIRST",
	"internal/second.go": "WITSECND",
}

func pathBase(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

// diffE2EFixtureFiles returns two distinct files, each >= 6 hunks.
func diffE2EFixtureFiles() []diffE2EFile {
	var out []diffE2EFile
	for _, p := range []string{"cmd/first.go", "internal/second.go"} {
		m := diffE2EMarkers[p]
		d, tail := diffE2EBuildDiff(p, m, 6, 48)
		out = append(out, diffE2EFile{Path: p, Marker: m, Diff: d, Tail: tail})
	}
	return out
}

// diffE2ELedger converts fixture files to ledger rows for one owner tuple.
func diffE2ELedger(kind, id string, files []diffE2EFile) []*v1.FileEdit {
	edits := make([]*v1.FileEdit, 0, len(files))
	for i, f := range files {
		edits = append(edits, &v1.FileEdit{
			Id:          fmt.Sprintf("%s-%s-%d", kind, id, i+1),
			OwnerKind:   kind,
			OwnerId:     id,
			Seq:         int64(i + 1),
			Path:        f.Path,
			Kind:        "modify",
			UnifiedDiff: f.Diff,
			Tool:        "write",
		})
	}
	return edits
}

// diffE2EFileEditService is the fixture FileEditService. It serves the fixture
// ledger for any owner tuple and can be switched into the two honest-state
// variants: an empty ledger and a failing fetch.
type diffE2EFileEditService struct {
	apiv1connect.UnimplementedFileEditServiceHandler
	files []diffE2EFile
	empty bool
	fail  bool
}

func (s *diffE2EFileEditService) GetSessionFileEdits(_ context.Context, req *connect.Request[v1.GetSessionFileEditsRequest]) (*connect.Response[v1.GetSessionFileEditsResponse], error) {
	if s.fail {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("fixture ledger fetch failure"))
	}
	if s.empty {
		return connect.NewResponse(&v1.GetSessionFileEditsResponse{MaxSeq: 0}), nil
	}
	kind, id := req.Msg.GetOwnerKind(), req.Msg.GetOwnerId()
	return connect.NewResponse(&v1.GetSessionFileEditsResponse{
		Edits:  diffE2ELedger(kind, id, s.files),
		MaxSeq: int64(len(s.files)),
	}), nil
}

// diffE2EAskService serves the conversations rail + a conversation transcript.
type diffE2EAskService struct {
	apiv1connect.UnimplementedAskOrchiconServiceHandler
}

func (diffE2EAskService) ListConversations(context.Context, *connect.Request[v1.ListConversationsRequest]) (*connect.Response[v1.ListConversationsResponse], error) {
	return connect.NewResponse(&v1.ListConversationsResponse{Conversations: []*v1.Conversation{
		{Id: diffE2EConvID, Title: "diff-e2e-conv", MessageCount: 2, ProjectId: diffE2EProject},
	}}), nil
}

func (diffE2EAskService) ListMessages(_ context.Context, req *connect.Request[v1.ListMessagesRequest]) (*connect.Response[v1.ListMessagesResponse], error) {
	return connect.NewResponse(&v1.ListMessagesResponse{Messages: []*v1.ChatMessage{
		{Id: "m1", ConversationId: req.Msg.GetConversationId(), Role: "user", Content: "please edit a file"},
	}}), nil
}

func (diffE2EAskService) GetConversation(_ context.Context, req *connect.Request[v1.GetConversationRequest]) (*connect.Response[v1.GetConversationResponse], error) {
	return connect.NewResponse(&v1.GetConversationResponse{Conversation: &v1.Conversation{
		Id: req.Msg.GetId(), Title: "diff-e2e-conv", MessageCount: 2,
	}}), nil
}

// diffE2EExecutionService serves one execution row so the Execution mount has a
// real owner to resolve.
type diffE2EExecutionService struct {
	apiv1connect.UnimplementedExecutionServiceHandler
}

func (diffE2EExecutionService) ListExecutions(context.Context, *connect.Request[v1.ListExecutionsRequest]) (*connect.Response[v1.ListExecutionsResponse], error) {
	return connect.NewResponse(&v1.ListExecutionsResponse{Executions: []*v1.WorkerExecution{
		{Id: diffE2EExecID, Status: v1.ExecutionStatus_EXECUTION_STATUS_RUNNING},
	}}), nil
}

func (diffE2EExecutionService) GetExecution(_ context.Context, req *connect.Request[v1.GetExecutionRequest]) (*connect.Response[v1.GetExecutionResponse], error) {
	return connect.NewResponse(&v1.GetExecutionResponse{Execution: &v1.WorkerExecution{
		Id: req.Msg.GetId(), Status: v1.ExecutionStatus_EXECUTION_STATUS_RUNNING,
	}}), nil
}

// diffE2EAuthService answers the tenant resolution + identity probe.
type diffE2EAuthService struct {
	apiv1connect.UnimplementedAuthServiceHandler
}

func (diffE2EAuthService) ListIdentities(context.Context, *connect.Request[v1.ListIdentitiesRequest]) (*connect.Response[v1.ListIdentitiesResponse], error) {
	return connect.NewResponse(&v1.ListIdentitiesResponse{Identities: []*v1.Identity{
		{Id: "id1", TenantId: diffE2ETenant, DisplayName: "e2e"},
	}}), nil
}

// The pane's shell chrome geometry (the diffs package's unexported paneTopRow /
// paneBodyRow contract): terminal row 3 is the pane's tab bar, row 4 is its
// first body row. Duplicated here so this gate clicks the REAL coordinates.
const (
	diffPaneTopRow  = 3
	diffPaneBodyRow = 4
)

// The scrollbar glyphs the pane paints (internal/tui/diffs/scrollbar.go) —
// duplicated here because that package's constants are unexported and this gate
// asserts on what the REAL process paints, not on importing the renderer.
const (
	diffScrollThumbGlyph = "█"
	diffScrollTrackGlyph = "│"
)

const (
	diffE2EConvID  = "conv-diff-e2e"
	diffE2EExecID  = "exec-diff-e2e"
	diffE2ETenant  = "tnt-diff-e2e"
	diffE2EProject = "p1"
)

// diffE2EPlane starts the disposable plane and returns its base URL.
func diffE2EPlane(t *testing.T, fe *diffE2EFileEditService) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/versionz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"version":"v9.9.9-diffe2e"}`))
	})
	// The project carries the DIRECTORY of the test process so the launch-time
	// project prompt stays quiet (see connectPlaneFixture/mousePlaneFixture).
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	pp, ph := apiv1connect.NewProjectServiceHandler(&ptyProjects{dir: cwd})
	mux.Handle(pp, ph)
	ap, ah := apiv1connect.NewAskOrchiconServiceHandler(diffE2EAskService{})
	mux.Handle(ap, ah)
	ep, eh := apiv1connect.NewExecutionServiceHandler(diffE2EExecutionService{})
	mux.Handle(ep, eh)
	fp, fh := apiv1connect.NewFileEditServiceHandler(fe)
	mux.Handle(fp, fh)
	authPath, authHandler := apiv1connect.NewAuthServiceHandler(diffE2EAuthService{})
	mux.Handle(authPath, authHandler)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

// ---------------------------------------------------------------------------
// Minimal terminal emulator
// ---------------------------------------------------------------------------

// screen replays a live pty byte stream into a rows×cols grid — the cells the
// terminal would actually show. It handles the subset of ANSI bubbletea's
// alt-screen renderer emits: cursor positioning (CUP/HVP), line/display erase,
// carriage return / line feed, backspace, and printable runes (width-aware).
type screen struct {
	cols, rows int
	grid       [][]rune
	curR, curC int
}

func newScreen(cols, rows int) *screen {
	g := make([][]rune, rows)
	for i := range g {
		g[i] = make([]rune, cols)
		for j := range g[i] {
			g[i][j] = ' '
		}
	}
	return &screen{cols: cols, rows: rows, grid: g}
}

// row returns row r as a string (right-trimmed of padding).
func (s *screen) row(r int) string {
	if r < 0 || r >= s.rows {
		return ""
	}
	return strings.TrimRight(string(s.grid[r]), " ")
}

// col returns column c across all rows as a string.
func (s *screen) col(c int) string {
	if c < 0 || c >= s.cols {
		return ""
	}
	var b strings.Builder
	for r := 0; r < s.rows; r++ {
		b.WriteRune(s.grid[r][c])
	}
	return b.String()
}

func (s *screen) put(r rune, w int) {
	if s.curR >= 0 && s.curR < s.rows && s.curC >= 0 && s.curC < s.cols {
		s.grid[s.curR][s.curC] = r
		for k := 1; k < w && s.curC+k < s.cols; k++ {
			s.grid[s.curR][s.curC+k] = ' '
		}
	}
	s.curC += w
	if s.curC >= s.cols {
		s.curC = s.cols - 1 // wrap is OFF (disableAutoWrap)
	}
}

func (s *screen) eraseLineRight() {
	if s.curR < 0 || s.curR >= s.rows {
		return
	}
	for c := s.curC; c < s.cols; c++ {
		if c >= 0 {
			s.grid[s.curR][c] = ' '
		}
	}
}

func (s *screen) eraseDisplay() {
	for r := range s.grid {
		for c := range s.grid[r] {
			s.grid[r][c] = ' '
		}
	}
}

// feed replays raw pty bytes into the grid.
func (s *screen) feed(data string) {
	i := 0
	for i < len(data) {
		c := data[i]
		switch {
		case c == 0x1b:
			i = s.consumeEscape(data, i)
		case c == '\r':
			s.curC = 0
			i++
		case c == '\n':
			// The pty has ONLCR, so a bare \n arrives as CR+LF; treat it as such.
			s.curR++
			s.curC = 0
			if s.curR >= s.rows {
				s.curR = s.rows - 1
			}
			i++
		case c == '\b':
			if s.curC > 0 {
				s.curC--
			}
			i++
		case c == '\t':
			s.curC = (s.curC/8 + 1) * 8
			if s.curC >= s.cols {
				s.curC = s.cols - 1
			}
			i++
		case c < 0x20:
			i++
		default:
			r, size := utf8.DecodeRuneInString(data[i:])
			s.put(r, ansi.StringWidth(string(r)))
			i += size
		}
	}
}

// consumeEscape consumes one ESC-introduced sequence starting at data[i]
// (data[i] == 0x1b) and returns the index past it.
func (s *screen) consumeEscape(data string, i int) int {
	if i+1 >= len(data) {
		return len(data)
	}
	switch data[i+1] {
	case '[': // CSI
		j := i + 2
		for j < len(data) && data[j] >= 0x20 && data[j] <= 0x3f {
			j++
		}
		if j >= len(data) {
			return len(data)
		}
		final := data[j]
		params := data[i+2 : j]
		s.csi(final, params)
		return j + 1
	case ']': // OSC — terminated by BEL or ST (ESC \)
		j := i + 2
		for j < len(data) {
			if data[j] == 0x07 {
				return j + 1
			}
			if data[j] == 0x1b && j+1 < len(data) && data[j+1] == '\\' {
				return j + 2
			}
			j++
		}
		return len(data)
	default:
		// Two-byte escapes (ESC =, ESC >, ESC (B, …): skip prefix + one byte.
		j := i + 1
		if data[j] >= 0x20 && data[j] <= 0x2f {
			j++ // intermediate
		}
		if j < len(data) {
			j++
		}
		return j
	}
}

func (s *screen) csi(final byte, params string) {
	switch final {
	case 'H', 'f': // CUP — row;col (1-based; omitted → 1)
		r, c := parseTwo(params)
		if r < 1 {
			r = 1
		}
		if c < 1 {
			c = 1
		}
		s.curR = min2(r-1, s.rows-1)
		s.curC = min2(c-1, s.cols-1)
	case 'A': // CUU
		n := parseOne(params)
		s.curR = max2(0, s.curR-n)
	case 'B': // CUD
		n := parseOne(params)
		s.curR = min2(s.rows-1, s.curR+n)
	case 'C': // CUF
		n := parseOne(params)
		s.curC = min2(s.cols-1, s.curC+n)
	case 'D': // CUB
		n := parseOne(params)
		s.curC = max2(0, s.curC-n)
	case 'G': // CHA (column)
		n := parseOne(params)
		if n > 0 {
			s.curC = min2(n-1, s.cols-1)
		}
	case 'K': // EL — 0=right, 1=left, 2=all
		switch parseOne(params) {
		case 1:
			for c := 0; c <= s.curC && c < s.cols; c++ {
				s.grid[s.curR][c] = ' '
			}
		case 2:
			for c := 0; c < s.cols; c++ {
				s.grid[s.curR][c] = ' '
			}
		default:
			s.eraseLineRight()
		}
	case 'J': // ED — 0=below, 2=all
		s.eraseDisplay()
	}
}

func parseOne(params string) int {
	params = strings.TrimPrefix(params, "?")
	if params == "" {
		return 0
	}
	if i := strings.IndexByte(params, ';'); i >= 0 {
		params = params[:i]
	}
	n, err := strconv.Atoi(params)
	if err != nil || n <= 0 {
		return 1 // an omitted numeric parameter defaults to 1
	}
	return n
}

func parseTwo(params string) (int, int) {
	params = strings.TrimPrefix(params, "?")
	parts := strings.SplitN(params, ";", 2)
	r, _ := strconv.Atoi(parts[0])
	c := 0
	if len(parts) == 2 {
		c, _ = strconv.Atoi(parts[1])
	}
	return r, c
}

func min2(a, b int) int {
	if a < b {
		return a
	}
	return b
}
func max2(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// startOrchPtySized is startOrchPtyAt with an explicit pty size (the shared
// helper hardcodes 120x40). It mirrors that helper's env isolation, and pins
// ORCHICON_CONFIG_DIR to THIS launch's home so the rail-width persistence is
// per-launcher (a restart reusing the same home restores it; other tests never
// see it) rather than the package-wide test dir.
func startOrchPtySized(t *testing.T, bin, planeURL, home string, cols, rows int) *ptySession {
	t.Helper()
	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(),
		"ORCHICON_URL="+planeURL,
		"ORCHICON_TOKEN=oc_diff_e2e",
		"TERM=xterm-256color",
		"HOME="+home,
		// The TUI resolves the config path from ORCHICON_CONFIG_DIR when set
		// (config.DefaultPath). The package init() points it at a shared temp dir;
		// override it per-launch so a persisted rail width is scoped to `home`.
		"ORCHICON_CONFIG_DIR="+filepath.Join(home, ".orchicon"),
	)
	tty, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
	if err != nil {
		t.Fatalf("pty start: %v", err)
	}
	s := &ptySession{cmd: cmd, tty: tty}
	go s.readLoop()
	return s
}
