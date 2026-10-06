package tui

// activity_summary_test.go — THE ACTIVITY LINE ALSO REPORTS THE WORK, AND THE REPORT YIELDS TO THE
// WATCHDOG'S VERDICT.
//
// The line has two halves now. The VERB (chat.ActivityVerb, verbs.go) says what the turn is doing to the
// problem and rotates on the server's clock. The SUMMARY (toolclass.SummarizeCalls, summarize.go) says
// what the turn has actually DONE — how many tools it has called inside the rolling window, and how long
// ago the newest one was. This file pins the half that is new, at the layer the operator reads (the PAINTED
// FRAME, the way ask_refresh_test.go:127 asserts), and pins the one rule that matters most:
//
//	ESCALATION OUTRANKS THE COUNTER. A turn that made five calls and then died must escalate, not glow.
//
// The counter comes from data the client ALREADY polls: the durable tool rows arrive on the same
// ListMessages page the transcript is built from (chat/pageToolCalls -> App.onTranscript -> chatStore), on
// the same 1s askTurnPollInterval. So there is no new RPC, no new clock, and no second source of truth.

import (
	"strings"
	"testing"
	"time"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"

	"github.com/beardedparrott/orchicon/internal/toolclass"
	"github.com/beardedparrott/orchicon/internal/tui/chat"
)

// summaryFixture is the tool work these tests count: two modifies and one read, all issued three seconds
// before the frozen "now". It is deliberately a MIXED set so a counter that collapsed every class into one
// bucket would render a different string.
func summaryFixture(now time.Time) []toolclass.Call {
	at := now.Add(-3 * time.Second).UnixMilli()
	return []toolclass.Call{
		{ToolName: "write", AtMs: at},
		{ToolName: "edit", AtMs: at},
		{ToolName: "read", AtMs: at},
	}
}

// freezeNoticeNow pins the activity line's window clock to one instant for the duration of a test, so the
// summary's trailing age is an exact string rather than a race against the wall clock. Same idiom the chat
// controller uses (controller.go: `func now() int64`).
func freezeNoticeNow(t *testing.T, at time.Time) {
	t.Helper()
	prev := noticeNow
	noticeNow = func() time.Time { return at }
	t.Cleanup(func() { noticeNow = prev })
}

// summaryTurnApp builds the Ask pane on a HEALTHY plane with a turn the SERVER reports in flight, which is
// the state in which the activity line is up and the counter is allowed to speak. It deliberately does NOT
// start a stream: the line's "either half" test accepts turn_in_flight, and not opening a socket keeps the
// test free of background goroutines that could repaint under the assertions.
func summaryTurnApp(t *testing.T) *App {
	t.Helper()
	m, _ := askWithTranscript(t, "c1")
	healthyPlane(m)
	m.conversations = []chat.Conversation{{ID: "c1", Title: "a chat", TurnInFly: true, PendingReplyID: "m1"}}
	return m
}

// AC1 — THE LINE REPORTS THE WORK, AND IT REPORTS THE SAME STRING THE SHARED SUMMARIZER PRODUCES.
//
// The expectation is computed by CALLING toolclass.SummarizeCalls over the same calls, so the two cannot
// drift: if the line ever stops deriving from the shared summarizer (a second ad-hoc counter, a different
// window, a different order), this test fails rather than agreeing with the copy.
//
// ASSERTED ON THE PAINTED FRAME, which is the layer the operator's report is true at — the way
// ask_refresh_test.go:127 asserts. A value set on the footer widget can be clipped away by the host's row
// budget and never reach the screen; that is how the thinking indicator shipped twice.
func TestTheLineReportsTheRollingToolSummary(t *testing.T) {
	now := time.UnixMilli(1_700_000_130_000)
	freezeNoticeNow(t, now)

	m := summaryTurnApp(t)
	calls := summaryFixture(now)
	m.chat.SetSilenceForTest("c1", 4*time.Second)
	m.chatStore.setToolCalls("c1", calls)
	m.onChatWake()

	wantSummary := toolclass.SummarizeCalls(calls, now, toolclass.DefaultWindow)
	if wantSummary == "" {
		t.Fatal("fixture: the summarizer produced nothing for a mixed set of calls, so this test would " +
			"assert an empty string is absent — which is true of every line")
	}
	if !strings.Contains(wantSummary, "2 modifies") || !strings.Contains(wantSummary, "1 read") {
		t.Fatalf("fixture: the summarizer no longer renders the mixed counts it is supposed to: %q", wantSummary)
	}

	line := m.askStatusLine()
	if !strings.Contains(line, wantSummary) {
		t.Errorf("the activity line does not carry the rolling tool summary. footer = %q, want it to contain "+
			"%q — the line is supposed to report the WORK, not only the phase", line, wantSummary)
	}
	frame := stripANSI(m.View())
	if !strings.Contains(frame, wantSummary) {
		t.Errorf("the summary is set but missing from the PAINTED frame — the operator cannot see the work "+
			"even though the footer holds it:\n%s", tailOf(frame, 1200))
	}
	// THE VERB IS STILL THERE, IN FRONT OF IT. The counter is ADDITIVE; it never displaces the half that
	// says what the turn is doing (the line's oldest invariant).
	if !strings.Contains(frame, "Orchicon is ") {
		t.Errorf("the summary displaced the verb:\n%s", tailOf(frame, 1200))
	}
}

