package chat

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/beardedparrott/orchicon/internal/adapter"
	"github.com/beardedparrott/orchicon/internal/tui/md"
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
	"github.com/beardedparrott/orchicon/internal/tui/theme"
)

// view.go renders grouped ChatItems as themed terminal rows — the TUI ToolCard/ArtifactCard
// equivalents: one row per tool call / artifact.

// SessionIdentity renders a KindSession item's transport identity for display: the adapter the
// execution ran on plus the session id, and the serve it ran on when there is one.
func SessionIdentity(it ChatItem) string {
	kind := it.AdapterKind
	if kind == "" {
		// Legacy transcript written before adapter identity was recorded:
		// opencode was the only session adapter then.
		kind = adapter.KindOpencode
	}
	id := it.SessionID
	if id == "" {
		id = "(session)"
	}
	if kind == adapter.KindOrchicon {
		return "session " + id + " · native · in-process"
	}
	meta := "session " + id + " · " + kind
	if it.ServeURL != "" {
		meta += " · " + it.ServeURL
	}
	return meta
}

// RenderItems renders the items into lines clamped to maxWidth (0 = unlimited).
type ItemSpan struct {
	Kind ItemKind
	Key  string
	// MessageID is the durable message this item came from, empty when it is not a persisted message.
	MessageID string
	Text      string // the item's copyable text ("" when there is nothing worth copying)
	Line      int    // 0-based first line of this item in the body
	Lines     int    // how many body lines it occupies
	// Options is where an ask card's OPTION ROWS landed, in lines RELATIVE to Line, each with the label
	// a click on it sends back as the next user message.
	Options []AskOptionSpan
	// Code is where this item's CODE BLOCKS landed, in lines RELATIVE to Line, each with the fence's
	// source.
	Code []CodeSpan
}

// AskOptionSpan is one option row of a clarifying-question card within its item, and its label.
type AskOptionSpan struct {
	Line  int // 0-based line of the option's first row, relative to the item
	Lines int // how many rows the option occupies (its label, plus its description when it has one)
	Label string
}

// CodeSpan is one code block's place within an item, and the code itself.
type CodeSpan struct {
	Line   int // 0-based line of the block's first row, relative to the item
	Lines  int // how many lines it occupies, label row included
	Lang   string
	Source string // the fence's contents, verbatim
}

// Contains reports whether a body line belongs to this item.
func (s ItemSpan) Contains(line int) bool {
	return line >= s.Line && line < s.Line+s.Lines
}

// OptionAt returns the LABEL of the ask card's option at a body line, when one is there.
func (s ItemSpan) OptionAt(line int) (string, bool) {
	rel := line - s.Line
	for _, o := range s.Options {
		if rel >= o.Line && rel < o.Line+o.Lines {
			return o.Label, true
		}
	}
	return "", false
}

// CodeAt returns the SOURCE of the code block containing a body line, when one does.
func (s ItemSpan) CodeAt(line int) (string, bool) {
	rel := line - s.Line
	for _, c := range s.Code {
		if rel >= c.Line && rel < c.Line+c.Lines {
			return c.Source, true
		}
	}
	return "", false
}

// copyTextFor is what a click on this item should put on the clipboard.
func copyTextFor(it ChatItem) string {
	switch it.Kind {
	case KindUser, KindText, KindReasoning, KindError:
		return it.Text
	case KindConsent:
		// A SETTLED ask is worth copying (it is the record of a decision); a PENDING one is not — copying a
		// question that has not been answered would paste the card's own furniture back into a message.
		if it.Consent != nil && !it.Consent.Pending() {
			return consentRecord(it.Consent)
		}
	}
	return ""
}

// RenderItemsSpans renders the transcript AND reports where each item's lines landed.
func RenderItemsSpans(items []ChatItem, maxWidth int, collapse ...func(key string) bool) (string, []ItemSpan) {
	folded := func(string) bool { return false }
	if len(collapse) > 0 && collapse[0] != nil {
		folded = collapse[0]
	}
	return renderItems(items, maxWidth, folded, "", nil, "", "")
}

