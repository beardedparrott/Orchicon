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
//	/attach <path>            → a FILE PATH (the operator types or pastes the path; a file has a path and
//	                            may never have been on the clipboard)
//
// ctrl+f used to be the FILE-PATH chord; it is now FIND IN TRANSCRIPT on both surfaces, and a file path
// is attached from the slash idiom the TUI already uses (/attach <path>), which delegates to the same
// reader (attachFileFromPrompt) this file provides.
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

	"github.com/beardedparrott/orchicon/internal/procwin"
)

// The caps, mirrored from the server (internal/askorchicon/chat.go: "too many attachments (max 5)",
// "attachments too large (max 20MB total)") and from the GUI's lib/ask-attachment.ts.
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

// validateAttachment answers whether one more file may be added, given what is already attached.
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

// mimeFor infers a MIME type the way the GUI's inferMime does, and falls back to sniffing the
// CONTENT when there is no extension to go on — which is the common case for a pasted screenshot.
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
	// SNIFF, because a pasted screenshot has no name.
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

// acceptedName reports whether a file passes the ALLOWED-TYPE gate.
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

// The TUI writes the clipboard with the terminal's OSC 52 escape (see clipState.copyCmd) — and that
// is WRITE-ONLY.
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
var errNoClipboardImage = errors.New("the clipboard holds no image")

// readClipboardImage pulls an image from the system clipboard, or returns an error whose text is
// worth showing the operator.
func readClipboardImage(current []attachment) (attachment, error) {
	argv := clipboardImageCommand()
	if argv == nil {
		return attachment{}, errors.New("no clipboard image reader on this machine — install " +
			clipboardHelperHint() + ", or attach the file by path (/attach <path>)")
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
		out, err := procwin.Command(argv[0], argv[1:]...).Output()
		_ = out
		if err != nil {
			return attachment{}, errNoClipboardImage
		}
		return readFileAttachment(path, current)
	}

	cmd := procwin.Command(argv[0], argv[1:]...)
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

// clipboardTextCommand returns the argv that reads TEXT off the system clipboard, or nil when this
// machine has no reader.
func clipboardTextCommand() []string {
	return clipboardTextCommandFor(runtime.GOOS, os.Getenv("WAYLAND_DISPLAY"), hasBinary)
}

// clipboardTextCommandFor is clipboardTextCommand's decision, with the platform, the Wayland hint
// and the PATH probe injected.
func clipboardTextCommandFor(goos, wayland string, has func(string) bool) []string {
	switch goos {
	case "darwin":
		if has("pbpaste") {
			return []string{"pbpaste"}
		}
	case "linux":
		if wayland != "" && has("wl-paste") {
			// -n trims the trailing newline these tools append, so a pasted sentence does not arrive with a
			// stray blank line in the composer.
			return []string{"wl-paste", "-n"}
		}
		if has("xclip") {
			return []string{"xclip", "-selection", "clipboard", "-o"}
		}
		if has("xsel") {
			return []string{"xsel", "--clipboard", "--output"}
		}
	case "windows":
		// NO SEPARATE HELPER TO PROBE FOR: the reader is PowerShell, which is present on every supported
		// Windows. A Windows 7-era box would have to spell it powershell.exe; the project's floor is well
		// above that, and "pwsh" is deliberately NOT the choice — PowerShell 7 is an optional install and
		// its absence would be a silent P0 on the one platform this branch exists for.
		//
		// -NoProfile IS FOR CORRECTNESS, NOT SPEED: anything a user profile prints lands on STDOUT and
		// therefore becomes part of the "pasted" text — a prompt banner would be pasted into their message.
		// -STA is REQUIRED: Get-Clipboard needs a single-threaded apartment and fails outright under MTA.
		// -Raw asks for the clipboard's text as ONE string; without it the cmdlet splits on the clipboard's
		// line breaks and the result is joined with newlines, which is the shape that then types as Enter.
		return []string{
			"powershell", "-NoProfile", "-STA", "-NoLogo", "-Command", "Get-Clipboard -Raw",
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
	out, err := procwin.Command(argv[0], argv[1:]...).Output()
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
	case "windows":
		// The text reader is PowerShell and needs no install, so the only thing that can be missing here is
		// the IMAGE path — which has no Windows reader yet.
		return "nothing for text (PowerShell is used) — images have no Windows clipboard reader yet, so use the file picker"
	}
	return "a platform clipboard tool"
}

// encodeForDebug is used by tests to assert the wire shape without a base64 import at each call site.
func encodeForDebug(b []byte) string { return base64.StdEncoding.EncodeToString(b) }