// AC3 — ESCALATION STILL OUTRANKS EVERYTHING, ALL FOUR STATES IN ONE TEST.
//
// This is the single most important behaviour of the feature. The counter may only appear in the HEALTHY
// band: a turn that made five calls and then died must show the watchdog's verdict, not a reassuring tool
// tally — a count beside "no output for 35s" would read as work still happening, which is the exact false
// claim the escalation bands exist to prevent.
//
// TWO LEGS, because the silence has two clocks and the fourth case has a different one still:
//
//	(a) the PURE FUNCTION leg, which needs no clock at all — the three silence bands, asserted exactly,
//	    including the ABSENCE of the counter from the two escalating ones;
//	(b) the FRAME leg for the disconnected case, driven through the real repaint the way
//	    connection_loss_test.go:120-133 already proves it.
func TestSummaryYieldsToEscalation(t *testing.T) {
	const stamp int64 = activityTestServerTime
	verb := "Orchicon is " + chat.VerbAt(stamp) + "…"
	summary := "3 modifies · 1 read · newest call 4s ago"

	// (a) THE PURE FUNCTION. One summary, three silences, and the band switch decides which text survives.
	cases := []struct {
		name     string
		silent   time.Duration
		want     string
		wantSumm bool
	}{
		{"the healthy band carries the counter", 4 * time.Second, verb + " · " + summary, true},
		{"25s escalates and the counter is GONE", 25 * time.Second, verb + " · no output for 25s", false},
		{"35s re-dials and the counter is GONE", 35 * time.Second,
			verb + " · no output for 35s — the stream will re-attach if it stays silent", false},
	}
	for _, c := range cases {
		got := turnActivityNotice(c.silent, stamp, summary, 0)
		if got != c.want {
			t.Errorf("turnActivityNotice(%v, …, summary, 0) = %q, want %q (%s)", c.silent, got, c.want, c.name)
		}
		// THE YIELD RULE ITSELF, asserted independently of the exact wording: once silence crosses into
		// warnAfter the counter must be ABSENT, not merely outranked. A line that showed both would still
		// be a false claim of work.
		if hasCounter := strings.Contains(got, "modifies"); hasCounter != c.wantSumm {
			t.Errorf("at %v silence the counter is present=%v, want present=%v (line = %q). The counter "+
				"belongs to the HEALTHY band only: escalation is the watchdog's verdict and a tool tally "+
				"beside it reads as work still happening", c.silent, hasCounter, c.wantSumm, got)
		}
	}

	// AND THE BAND STILL WINS AT ANY WIDTH, because fitNotice may only ever REMOVE text — a narrow pane
	// must never resurrect the counter inside an escalation, and must never drop the band before it.
	for _, width := range []int{0, 200, 40, 12} {
		got := turnActivityNotice(35*time.Second, stamp, summary, width)
		if strings.Contains(got, "modifies") {
			t.Errorf("at width %d the re-dial band lost to the counter: %q", width, got)
		}
	}

	// (b) THE FOURTH STATE, ON THE FRAME: a dead plane outranks the turn's own activity line entirely.
	now := time.UnixMilli(1_700_000_130_000)
	freezeNoticeNow(t, now)
	m := summaryTurnApp(t)
	m.chat.SetSilenceForTest("c1", 4*time.Second)
	m.chatStore.setToolCalls("c1", summaryFixture(now))
	healthyPlane(m)
	m.onChatWake()
	if line := m.askStatusLine(); !strings.Contains(line, "modifies") {
		t.Fatalf("fixture: with a healthy plane the counter should be showing, got %q", line)
	}

	deadPlane(m)
	m.onChatWake()
	dead := m.askStatusLine()
	if !strings.Contains(dead, "disconnected") {
		t.Errorf("a dead plane did not take the status slot from the counter — the operator would be told "+
			"work is happening while no reply can arrive. footer = %q", dead)
	}
	if strings.Contains(dead, "modifies") {
		t.Errorf("the disconnected banner and the tool counter are fighting for one row: %q", dead)
	}
	frame := stripANSI(m.View())
	if !strings.Contains(frame, "disconnected") {
		t.Errorf("the disconnection banner never reached the PAINTED frame:\n%s", tailOf(frame, 1200))
	}
	if strings.Contains(frame, "modifies") {
		t.Errorf("the frame carries the counter under a disconnected plane:\n%s", tailOf(frame, 1200))
	}
}