// CopyGlyph marks what the operator can CLICK TO COPY in the transcript.
const CopyGlyph = "⧉"

// RenderItemsSpansWithCopy is RenderItemsSpans drawn for a surface where clicking a message or a
// code block copies it, so both say so.
func RenderItemsSpansWithCopy(items []ChatItem, maxWidth int, copyGlyph string, collapse func(key string) bool) (string, []ItemSpan) {
	return renderItems(items, maxWidth, collapse, copyGlyph, nil, "", "")
}

// renderItems is the shared body: the two entry points differ only in whether a copy affordance is drawn.
func renderItems(items []ChatItem, maxWidth int, folded func(key string) bool, copyGlyph string, cache *RenderCache, scope, searchQ string) (string, []ItemSpan) {
	var b strings.Builder
	spans := make([]ItemSpan, 0, len(items))
	lineIdx := 0
	for _, it := range items {
		// ONE ITEM AT A TIME, so a frame that changes one message costs one message. See renderOne.
		seg, code, askOpts := renderOne(it, maxWidth, folded, copyGlyph, cache, scope, searchQ)
		// THE ITEM'S OWN LINES.
		lines := strings.Count(seg, "\n")
		spans = append(spans, ItemSpan{
			Kind: it.Kind, Key: it.Key, Text: copyTextFor(it), Line: lineIdx, Lines: lines,
			Options: askOpts, Code: code, MessageID: it.MessageID,
		})
		lineIdx += lines
		b.WriteString(seg)
	}
	return b.String(), spans
}

// renderOne renders ONE item's segment, through the cache when there is one.
func renderOne(it ChatItem, maxWidth int, folded func(key string) bool, copyGlyph string, cache *RenderCache, scope, searchQ string) (string, []CodeSpan, []AskOptionSpan) {
	fold := false
	if folded != nil {
		fold = folded(it.Key)
	}
	if cache == nil {
		return renderItem(it, maxWidth, fold, copyGlyph, searchQ)
	}
	sig := renderSig(it)
	if seg, code, askOpts, ok := cache.get(scope, it.Key, maxWidth, copyGlyph, fold, searchQ, sig); ok {
		return seg, code, askOpts
	}
	seg, code, askOpts := renderItem(it, maxWidth, fold, copyGlyph, searchQ)
	cache.put(scope, it.Key, maxWidth, copyGlyph, fold, searchQ, sig, seg, code, askOpts)
	return seg, code, askOpts
}

// renderItem renders ONE item to its own segment, plus the line geometry that segment carries: the
// code blocks and the ask card's option rows, in lines RELATIVE to this item.
func renderItem(it ChatItem, maxWidth int, fold bool, copyGlyph string, searchQ string) (string, []CodeSpan, []AskOptionSpan) {
	var b strings.Builder
	var code []CodeSpan
	var askOpts []AskOptionSpan
	switch it.Kind {
	case KindUser:
		// THE AFFORDANCE RIDES THE BAND LABEL. The operator's own message is the one the operator can
		// click to get back, so the marker belongs beside the label that already says whose it is.
		label := userBandLabel
		if copyGlyph != "" {
			// THE GLYPH IS BOLD, the same weight change the code block's affordance got.
			label = label + " \x1b[1m" + copyGlyph + "\x1b[22m"
		}
		body, cs := renderChatMessageSpansOpts(userTextWithMarkers(it), theme.BubbleUser, maxWidth, true, label, copyGlyph, true, searchQ)
		b.WriteString(body)
		code = cs
	case KindText:
		body, cs := renderChatMessageSpansOpts(it.Text, theme.BubbleModel, maxWidth, false, "", copyGlyph, false, searchQ)
		b.WriteString(body)
		code = cs
	case KindReasoning:
		// A REASONING BLOCK, not a dim paragraph — the GUI's own shape, and COLLAPSIBLE.
		b.WriteString(renderReasoningBlock(it, maxWidth, fold))
	case KindError:
		b.WriteString(renderBubble("error", it.Text, theme.ErrorText, maxWidth))
	case KindNotice:
		b.WriteString(renderNotice(it.Text, maxWidth))
	case KindTool:
		b.WriteString(renderToolRow(it.Tool, maxWidth))
	case KindAsk:
		card, opts := renderAskCardSpans(it.Ask, maxWidth)
		b.WriteString(card)
		askOpts = opts
	case KindArtifact:
		b.WriteString(renderArtifactRow(it, maxWidth))
	case KindSession:
		meta := SessionIdentity(it)
		b.WriteString(theme.HintText.Render(truncateRow(meta, maxWidth)) + "\n")
	case KindConsent:
		// THE CARD RIDES THE TRANSCRIPT: a pending ask draws its box here, and the settled ask leaves a
		// one-line record.
		card, opts := consentLineSpans(it, maxWidth)
		b.WriteString(card)
		askOpts = opts
	}
	return b.String(), code, askOpts
}

