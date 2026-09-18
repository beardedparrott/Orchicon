package tui

// attachments.go — ATTACHMENTS: pasting a FILE or a SCREENSHOT into a chat message.
//
// The operator: "People should be able copy and paste file and screenshots/images in the TUI. OpenCode
// does this and it works. It shows up as [image] in the chat prompt but it works."
//
// THE SERVER ALREADY DOES ALL OF IT, which is why this is a CLIENT-ONLY change. The wire contract is
// ask_orchicon.proto's AttachmentInput {name, mime_type, data}; the service validates the caps the GUI's
// lib/ask-attachment.ts also enforces (5 files, 10MB each, 20MB total), describes each attachment in the
// system prompt (images as "forwarded as vision input", text files inlined in a fenced block), and
// internal/opencode/session.go turns each one into an OpenCode FilePartInput —
// {type:"file", mime, url: "data:<mime>;base64,…", filename} — which is exactly how OpenCode's own TUI
// receives an image. So an attachment sent from this client arrives as a vision part with no server work
// at all.
//
// WHAT WAS MISSING IS THE CLIENT HALF: the TUI's ChatStreamRequest carried `Message` and nothing else, and
// the composer had no way to acquire a file. This file is the acquisition, the validation, and the wire
// shape.
//
// TWO WAYS IN, and they are different mechanisms because the sources are different:
//
//	ctrl+v / the paste chord  → the SYSTEM CLIPBOARD (a screenshot lives there as image bytes, and it has
//	                            no path to read)
//	ctrl+f                    → a FILE PATH (the operator types or pastes the path; a file has a path and
//	                            may never have been on the clipboard)
//
// See readClipboardImage for why the first one cannot be OSC 52.

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
)

// The caps, mirrored from the server (internal/askorchicon/chat.go: "too many attachments (max 5)",
// "attachments too large (max 20MB total)") and from the GUI's lib/ask-attachment.ts. The SERVER IS
// AUTHORITATIVE — these exist to refuse locally with a reason instead of bouncing the send, and they must
// not be LOOSER than the server's or the operator gets a failed turn after a long encode.
const (
	maxAttachments     = 5
	maxAttachmentByte  = 10 * 1024 * 1024
	maxTotalAttachByte = 20 * 1024 * 1024
)

// acceptedExt mirrors the GUI's ACCEPTED_EXTS. A file whose extension is here is accepted even when the
// platform reports no MIME type, which is common for arbitrary paths.
var acceptedExt = map[string]bool{
	"png": true, "jpg": true, "jpeg": true, "gif": true, "webp": true, "svg": true, "bmp": true,
	"pdf": true, "txt": true, "md": true, "mdx": true, "json": true, "csv": true,
	"yaml": true, "yml": true, "html": true, "css": true, "js": true, "ts": true, "tsx": true,
	"go": true, "py": true, "rs": true, "java": true, "sh": true, "xml": true, "log": true,
}

// attachment is one acquired file, ready to put on the wire. Data is the RAW bytes; base64 is applied only
// at the wire boundary, so nothing here holds an encoded copy.
type attachment struct {
	Name     string
	MimeType string
	Data     []byte
}

// size is the attachment's byte length (the number the caps are measured in).
func (a attachment) size() int { return len(a.Data) }

// isImage reports whether this is a vision part. It is the same test the server uses to decide between
// "forwarded as vision input" and an inlined fenced block.
func (a attachment) isImage() bool { return strings.HasPrefix(a.MimeType, "image/") }

// marker renders the attachment as it appears IN THE TRANSCRIPT — "[image]" for a picture and
// "[file: name]" for anything else.
//
// The operator said it "shows up as [image] in the chat prompt", so that is the vocabulary. Marking it in
// the transcript is not decoration: the composer line is cleared on send, and without a marker the
// operator would have no record on screen that the turn carried a screenshot.
func (a attachment) marker() string {
	if a.isImage() {
		return "[image]"
	}
	return "[file: " + a.Name + "]"
}