// AC5 — ZERO TOOL CALLS IS NOT "0 modifies".
//
// The summary's empty string is load-bearing (toolclass.Summarize's own doc): with nothing counted the line
// must fall back to exactly what it said before the counter existed — the phase verb and the watchdog age.
// A "0 modifies" would be a false claim that work is happening, which is worse than silence.
func TestZeroToolCallsRendersTheBareVerb(t *testing.T) {
	now := time.UnixMilli(1_700_000_130_000)
	freezeNoticeNow(t, now)

	m := summaryTurnApp(t)
	// The store holds NO tool calls for this conversation — the state of a turn that has not called a tool
	// yet, and of every conversation whose ledger rows predate the stamp field. The silence sits in the
	// HEALTHY band (4s), which is precisely where the counter would appear if there were any.
	m.chat.SetSilenceForTest("c1", 4*time.Second)
	m.chatStore.setToolCalls("c1", nil)
	m.onChatWake()

	frame := stripANSI(m.View())
	if !strings.Contains(frame, "Orchicon is") {
		t.Errorf("with no counted work the phase verb must still be there:\n%s", tailOf(frame, 1200))
	}
	if !strings.Contains(frame, "last activity") {
		t.Errorf("with no counted work the line must fall back to the plain watchdog age:\n%s",
			tailOf(frame, 1200))
	}
	for _, banned := range []string{"0 modifies", "0 reads", "0 bash", "· ·", "  ·"} {
		if strings.Contains(frame, banned) {
			t.Errorf("the line renders %q with zero counted calls — the summary's empty string exists "+
				"precisely so this cannot happen:\n%s", banned, tailOf(frame, 1200))
		}
	}
}