// RenderItems renders the transcript. See RenderItemsSpans for the same render with its line geometry.
func RenderItems(items []ChatItem, maxWidth int, collapse ...func(key string) bool) string {
	body, _ := RenderItemsSpans(items, maxWidth, collapse...)
	return body
}

// chatBandGap is the number of BLANK rows emitted after each message band, so the two speakers read
// as separate blocks of text rather than one continuous fill (the operator's "there should.
const chatBandGap = 3

// userBandLabel names the operator's own messages at the band's left edge.
const userBandLabel = "You"

// renderChatMessage renders one message as a FULL-WIDTH background band: every
// line is padded to the pane's width and painted with the speaker's fill, so
// the band runs from the left edge of the conversation pane to its right edge
// (the operator's "a whole background color change behind the entire block of
// text ... from beginning to end of the width of the conversation for each
// section ... a lighter or darker color"). The operator's messages sit at the
// RIGHT of their band as a BLOCK — one left edge shared by every row, so a
// wrapped message does not begin each line at a different column — and the
// model's at the LEFT.
//
// The padding is rendered INSIDE the style, which is the whole point: filling
// only the text and leaving the margin unstyled is what made an earlier
// attempt read as "just different colored text" rather than a block.
//
// userTextWithMarkers appends the attachment markers to the operator's own text for DISPLAY.
//
// The operator said an attachment "shows up as [image] in the chat prompt", so the marker rides with the
// message it belongs to and the operator can see, on scrolling back, that a turn carried a screenshot. It is
// applied at RENDER time rather than stored into Text, so the durable copy of the message — which is what the
// server keeps and what the optimistic-echo dedupe matches on — stays exactly the text that was sent.
func userTextWithMarkers(it ChatItem) string {
	if len(it.Attachments) == 0 {
		return it.Text
	}
	markers := strings.Join(it.Attachments, " ")
	if strings.TrimSpace(it.Text) == "" {
		return markers
	}
	return it.Text + "\n" + markers
}

func renderChatMessage(text string, style lipgloss.Style, maxWidth int, right bool, label string) string {
	body, _ := renderChatMessageSpans(text, style, maxWidth, right, label, "")
	return body
}

// renderChatMessageSpans is renderChatMessage PLUS where its code blocks landed.
func renderChatMessageSpans(text string, style lipgloss.Style, maxWidth int, right bool, label, copyGlyph string) (string, []CodeSpan) {
	return renderChatMessageSpansOpts(text, style, maxWidth, right, label, copyGlyph, false, "")
}

// renderUserChatMessageSpans is renderChatMessageSpans for the OPERATOR'S OWN band: A NEWLINE THEY
// TYPED IS A LINE BREAK.
func renderUserChatMessageSpans(text string, style lipgloss.Style, maxWidth int, right bool, label, copyGlyph string) (string, []CodeSpan) {
	return renderChatMessageSpansOpts(text, style, maxWidth, right, label, copyGlyph, true, "")
}