// toWire converts to the request message. base64 happens here and nowhere else.
func (a attachment) toWire() *apiv1.AttachmentInput {
	return &apiv1.AttachmentInput{
		Name:     a.Name,
		MimeType: a.MimeType,
		Data:     a.Data,
	}
}

// validateAttachment answers whether one more file may be added, given what is already attached. It mirrors
// the server's caps and returns the REASON so the operator is told (a refusal with no explanation is
// indistinguishable from a broken gesture).
func validateAttachment(name string, size int, current []attachment) error {
	if len(current) >= maxAttachments {
		return fmt.Errorf("too many attachments (max %d) — send this turn first", maxAttachments)
	}
	if size > maxAttachmentByte {
		return fmt.Errorf("%s is too large (max 10MB)", name)
	}
	if total := totalBytes(current) + size; total > maxTotalAttachByte {
		return fmt.Errorf("attachments too large (max 20MB total)")
	}
	return nil
}

// totalBytes is the running total the 20MB cap is measured against.
func totalBytes(as []attachment) int {
	n := 0
	for _, a := range as {
		n += a.size()
	}
	return n
}

// mimeFor infers a MIME type the way the GUI's inferMime does, and falls back to sniffing the CONTENT when
// there is no extension to go on — which is the common case for a pasted screenshot, whose clipboard bytes
// arrive with no name at all.
func mimeFor(name string, data []byte) string {
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(name), "."))
	switch ext {
	case "png":
		return "image/png"
	case "jpg", "jpeg":
		return "image/jpeg"
	case "gif":
		return "image/gif"
	case "webp":
		return "image/webp"
	case "svg":
		return "image/svg+xml"
	case "bmp":
		return "image/bmp"
	case "pdf":
		return "application/pdf"
	case "json":
		return "application/json"
	case "csv":
		return "text/csv"
	case "md", "mdx":
		return "text/markdown"
	case "txt", "log":
		return "text/plain"
	case "yaml", "yml":
		return "text/yaml"
	}
	// SNIFF, because a pasted screenshot has no name. The magic numbers are the ones that matter for a
	// terminal paste and they are checked before defaulting, so an image is never sent as
	// application/octet-stream — which the server would not forward as a vision part.
	if m := sniffMime(data); m != "" {
		return m
	}
	return "application/octet-stream"
}

// sniffMime reads the leading bytes for a known image format. Order matters only in that PNG is checked
// first (its signature is the longest and least likely to collide).
func sniffMime(data []byte) string {
	switch {
	case len(data) >= 8 && bytes.Equal(data[:8], []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}):
		return "image/png"
	case len(data) >= 3 && data[0] == 0xff && data[1] == 0xd8 && data[2] == 0xff:
		return "image/jpeg"
	case len(data) >= 6 && (bytes.Equal(data[:6], []byte("GIF87a")) || bytes.Equal(data[:6], []byte("GIF89a"))):
		return "image/gif"
	case len(data) >= 12 && bytes.Equal(data[:4], []byte("RIFF")) && bytes.Equal(data[8:12], []byte("WEBP")):
		return "image/webp"
	case len(data) >= 5 && bytes.Equal(data[:5], []byte("%PDF-")):
		return "application/pdf"
	}
	return ""
}

// acceptedName reports whether a file passes the ALLOWED-TYPE gate. MIME wins when it is known and
// specific; the extension is the fallback, exactly as the GUI does it — because a path typed by hand often
// carries no MIME at all.
func acceptedName(name, mime string) bool {
	m := strings.ToLower(mime)
	switch {
	case strings.HasPrefix(m, "image/"), strings.HasPrefix(m, "text/"):
		return true
	case strings.Contains(m, "json"), strings.Contains(m, "csv"),
		strings.Contains(m, "pdf"), strings.Contains(m, "xml"), strings.Contains(m, "yaml"):
		return true
	}
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(name), "."))
	return ext != "" && acceptedExt[ext]
}