// AC8 — THE SUMMARY NEVER WRAPS OR OVERFLOWS THE ONE ROW, AND IT DEGRADES PREDICTABLY.
//
// The footer is a SINGLE row (screenkit.Detail.footerRows counts "\n") and the host Panel CLIPS its tail.
// The tail is where the escalation band and the counter live, so a naive append on a narrow terminal would
// silently eat the more important text. fitNotice therefore DEGRADES: the summary goes first, the band
// second, and the verb never — mirroring the composer's own documented precedence, where the load-bearing
// text outranks the informative text (internal/tui/dock/statline_test.go:134).
//
// ASSERTED AT TWO WIDTHS, because a one-width test cannot tell "degrades" from "happens to fit". The wide
// pane keeps the counter; the narrow one must drop it and keep the verb.
func TestTheSummaryNeverWrapsTheFooterRow(t *testing.T) {
	now := time.UnixMilli(1_700_000_130_000)
	freezeNoticeNow(t, now)

	type probe struct {
		appWidth     int
		wantCounter  bool
		humanExplain string
	}
	probes := []probe{
		{120, true, "a full-width terminal has room for the verb, the counts and the age"},
		{80, false, "a tight pane must give the row back to the verb rather than clip the pane"},
	}
	for _, p := range probes {
		m := summaryTurnApp(t)
		m.width, m.height = p.appWidth, 40
		m.refreshLayout()
		m.chat.SetSilenceForTest("c1", 4*time.Second)
		m.chatStore.setToolCalls("c1", summaryFixture(now))
		m.onChatWake()

		line := m.askStatusLine()
		if line == "" {
			t.Fatalf("fixture at width %d: no activity line at all", p.appWidth)
		}
		if strings.Contains(line, "\n") {
			t.Errorf("at width %d the activity line WRAPPED (%q) — the footer is one row and a second line "+
				"pushes the notice off the pane (ask/screen.go:293-298)", p.appWidth, line)
		}
		if w := ansi.StringWidth(line); w > m.askWidth() {
			t.Errorf("at width %d the activity line is %d cells wide but the pane's row is %d — an over-wide "+
				"row is CLIPPED AT THE TAIL by the host Panel, which eats the escalation band first: %q",
				p.appWidth, w, m.askWidth(), line)
		}
		// THE VERB NEVER GOES. It is the line's oldest promise (there is an activity line at every stage of
		// the turn) and the half that says what the turn is doing.
		if !strings.Contains(line, "Orchicon is ") || !strings.Contains(line, "…") {
			t.Errorf("at width %d the line lost the verb — the summary must be dropped BEFORE the verb "+
				"(the composer's own model-before-numbers precedence): %q", p.appWidth, line)
		}
		if has := strings.Contains(line, "modifies"); has != p.wantCounter {
			t.Errorf("at width %d the counter is present=%v, want present=%v (%s): %q",
				p.appWidth, has, p.wantCounter, p.humanExplain, line)
		}
		// AND IT IS ON THE FRAME, not only on the widget — a row the renderer clipped would pass every
		// assertion above while being invisible.
		frame := stripANSI(m.View())
		if !strings.Contains(frame, "Orchicon is ") {
			t.Errorf("at width %d the activity line never reached the painted frame:\n%s",
				p.appWidth, tailOf(frame, 1200))
		}
	}

	// THE FLOOR, asserted on the pure function because a pane narrower than the VERB cannot be reached
	// through the app's rail geometry. Three widths below the verb's own cells pin the LAST degradation
	// step: when even the verb does not fit, fitNotice truncates it rather than WRAPPING — a wrapped
	// footer is a SECOND row, which pushes the notice off the pane (ask/screen.go:293-298) and is the
	// failure the one-row budget exists to prevent.
	//
	// The bare-verb arm (silent 0, the first second after send) is included deliberately: it returns
	// through fitNotice too, so "the line never overflows" is true of EVERY state and not only of the
	// states that happen to have something degradable in them.
	const stamp int64 = activityTestServerTime
	verb := "Orchicon is " + chat.VerbAt(stamp) + "…"
	for _, w := range []int{1, 12, len([]rune(verb)) - 1} {
		for _, c := range []struct {
			name    string
			silent  time.Duration
			summary string
		}{
			{"with a summary to drop", 4 * time.Second, "3 modifies · 1 read · newest call 4s ago"},
			{"the bare-verb arm", 0, ""},
			{"the escalation band", 30 * time.Second, "3 modifies · 1 read · newest call 4s ago"},
		} {
			got := turnActivityNotice(c.silent, stamp, c.summary, w)
			if got == "" {
				t.Errorf("at width %d (%s) the line is EMPTY — there is always an activity line during a "+
					"turn", w, c.name)
				continue
			}
			if strings.Contains(got, "\n") {
				t.Errorf("at width %d (%s) the line WRAPPED into a second row, which pushes the notice "+
					"off the pane: %q", w, c.name, got)
			}
			if cells := ansi.StringWidth(got); cells > w {
				t.Errorf("at width %d (%s) the line is %d cells — it OVERFLOWS the one row it is given "+
					"(fitNotice must truncate the verb as its last resort, never let the row wrap): %q",
					w, c.name, cells, got)
			}
		}
	}
	// AND THE ESCALATION BAND STILL WINS AT THAT FLOOR — never the counter — so the truncation cannot
	// resurrect a stale summary inside a stall.
	if got := turnActivityNotice(30*time.Second, stamp, "3 modifies · 1 read · newest call 4s ago", 24); strings.Contains(got, "modifies") {
		t.Errorf("at the width floor the counter survived an escalation: %q", got)
	}
}