// renderChatMessageSpansOpts is the shared body: the two entry points differ only in whether a source newline
// inside a paragraph is a line break.
func renderChatMessageSpansOpts(text string, style lipgloss.Style, maxWidth int, right bool, label, copyGlyph string, userBreaks bool, searchQ string) (string, []CodeSpan) {
	if strings.TrimSpace(text) == "" {
		return "", nil
	}
	pane := maxWidth
	if pane <= 0 {
		pane = 80
	}
	// One cell of padding inside the band on each side.
	inner := pane - 2
	if inner < 8 {
		inner = pane
	}
	// THE BODY IS MARKDOWN — the same text the GUI renders through react-markdown in AssistantBubble
	// and in the operator's own bubble.
	labelW := 0
	bodyWidth := inner
	if label != "" {
		labelW = lipgloss.Width(label)
		// The 1-cell leading pad, the label, and one space between it and the text.
		bodyWidth = inner - labelW - 1
		if bodyWidth < 8 {
			bodyWidth = 8
		}
	}
	// THE SEARCH-AWARE ENTRY POINT WHEN A QUERY IS ARMED.
	renderMD := md.RenderOnSpans
	if userBreaks {
		renderMD = md.RenderUserOnSpans
	}
	var body []string
	var mdSpans []md.CodeSpan
	if searchQ != "" {
		if userBreaks {
			body, mdSpans = md.RenderUserOnSpansSearch(text, bodyWidth, md.SurfaceOf(style), searchQ, copyGlyph)
		} else {
			body, mdSpans = md.RenderOnSpansSearch(text, bodyWidth, md.SurfaceOf(style), searchQ, copyGlyph)
		}
	} else {
		body, mdSpans = renderMD(text, bodyWidth, md.SurfaceOf(style), copyGlyph)
	}
	if len(body) == 0 {
		body = []string{text}
	}

	// blockLeft is where every row of the operator's block starts.
	blockLeft := 1
	if right {
		widest := 0
		for _, l := range body {
			if w := lipgloss.Width(l); w > widest {
				widest = w
			}
		}
		blockLeft = 1 + inner - widest
		// NEVER FURTHER LEFT THAN ONE SPACE AFTER THE LABEL, so a wide message hangs beneath the label rather
		// than running into it. bodyWidth reserves exactly that much, so the two agree by construction.
		if floor := 1 + labelW + 1; blockLeft < floor {
			blockLeft = floor
		}
	}

	var out strings.Builder
	for i, l := range body {
		var row string
		switch {
		case i == 0 && label != "":
			// The label sits at the band's left edge and the text aligns to the BLOCK, so a one-line message
			// still reads as the operator's side (flush right) while a wrapped one hangs beneath the label.
			gap := blockLeft - 1 - labelW
			if gap < 1 {
				gap = 1
			}
			row = " " + label + strings.Repeat(" ", gap) + l
		case right:
			pad := blockLeft - 1
			if pad < 1 {
				pad = 1
			}
			row = " " + strings.Repeat(" ", pad) + l
		default:
			row = " " + l
		}
		// EVERY ROW IS PADDED TO THE PANE, so the fill runs edge to edge and consecutive rows are the same
		// width.
		if pad := pane - lipgloss.Width(row); pad > 0 {
			row += strings.Repeat(" ", pad)
		}
		// AND THE ROW IS CLAMPED TO THE PANE ON EVERY PATH, so no band can hand the stream a line it would
		// have to truncate.
		row = truncateCells(row, pane)
		out.WriteString(style.Render(row))
		out.WriteString("\n")
	}
	// Separate this band from the next with BLANK rows (the pane's own
	// background), so consecutive messages read as distinct blocks.
	for i := 0; i < chatBandGap; i++ {
		out.WriteString("\n")
	}
	// The blocks, in lines relative to THIS ITEM — which is the band's own row numbering, since one markdown
	// line became exactly one row above.
	code := make([]CodeSpan, 0, len(mdSpans))
	for _, s := range mdSpans {
		code = append(code, CodeSpan{
			Line:   s.FirstRow,
			Lines:  s.LastRow - s.FirstRow + 1,
			Lang:   s.Lang,
			Source: s.Source,
		})
	}
	return out.String(), code
}