// readFileAttachment loads a path from disk and validates it. It returns a REASON on every failure so the
// composer can say what went wrong rather than appearing to do nothing.
func readFileAttachment(path string, current []attachment) (attachment, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return attachment{}, errors.New("no file given")
	}
	// Unquote a shell-style path: an operator who dragged a file into the terminal often gets quotes, and a
	// path with spaces pasted from a shell may be inside them.
	path = strings.Trim(strings.TrimSpace(path), `'"`)
	info, err := os.Stat(path)
	if err != nil {
		return attachment{}, fmt.Errorf("cannot read %s: %w", filepath.Base(path), err)
	}
	if info.IsDir() {
		return attachment{}, fmt.Errorf("%s is a directory — attach a file", filepath.Base(path))
	}
	if err := validateAttachment(filepath.Base(path), int(info.Size()), current); err != nil {
		return attachment{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return attachment{}, fmt.Errorf("cannot read %s: %w", filepath.Base(path), err)
	}
	name := filepath.Base(path)
	mime := mimeFor(name, data)
	if !acceptedName(name, mime) {
		return attachment{}, fmt.Errorf("%s is not an accepted type (%s)", name, mime)
	}
	return attachment{Name: name, MimeType: mime, Data: data}, nil
}

// clipboardImageCommand returns the argv that reads an image off the system clipboard, or nil when this
// machine has no reader.
//
// WHY NOT OSC 52. The TUI writes the clipboard with the terminal's OSC 52 escape (see clipState.copyCmd) —
// and that is WRITE-ONLY. The protocol has a read request, but replies arrive on the input stream
// interleaved with the operator's keystrokes, many terminals disable it, and tmux drops it; it is not a
// foundation a feature can stand on. A screenshot is bytes in the SYSTEM clipboard, so getting it out is a
// platform question, answered the same way every terminal editor answers it: a helper binary.
//
// The helpers are the conventionally-installed ones, and the order prefers the tool for the session type
// that is actually present (a Wayland session has wl-paste, an X session has xclip/xsel, macOS has
// pngpaste). A machine with none returns nil and the TUI says so rather than failing silently.
func clipboardImageCommand() []string {
	switch runtime.GOOS {
	case "darwin":
		// pngpaste writes the clipboard's image to a path; the empty arg is filled in by the caller.
		if hasBinary("pngpaste") {
			return []string{"pngpaste", ""}
		}
	case "linux":
		if os.Getenv("WAYLAND_DISPLAY") != "" && hasBinary("wl-paste") {
			// --type image/png so a text clipboard produces nothing rather than text bytes; -n avoids a
			// trailing newline being taken as part of the image.
			return []string{"wl-paste", "-n", "--type", "image/png"}
		}
		if hasBinary("xclip") {
			return []string{"xclip", "-selection", "clipboard", "-t", "image/png", "-o"}
		}
		if hasBinary("xsel") {
			return []string{"xsel", "--clipboard", "--output"}
		}
	}
	return nil
}