// AC9 — THE PLAIN (ascii) PROFILE IS NOT GARBLED.
//
// The rotation's glyph discipline (chat.VerbCellCap, verbs_test.go's TestAskVerbsAreAsciiShortAndUnique) is
// only half the promise: the counter's own separator (·), ellipsis (…), digits and hyphens must also draw on
// a terminal that has no colour and no fancy glyph set. This asserts the RENDERED footer, not the source:
// a character the profile cannot draw would appear as a replacement box or shift the row's width.
func TestTheSummaryIsNotGarbledAtTheAsciiProfile(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	t.Cleanup(func() { lipgloss.SetColorProfile(termenv.Ascii) })

	now := time.UnixMilli(1_700_000_130_000)
	freezeNoticeNow(t, now)

	m := summaryTurnApp(t)
	calls := summaryFixture(now)
	m.chatStore.setToolCalls("c1", calls)
	m.onChatWake()

	line := m.askStatusLine()
	// THE FOOTER IS PLAIN UNDER THE PLAIN PROFILE: no SGR at all, so stripping is a no-op and nothing can be
	// lost to an escape the profile refused to emit.
	if got := stripANSI(line); got != line {
		t.Errorf("the activity line carries escape sequences under the ascii profile: raw %q, stripped %q",
			line, got)
	}
	// AND EVERY RUNE IS ONE THE PROFILE CAN DRAW. The permitted set is exactly what the two halves emit:
	// lowercase a-z and a space (the verb), the middle dot and the ellipsis (the shared separators), digits
	// (the counts and the age), and the hyphen (the re-dial sentence).
	for _, r := range line {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == ' ', r == '·', r == '…', r == '-':
		default:
			t.Errorf("the activity line contains %q under the ascii profile — a glyph the plain terminal "+
				"cannot draw (line = %q)", r, line)
		}
	}
	// The rotation's own list is re-asserted here as the narrow form of the same promise: an entry outside
	// a-z would be the first thing to garble.
	for _, v := range chat.AskVerbs {
		for _, r := range v {
			if r < 'a' || r > 'z' {
				t.Errorf("the rotation carries %q (%q), which the plain profile cannot draw", r, v)
				break
			}
		}
		if strings.ToLower(v) != v {
			t.Errorf("the rotation entry %q is not lowercase, so it does not continue \"Orchicon is \"", v)
		}
	}
	// Guard against the assertion above going vacuous if the counter silently stopped rendering.
	if !strings.Contains(line, "modifies") {
		t.Fatalf("fixture: no counter in the line, so the glyph check above proved nothing: %q", line)
	}
	if !unicode.IsPrint([]rune(line)[0]) {
		t.Errorf("the activity line does not begin with a printable rune: %q", line)
	}
}

// AC10 — NO RPC IS ADDED FOR THE COUNTER; IT RIDES THE POLL THAT ALREADY RUNS.
//
// The claim the feature rests on is "the counter comes from data the client already polls". The mechanical
// form of that claim: a real transcript poll fetches a page, and repainting the line over that page's work
// issues NO FURTHER fetch — while the page with work renders a different line from the page without it. If
// the summary ever grew its own fetch, the count would move between the two repaints and this fails.
func TestTheSummaryAddsNoClientRPC(t *testing.T) {
	now := time.UnixMilli(1_700_000_130_000)
	freezeNoticeNow(t, now)

	m, stub := askWithTranscript(t, "c1")
	healthyPlane(m)
	m.conversations = []chat.Conversation{{ID: "c1", Title: "a chat", TurnInFly: true, PendingReplyID: "m1"}}
	m.chat.SetSilenceForTest("c1", 4*time.Second)

	// ONE REAL POLL, driven the way the controller drives it (listMessages -> TranscriptMsg -> onTranscript).
	// This is the fetch the counter is required to share; anything else it did would show up below.
	runCmdForSummary(t, m, m.chat.Poll("c1"))
	afterPoll := stub.listMessagesCalls
	if afterPoll == 0 {
		t.Fatal("fixture: the poll issued no ListMessages at all, so this test would measure nothing")
	}

	// THE REPAINT OVER A PAGE THAT CARRIED WORK.
	m.chatStore.setToolCalls("c1", summaryFixture(now))
	m.onChatWake()
	withSummary := m.askStatusLine()
	// AND OVER ONE THAT CARRIED NONE. Seeded directly so the ONLY difference between the two lines is the
	// summary — nothing else about the app changed between the repaints.
	m.chatStore.setToolCalls("c1", nil)
	m.onChatWake()
	withoutSummary := m.askStatusLine()

	if stub.listMessagesCalls != afterPoll {
		t.Errorf("rendering the activity line issued %d extra ListMessages call(s) (count %d -> %d) — the "+
			"counter must come from the page the transcript poll already carried, not from a fetch of its "+
			"own", stub.listMessagesCalls-afterPoll, afterPoll, stub.listMessagesCalls)
	}
	if !strings.Contains(withSummary, "modifies") {
		t.Errorf("the page carrying three tool calls rendered no counter: %q", withSummary)
	}
	if strings.Contains(withoutSummary, "modifies") {
		t.Errorf("the page carrying no tool calls still rendered a counter: %q", withoutSummary)
	}
}