// truncateCells shortens s to at most w DISPLAY CELLS (not runes, not bytes), so a wide glyph or an ANSI
// span cannot make a row overflow the pane.
func truncateCells(s string, w int) string {
	if w <= 0 || lipgloss.Width(s) <= w {
		return s
	}
	return ansi.Truncate(s, w, "")
}

// The markdown output carries ONLY targeted attribute codes, so it nests inside the theme style
// without clearing it — which is what makes this safe over a labelled, styled row.
func renderMarkdownBubble(label, text string, style lipgloss.Style, maxWidth int) string {
	if text == "" {
		return ""
	}
	avail := maxWidth
	if avail > 0 {
		avail -= lipgloss.Width(label) + 3
	}
	lines := md.RenderOn(text, avail, md.SurfaceOf(style))
	if len(lines) == 0 {
		lines = []string{text}
	}
	var b strings.Builder
	b.WriteString(theme.ListMeta.Render(label) + " ")
	for i, l := range lines {
		if i > 0 {
			// Continuation rows are indented under the body, past the label.
			l = strings.Repeat(" ", lipgloss.Width(label)+1) + l
		}
		b.WriteString(style.Render(l))
		b.WriteString("\n")
	}
	return b.String()
}

// reasoningBodyMaxRows bounds how much of a reasoning block is drawn.
const reasoningBodyMaxRows = 12

// renderReasoningBlock renders a reasoning ("thinking") item the way the GUI does: a labelled
// header carrying its own state, then the body beneath it.
func renderReasoningBlock(it ChatItem, maxWidth int, folded bool) string {
	if strings.TrimSpace(it.Text) == "" {
		return ""
	}
	// THE ARROW IS TEXT, not colour, so it reads on a monochrome terminal — and it is on the LEFT,
	// where the operator asked for it.
	arrow := "▾"
	if folded {
		arrow = "▸"
	}
	header := theme.ReasoningLabel.Render(arrow + " reasoning")
	if it.Live {
		header += theme.ReasoningLabel.Render(" · thinking…")
	} else {
		header += theme.ListMeta.Render(" · " + groupDigits(len([]rune(it.Text))) + " chars")
	}
	if folded {
		// A collapsed block is ONE row. The header carries the state and the size, so nothing is lost by
		// hiding the body — which is the whole point of folding it.
		return header + "\n"
	}

	avail := maxWidth
	if avail > 0 {
		avail -= 4 // the body is indented under the arrow and the label
	}
	body := md.RenderOn(it.Text, avail, md.SurfaceOf(theme.ReasoningBody))
	if len(body) == 0 {
		body = []string{it.Text}
	}

	// BOUNDED — see reasoningBodyMaxRows. A transcript that draws a reasoning block in full is how a long
	// conversation's pane became an unreadable wall, and the header already carries the true size.

	var b strings.Builder
	b.WriteString(header)
	b.WriteString("\n")
	for i, l := range body {
		if i >= reasoningBodyMaxRows {
			// The elision is EXPLICIT: a silent cut would read as the end of the model's reasoning rather
			// than as a display limit, and the operator would have no way to know there was more.
			b.WriteString(theme.ReasoningBody.Render(fmt.Sprintf("  … %d more lines", len(body)-reasoningBodyMaxRows)))
			b.WriteString("\n")
			break
		}
		b.WriteString(theme.ReasoningBody.Render("  " + l))
		b.WriteString("\n")
	}
	return b.String()
}

// groupDigits inserts thousands separators: 60909 -> "60,909".
func groupDigits(n int) string {
	s := strconv.Itoa(n)
	if len(s) <= 3 {
		return s
	}
	var b strings.Builder
	lead := len(s) % 3
	if lead > 0 {
		b.WriteString(s[:lead])
	}
	for i := lead; i < len(s); i += 3 {
		if b.Len() > 0 {
			b.WriteString(",")
		}
		b.WriteString(s[i : i+3])
	}
	return b.String()
}