// hasBinary reports whether a command is on PATH.
func hasBinary(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

// errNoClipboardImage is the "the clipboard holds no IMAGE" case, as a sentinel.
//
// It is a sentinel rather than a plain message because the caller has to TELL IT APART from a real failure:
// ctrl+v tries the image first and falls back to TEXT, and that fallback is only correct when there was
// genuinely no image. A size or format refusal must NOT be reported as "the clipboard holds no text".
var errNoClipboardImage = errors.New("the clipboard holds no image")

// readClipboardImage pulls an image from the system clipboard, or returns an error whose text is worth
// showing the operator.
//
// Every failure here is EXPLAINED rather than silent, because the gesture is a keystroke: a chord that
// appears to do nothing is indistinguishable from one that is not bound, which is the failure mode this
// whole codebase keeps running into.
func readClipboardImage(current []attachment) (attachment, error) {
	argv := clipboardImageCommand()
	if argv == nil {
		return attachment{}, errors.New("no clipboard image reader on this machine — install " +
			clipboardHelperHint() + ", or attach the file by path (ctrl+f)")
	}
	// macOS: replace the placeholder output path with a temp file, since pngpaste writes to a path rather
	// than streaming to stdout.
	if runtime.GOOS == "darwin" && argv[0] == "pngpaste" {
		tmp, err := os.CreateTemp("", "orch-paste-*.png")
		if err != nil {
			return attachment{}, fmt.Errorf("cannot stage the pasted image: %w", err)
		}
		path := tmp.Name()
		tmp.Close()
		defer os.Remove(path)
		argv = []string{"pngpaste", path}
		out, err := exec.Command(argv[0], argv[1:]...).Output()
		_ = out
		if err != nil {
			return attachment{}, errNoClipboardImage
		}
		return readFileAttachment(path, current)
	}

	cmd := exec.Command(argv[0], argv[1:]...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	data, err := cmd.Output()
	if err != nil {
		// An empty output with a failure is the ordinary "no image on the clipboard" case — which the caller
		// uses as the signal to try TEXT instead (see attachOrPasteFromClipboard).
		if len(data) == 0 {
			return attachment{}, errNoClipboardImage
		}
		return attachment{}, fmt.Errorf("reading the clipboard failed: %v", err)
	}
	if len(data) == 0 {
		return attachment{}, errNoClipboardImage
	}
	if err := validateAttachment("pasted image", len(data), current); err != nil {
		return attachment{}, err
	}
	mime := sniffMime(data)
	if mime == "" {
		return attachment{}, errors.New("the clipboard image is not a supported format")
	}
	return attachment{Name: pastedImageName(mime), MimeType: mime, Data: data}, nil
}

// clipboardTextCommand returns the argv that reads TEXT off the system clipboard, or nil when this machine
// has no reader.
//
// IT IS THE SAME HELPERS AS THE IMAGE READER with the type argument dropped, because text is the default
// representation for every one of them — and it is a SEPARATE command rather than a flag because the failure
// semantics differ: an image read must produce NOTHING when the clipboard holds text (so the caller can fall
// back), whereas a text read must produce the text.
func clipboardTextCommand() []string {
	switch runtime.GOOS {
	case "darwin":
		if hasBinary("pbpaste") {
			return []string{"pbpaste"}
		}
	case "linux":
		if os.Getenv("WAYLAND_DISPLAY") != "" && hasBinary("wl-paste") {
			// -n trims the trailing newline these tools append, so a pasted sentence does not arrive with a
			// stray blank line in the composer.
			return []string{"wl-paste", "-n"}
		}
		if hasBinary("xclip") {
			return []string{"xclip", "-selection", "clipboard", "-o"}
		}
		if hasBinary("xsel") {
			return []string{"xsel", "--clipboard", "--output"}
		}
	}
	return nil
}

// readClipboardText reads the clipboard's text. No text and no reader are both errors, with their own words:
// the operator is told which one it was.
func readClipboardText() (string, error) {
	argv := clipboardTextCommand()
	if argv == nil {
		return "", errors.New("no clipboard reader on this machine — install " + clipboardHelperHint())
	}
	out, err := exec.Command(argv[0], argv[1:]...).Output()
	if err != nil {
		return "", errors.New("the clipboard holds no text")
	}
	return string(out), nil
}

// pastedImageName names a clipboard image after its format, so the transcript's [file: …] marker (and the
// model's own view of the attachment) says something more useful than "clipboard".
func pastedImageName(mime string) string {
	switch mime {
	case "image/png":
		return "pasted-image.png"
	case "image/jpeg":
		return "pasted-image.jpg"
	case "image/gif":
		return "pasted-image.gif"
	case "image/webp":
		return "pasted-image.webp"
	}
	return "pasted-image"
}

// clipboardHelperHint names what to install for this platform, so the error tells the operator what to DO.
func clipboardHelperHint() string {
	switch runtime.GOOS {
	case "darwin":
		return "pngpaste (brew install pngpaste)"
	case "linux":
		if os.Getenv("WAYLAND_DISPLAY") != "" {
			return "wl-clipboard (wl-paste)"
		}
		return "xclip or xsel"
	}
	return "a platform clipboard tool"
}

// encodeForDebug is used by tests to assert the wire shape without a base64 import at each call site.
func encodeForDebug(b []byte) string { return base64.StdEncoding.EncodeToString(b) }