// runCmdForSummary runs one command tree for the AC10 test: the command, then the TranscriptMsg it
// produces, through the shell's own router (so onTranscript runs exactly as it does in the program). It
// deliberately runs only the FIRST command and its message — enough to observe the fetch count, and short
// enough that a re-arming tick cannot spin.
func runCmdForSummary(t *testing.T, m *App, cmd tea.Cmd) {
	t.Helper()
	if cmd == nil {
		return
	}
	msg := cmd()
	if msg == nil {
		return
	}
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, c := range batch {
			runCmdForSummary(t, m, c)
		}
		return
	}
	nm, _ := m.Update(msg)
	if m2, ok := nm.(*App); ok {
		*m = *m2
	}
}

// AC4 — THE VERB IN THE FRAME IS THE ONE THE ROTATION NAMES FOR THE LAST HEARTBEAT'S SERVER TIME.
//
// The rotation is already merged (child 3, PR #655) and its selector is pure over the server stamp the
// heartbeat carries. What this asserts is the WIRING at the line: the stamp recorded from a heartbeat is
// the one the footer's word is derived from — a heartbeat that records nothing, or a line that indexes on
// the local clock instead, would leave the word frozen or disagreeing between clients.
func TestTheVerbInTheFrameComesFromTheLastHeartbeat(t *testing.T) {
	now := time.UnixMilli(1_700_000_130_000)
	freezeNoticeNow(t, now)

	m := summaryTurnApp(t)
	m.chat.SetSilenceForTest("c1", 4*time.Second)
	m.chatStore.setToolCalls("c1", summaryFixture(now))

	// BEFORE ANY HEARTBEAT: the pre-heartbeat fallback, the list's first word — the state of the first
	// second after sending, which is when the operator is most likely to be looking.
	m.onChatWake()
	if want := "Orchicon is " + chat.VerbAt(0) + "…"; !strings.Contains(m.askStatusLine(), want) {
		t.Fatalf("with no heartbeat the line must show the fallback word %q, got %q", want, m.askStatusLine())
	}

	// A HEARTBEAT PLANTS THE STAMP (the same pair of writes handleEvent's Heartbeat arm makes).
	m.chat.SetServerTimeForTest("c1", activityTestServerTime)
	m.onChatWake()
	want := "Orchicon is " + chat.VerbAt(activityTestServerTime) + "…"
	frame := stripANSI(m.View())
	if !strings.Contains(frame, want) {
		t.Errorf("the painted frame does not carry the word the rotation names for the heartbeat's server "+
			"time. want %q in:\n%s", want, tailOf(frame, 1200))
	}

	// AND A LATER HEARTBEAT MOVES IT. One period on from the first stamp is exactly the pair the selector's
	// contract says must differ, so a line that ignores the recorded stamp cannot pass both halves.
	later := activityTestServerTime + chat.VerbPeriodMS
	if chat.VerbAt(later) == chat.VerbAt(activityTestServerTime) {
		t.Fatalf("fixture: VerbAt gives the same word for %d and %d, so this test cannot detect a frozen word",
			activityTestServerTime, later)
	}
	m.chat.SetServerTimeForTest("c1", later)
	m.onChatWake()
	if wantLater := "Orchicon is " + chat.VerbAt(later) + "…"; !strings.Contains(m.askStatusLine(), wantLater) {
		t.Errorf("a new heartbeat did not move the word: got %q, want it to carry %q",
			m.askStatusLine(), wantLater)
	}
}