// renderBubble renders `label · text` with wrap, skipping empty bodies.
func renderBubble(label, text string, style lipgloss.Style, maxWidth int) string {
	if text == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString(theme.ListMeta.Render(label) + " ")
	body := text
	if maxWidth > 0 && maxWidth > len(label)+4 {
		// soft-wrap at the width boundary (word-greedy)
		body = wrapText(text, maxWidth-len(label)-3)
	}
	b.WriteString(style.Render(body))
	if !strings.HasSuffix(body, "\n") {
		b.WriteString("\n")
	}
	return b.String()
}

// FailedTurnText composes the transcript row for a FAILED turn.
func FailedTurnText(errText, modelRef string) string {
	var b strings.Builder
	b.WriteString("turn failed: " + strings.TrimSpace(errText))
	if ref := strings.TrimSpace(modelRef); ref != "" {
		b.WriteString("\nmodel: " + ref)
	}
	// RAW, not markdown: errors stay unwrapped/unstyled, per the transcript's existing rule for
	// KindError (renderBubble, which soft-wraps the raw text to the pane width so a long provider
	// error.
	return b.String()
}

// RetryAffordanceLine is the TUI's own retry affordance — the equivalent of the GUI's
// always-present Retry button, phrased as the thing the operator actually does here (Enter on the
// restored.
const RetryAffordanceLine = "your message is back in the composer — press enter to send it again"

// WithRetryAffordance appends the retry line to a failed-turn row's text. Idempotent — a re-stamp on a later
// poll (the transcript is re-read every second while a turn runs) must not double the line.
func WithRetryAffordance(text string) string {
	if text == "" || strings.Contains(text, RetryAffordanceLine) {
		return text
	}
	return text + "\n" + RetryAffordanceLine
}

// noticeLabel is the band label a platform notice carries.
const noticeLabel = "notice"

// renderNotice draws a platform notice — a `system` row, i.e. a record ABOUT the conversation
// rather than something either party said in it.
func renderNotice(text string, maxWidth int) string {
	if strings.TrimSpace(text) == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString(theme.ListMeta.Render(noticeLabel) + "\n")
	for _, para := range strings.Split(text, "\n\n") {
		para = strings.TrimSpace(para)
		if para == "" {
			continue
		}
		body := para
		if maxWidth > 0 {
			body = wrapText(para, maxWidth)
		}
		b.WriteString(theme.HintText.Render(body) + "\n")
	}
	return b.String()
}

func renderToolRow(t *ParsedTool, maxWidth int) string {
	if t == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString(theme.ToolName.Render("⚙ " + t.ToolName))
	if t.Input != "" {
		b.WriteString(theme.HintText.Render(" " + firstLine(t.Input)))
	}
	if t.Output != "" {
		b.WriteString(theme.ToolMeta.Render(" → " + firstLine(t.Output)))
	}
	b.WriteString("\n")
	return truncateLine(b.String(), maxWidth)
}

// renderAskCard paints a recorded clarifying question as the terminal card: the question and its
// numbered options.
func renderAskCard(a *ParsedAsk, maxWidth int) string {
	body, _ := renderAskCardSpans(a, maxWidth)
	return body
}

