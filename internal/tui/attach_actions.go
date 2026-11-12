package tui

// attach_actions.go — THE CHORDS THAT ACQUIRE AN ATTACHMENT.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
)

// attachResultMsg reports one acquisition attempt.
type attachResultMsg struct {
	att attachment
	err error
	// note carries a non-fatal remark worth showing even on success (e.g. the caps are near).
	note string
}

// A BRACKETED PASTE THAT IS ONLY A PATH is handled the same way: an operator who copies a file in
// their file manager and pastes it into the composer produces exactly this text.
func attachFileFromPrompt(prompt string) tea.Cmd {
	path := strings.TrimSpace(prompt)
	return func() tea.Msg {
		if path == "" {
			return attachResultMsg{err: fmt.Errorf("nothing to attach — type or paste a file path first")}
		}
		att, err := readFileAttachment(path, nil)
		return attachResultMsg{att: att, err: err}
	}
}

// clipboardTextMsg carries TEXT read off the system clipboard, for insertion into the composer.
type clipboardTextMsg struct {
	text string
}

// attachOrPasteFromClipboard is ctrl+v: an IMAGE on the clipboard ATTACHES, and TEXT is PASTED.
func attachOrPasteFromClipboard() tea.Cmd {
	return clipboardPasteCmd(readClipboardImage, readClipboardText)
}

// clipboardPasteCmd is the DECISION behind ctrl+v, with the two readers injected.
func clipboardPasteCmd(
	readImage func([]attachment) (attachment, error),
	readText func() (string, error),
) tea.Cmd {
	return func() tea.Msg {
		att, err := readImage(nil)
		if err == nil {
			return attachResultMsg{att: att}
		}
		if !errors.Is(err, errNoClipboardImage) {
			return attachResultMsg{err: err}
		}
		// No image: it is text, or it is nothing.
		text, terr := readText()
		if terr != nil {
			// Both reads failed: report the IMAGE error, because ctrl+v's headline job is the screenshot and
			// an empty clipboard should not be explained as "no text".
			return attachResultMsg{err: errNoClipboardImage}
		}
		if strings.TrimSpace(text) == "" {
			return attachResultMsg{err: errNoClipboardImage}
		}
		return clipboardTextMsg{text: text}
	}
}

// isProbablyPath reports whether a pasted block is a FILE PATH rather than prose, so a paste of a
// file can attach instead of being inserted as text.
func isProbablyPath(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" || strings.ContainsAny(s, "\n\r") {
		return false
	}
	// Unquote, so a shell-style quoted path is recognised.
	s = strings.Trim(s, `'"`)
	if s == "" || strings.Contains(s, " ") {
		return false
	}
	// An absolute or ~ path is evidence on its own.
	if strings.HasPrefix(s, "/") || strings.HasPrefix(s, "~") || strings.HasPrefix(s, "./") {
		return true
	}
	// A relative path is evidence only if it actually resolves to a file — which is what makes this safe
	// to run on any paste: a word that is not a file simply is not a path.
	if !strings.Contains(s, "/") {
		// A bare filename: only accept a type we would attach, so typing "readme.md" as prose does not
		// silently become an attachment.
		ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(s), "."))
		return acceptedExt[ext] && existsAsFile(s)
	}
	return existsAsFile(s)
}

// existsAsFile reports whether a path resolves to a REGULAR file the operator could attach, which is what
// makes isProbablyPath safe to run on any paste: a word that is not a file is simply not a path.
func existsAsFile(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}

// applyAttachResult folds an acquisition attempt into the pending set and tells the operator.
func (m *App) applyAttachResult(msg attachResultMsg) {
	if msg.err != nil {
		m.dock.SetError("attach: " + msg.err.Error())
		return
	}
	if err := validateAttachment(msg.att.Name, msg.att.size(), m.pendingAttach); err != nil {
		m.dock.SetError("attach: " + err.Error())
		return
	}
	m.pendingAttach = append(m.pendingAttach, msg.att)
	m.dock.SetError("")
	// The box held a PATH: clear it now that the file is attached, so the same path is not also sent as
	// prose. Only on success — see pendingAttachClear.
	if m.pendingAttachClear {
		m.pendingAttachClear = false
		m.dock.SetValue("")
	}
	// SAY WHAT WAS ATTACHED, in the operator's own vocabulary ("[image]"), plus the count so a multi-file
	// turn is legible without counting markers in the strip.
	m.dock.SetNotice(fmt.Sprintf("attached %s (%s, %s) — %d pending · send to include",
		msg.att.marker(), msg.att.MimeType, humanBytes(msg.att.size()), len(m.pendingAttach)))
	m.refreshComposerHint()
}

// attachmentPromptMarkers renders the pending set as the markers that follow the operator's text on the
// turn, so what is about to be sent is visible IN THE MESSAGE rather than only in the strip.
func (m *App) attachmentPromptMarkers() string {
	if len(m.pendingAttach) == 0 {
		return ""
	}
	parts := make([]string, 0, len(m.pendingAttach))
	for _, a := range m.pendingAttach {
		parts = append(parts, a.marker())
	}
	return strings.Join(parts, " ")
}

// wireAttachments converts the pending set for the request body. It is the ONLY place the pending
// attachments become wire messages, so the send path and the caps cannot disagree.
func (m *App) wireAttachments() []*apiv1.AttachmentInput {
	if len(m.pendingAttach) == 0 {
		return nil
	}
	out := make([]*apiv1.AttachmentInput, 0, len(m.pendingAttach))
	for _, a := range m.pendingAttach {
		out = append(out, a.toWire())
	}
	return out
}

// pendingAttachMarkers renders the CURRENT pending set as markers, for the optimistic echo.
func (m *App) pendingAttachMarkers() []string {
	if len(m.pendingAttach) == 0 {
		return nil
	}
	out := make([]string, 0, len(m.pendingAttach))
	for _, a := range m.pendingAttach {
		out = append(out, a.marker())
	}
	return out
}

// clearPendingAttachments empties the set after a successful send.
func (m *App) clearPendingAttachments() { m.pendingAttach = nil }

// humanBytes renders a byte count the way an operator reads it.
func humanBytes(n int) string {
	switch {
	case n >= 1024*1024:
		return fmt.Sprintf("%.1fMB", float64(n)/(1024*1024))
	case n >= 1024:
		return fmt.Sprintf("%.0fKB", float64(n)/1024)
	}
	return fmt.Sprintf("%dB", n)
}