// renderAskCardSpans draws a recorded clarifying question as an INTERACTIVE CARD, and reports where
// each option row landed so a click resolves to the option under it.
func renderAskCardSpans(a *ParsedAsk, maxWidth int) (string, []AskOptionSpan) {
	if a == nil {
		return "", nil
	}
	// AN ANSWERED QUESTION IS A RECORD, NOT A CARD — the same split the permission card makes.
	if a.Refused {
		refusal := a.RefusalText
		if refusal == "" {
			refusal = "it was refused before you were shown it"
		}
		return theme.ListMeta.Render(truncateRow("not asked · "+refusal, maxWidth)) + "\n", nil
	}
	if a.Answered {
		answered := a.AnswerText
		if answered == "" {
			answered = "(no answer)"
		}
		return theme.ListMeta.Render(truncateRow("answered · "+answered, maxWidth)) + "\n", nil
	}
	// THE FOOTER PROMISES ONLY WHAT WORKS.
	footer := "↑/↓ select · enter answer · click also answers · esc hands back the keyboard"
	if a.AllowOther {
		// The free-text row is a ROW now, so the footer names it as one. It keeps the words "reply in
		// your own words" because that is still exactly what the row is for.
		footer = "↑/↓ select · enter answer · Other: reply in your own words · esc hands back the keyboard"
	}
	spec := kit2.CardSpec{
		Title:  "Orchicon asks",
		Body:   a.Question,
		Footer: footer,
	}
	for i, o := range a.Options {
		text := itoa(int64(i+1)) + ". " + o.Label
		if o.Description != "" {
			// One row per option rather than a stray indented sentence: the description belongs to the option
			// and must move, wrap and highlight WITH it, not sit beside it as unrelated prose.
			text += " — " + o.Description
		}
		// THE ▸ BAR IS THE CARD'S CURSOR, drawn from the item's Sel exactly as the consent card draws its
		// own.
		spec.Lines = append(spec.Lines, kit2.CardLine{Text: text, Selected: i == a.Sel})
	}

	// THE FREE-TEXT ROW, WHICH THIS CARD USED TO NOT HAVE AT ALL.
	labels := make([]string, 0, len(a.Options)+1)
	for _, o := range a.Options {
		labels = append(labels, o.Label)
	}
	if a.AllowOther {
		spec.Lines = append(spec.Lines, kit2.CardLine{
			Text:     ConsentOther,
			Selected: a.Drafting || a.Sel == len(a.Options),
		})
		spec.ShowInput = a.Drafting
		spec.Input = a.Draft
		labels = append(labels, ConsentOther)
	}

	lines, rows := kit2.CardLinesSpans(spec, maxWidth)
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l)
		b.WriteString("\n")
	}
	// ONE SPAN PER DRAWABLE ROW, from the labels the rows were built from — the options and, when it is
	// offered, the free-text row.
	opts := make([]AskOptionSpan, 0, len(rows))
	for i, r := range rows {
		if i >= len(labels) {
			break
		}
		opts = append(opts, AskOptionSpan{Line: r.Line, Lines: r.Lines, Label: labels[i]})
	}
	return b.String(), opts
}

func renderArtifactRow(it ChatItem, maxWidth int) string {
	line := "⬒ " + it.Name + " (" + it.Type + ")"
	out := theme.ListTitle.Render(line)
	if it.Content != "" {
		out += "\n" + theme.HintText.Render(firstLine(it.Content))
	}
	return truncateLine(out+"\n", maxWidth)
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 120 {
		s = s[:117] + "…"
	}
	return s
}

func truncateRow(s string, w int) string {
	if w <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= w {
		return s
	}
	return string(r[:w-1]) + "…"
}

func truncateLine(s string, w int) string {
	if w <= 0 {
		return s
	}
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = truncateRow(l, w)
	}
	return strings.Join(lines, "\n") + "\n"
}

// wrapText soft-wraps text at width w (simple greedy word wrap).
func wrapText(s string, w int) string {
	if w < 8 {
		w = 8
	}
	var out strings.Builder
	for _, para := range strings.Split(s, "\n") {
		words := strings.Fields(para)
		if len(words) == 0 {
			out.WriteString("\n")
			continue
		}
		line := ""
		for _, word := range words {
			switch {
			case line == "":
				line = word
			case len(line)+1+len(word) <= w:
				line += " " + word
			default:
				out.WriteString(line + "\n")
				line = word
			}
			for len(line) > w { // single long word: hard-split
				out.WriteString(line[:w] + "\n")
				line = line[w:]
			}
		}
		out.WriteString(line + "\n")
	}
	return strings.TrimRight(out.String(), "\n")
}
