package tui

// attachments_test.go — PASTING A FILE OR A SCREENSHOT INTO A CHAT MESSAGE.
//
// The operator: "People should be able copy and paste file and screenshots/images in the TUI. OpenCode does
// this and it works. It shows up as [image] in the chat prompt but it works."
//
// THE SERVER HALF ALREADY EXISTED, which is why this is client-only: ask_orchicon.proto carries
// AttachmentInput{name, mime_type, data}, the service enforces the caps, and internal/opencode/session.go
// turns each attachment into an OpenCode FilePartInput with a data: URL — exactly how OpenCode's own client
// sends an image. What was missing is the ACQUISITION, the VALIDATION and the WIRE SHAPE, and those are what
// this file pins.
//
// The tests are mostly at the PURE level because that is where the decisions are: the caps, the type gate,
// the MIME inference, and the marker vocabulary. The one that touches the disk does so in a temp dir, so it
// cannot depend on the operator's files.

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- the caps ------------------------------------------------------------------------------------

// THE CAPS REFUSE WITH A REASON, and each cap is separate.
//
// A refusal with no explanation is indistinguishable from a broken gesture — the failure mode this codebase
// keeps producing — so every case asserts the reason names what was wrong. The caps mirror the SERVER's
// (internal/askorchicon/chat.go) and the GUI's (lib/ask-attachment.ts); being LOOSER than the server would
// mean a long base64 encode followed by a failed turn.
func TestAttachmentCapsRefuseWithAReason(t *testing.T) {
	full := make([]attachment, maxAttachments)
	for i := range full {
		full[i] = attachment{Name: "a.txt", MimeType: "text/plain", Data: []byte("x")}
	}
	if err := validateAttachment("more.txt", 1, full); err == nil {
		t.Errorf("a %dth attachment was accepted; the server caps at %d", maxAttachments+1, maxAttachments)
	} else if !strings.Contains(err.Error(), "too many") {
		t.Errorf("the count refusal does not say why: %v", err)
	}

	if err := validateAttachment("big.png", maxAttachmentByte+1, nil); err == nil {
		t.Error("an oversized attachment was accepted")
	} else if !strings.Contains(err.Error(), "too large") {
		t.Errorf("the size refusal does not say why: %v", err)
	}

	// The TOTAL cap, reached by SEVERAL individually-legal files. Three 7MB files total 21MB, which is over
	// the 20MB total while each one is under the 10MB single-file cap — so this exercises the total rule
	// rather than tripping the per-file one first (which is what a 10MB+1 fixture would do).
	twoSevenths := []attachment{
		{Name: "a.bin", Data: make([]byte, 7*1024*1024)},
		{Name: "b.bin", Data: make([]byte, 7*1024*1024)},
	}
	if err := validateAttachment("c.bin", 7*1024*1024, twoSevenths); err == nil {
		t.Error("the total-size cap was not enforced")
	} else if !strings.Contains(err.Error(), "20MB") {
		t.Errorf("the total refusal does not say why: %v", err)
	}

	// A file that fits both the per-file cap and the remaining total is accepted.
	if err := validateAttachment("ok.txt", 128, twoSevenths); err != nil {
		t.Errorf("a valid attachment was refused: %v", err)
	}
}

// --- MIME inference ------------------------------------------------------------------------------

// THE MIME IS INFERRED THE WAY THE TWO OTHER CLIENTS DO IT: from the extension when there is one, and by
// SNIFFING THE CONTENT when there is not — which is the case that matters, because a pasted screenshot
// arrives with no name at all.
func TestAttachmentMimeInference(t *testing.T) {
	png := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0, 0, 0, 0}
	jpeg := []byte{0xff, 0xd8, 0xff, 0xe0, 0, 0}
	gif := []byte("GIF89a.....")
	webp := append([]byte("RIFF\x00\x00\x00\x00"), []byte("WEBP")...)
	pdf := []byte("%PDF-1.7\n")

	for _, c := range []struct {
		what string
		name string
		data []byte
		want string
	}{
		{"png by extension", "shot.png", []byte("not really"), "image/png"},
		{"jpg by extension", "shot.JPG", []byte("x"), "image/jpeg"},
		{"text by extension", "notes.md", []byte("x"), "text/markdown"},
		{"png by CONTENT with no name", "", png, "image/png"},
		{"png by content, unknown name", "clip", png, "image/png"},
		{"jpeg by content", "", jpeg, "image/jpeg"},
		{"gif by content", "", gif, "image/gif"},
		{"webp by content", "", webp, "image/webp"},
		{"pdf by content", "", pdf, "application/pdf"},
	} {
		if got := mimeFor(c.name, c.data); got != c.want {
			t.Errorf("%s: mimeFor(%q) = %q, want %q", c.what, c.name, got, c.want)
		}
	}
}

// THE TYPE GATE. MIME wins when it is specific; the EXTENSION is the fallback, because a path typed by hand
// often carries no MIME at all. This mirrors the GUI's ACCEPTED_EXTS.
func TestAttachmentTypeGate(t *testing.T) {
	for _, c := range []struct {
		what string
		name string
		mime string
		want bool
	}{
		{"an image", "shot.png", "image/png", true},
		{"text", "notes.txt", "text/plain", true},
		{"json by mime", "data.bin", "application/json", true},
		{"pdf by mime", "doc.bin", "application/pdf", true},
		{"png with NO mime, by extension", "shot.png", "", true},
		{"go with no mime, by extension", "main.go", "", true},
		{"an executable with no mime", "run.exe", "", false},
		{"a binary blob with no mime", "firmware.bin", "application/octet-stream", false},
	} {
		if got := acceptedName(c.name, c.mime); got != c.want {
			t.Errorf("%s: acceptedName(%q, %q) = %v, want %v", c.what, c.name, c.mime, got, c.want)
		}
	}
}

// --- the transcript marker -----------------------------------------------------------------------

// THE MARKER IS THE OPERATOR'S OWN VOCABULARY: "[image]" for a picture, because that is what they said they
// see in OpenCode's prompt. It is not decoration — the composer line is cleared on send, so without it the
// operator has no record on screen that the turn carried a screenshot.
func TestAttachmentMarkerVocabulary(t *testing.T) {
	img := attachment{Name: "shot.png", MimeType: "image/png"}
	if got := img.marker(); got != "[image]" {
		t.Errorf("an image's marker = %q, want the operator's %q", got, "[image]")
	}
	doc := attachment{Name: "notes.md", MimeType: "text/markdown"}
	if got := doc.marker(); got != "[file: notes.md]" {
		t.Errorf("a file's marker = %q, want it to name the file", got)
	}
}

// THE WIRE SHAPE IS THE PROTO'S, and the raw bytes go through — base64 happens at the transport, not here.
func TestAttachmentWireShape(t *testing.T) {
	raw := []byte{1, 2, 3}
	a := attachment{Name: "shot.png", MimeType: "image/png", Data: raw}
	w := a.toWire()
	if w.GetName() != "shot.png" || w.GetMimeType() != "image/png" {
		t.Errorf("wire = %+v, want the name and mime carried", w)
	}
	if !bytes.Equal(w.GetData(), raw) {
		t.Errorf("wire data = %v, want the raw bytes %v", w.GetData(), raw)
	}
	if got := totalBytes([]attachment{a, a}); got != 6 {
		t.Errorf("totalBytes = %d, want 6", got)
	}
}

// --- the paste-as-path heuristic -----------------------------------------------------------------

// isProbablyPath IS DELIBERATELY CONSERVATIVE, and this pins the asymmetry it is built on: attaching
// something the operator meant to TYPE is a silent change to their message, while inserting a path they
// meant to attach is merely unhelpful. So it demands positive evidence.
func TestPastedPathHeuristic(t *testing.T) {
	// A real file in a temp dir, for the relative-path case.
	dir := t.TempDir()
	real := filepath.Join(dir, "notes.md")
	if err := os.WriteFile(real, []byte("hi"), 0o600); err != nil {
		t.Fatal(err)
	}
	rel := func() string {
		wd, _ := os.Getwd()
		if err := os.Chdir(dir); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chdir(wd) })
		return "notes.md"
	}

	for _, c := range []struct {
		what string
		in   string
		want bool
	}{
		{"an absolute path", "/tmp/whatever.png", true},
		{"a home path", "~/shot.png", true},
		{"an explicit relative path", "./shot.png", true},
		{"a QUOTED path", `"/tmp/shot.png"`, true},
		// A QUOTED PATH CONTAINING A SPACE IS REFUSED, and that is the documented conservative rule rather
		// than an oversight: the heuristic demands no interior spaces so that prose cannot be mistaken for a
		// path. The cost is that such a path must be attached by chord instead of pasted — the safe side of
		// the asymmetry this function is built on.
		{"a quoted path with a space (refused by design)", `"/tmp/a shot.png"`, false},
		{"prose", "let me explain what I meant by that", false},
		{"an empty paste", "   ", false},
		{"a multi-line paste", "/tmp/a.png\n/tmp/b.png", false},
		{"a sentence ending in a slash", "see the docs at /", false},
		{"a bare word that is not a file", "readme", false},
	} {
		if got := isProbablyPath(c.in); got != c.want {
			t.Errorf("%s: isProbablyPath(%q) = %v, want %v", c.what, c.in, got, c.want)
		}
	}

	// A bare FILENAME is accepted only when it is both an attachable type and actually present — so typing
	// "readme.md" as prose does not silently become an attachment.
	name := rel()
	if !isProbablyPath(name) {
		t.Errorf("isProbablyPath(%q) = false for a file that exists in the working dir", name)
	}
	if isProbablyPath("nope.md") {
		t.Error("a bare filename that does not exist was treated as a path")
	}
}

// --- reading a real file -------------------------------------------------------------------------

// A REAL FILE IS READ AND VALIDATED, and every failure names its reason.
func TestReadFileAttachment(t *testing.T) {
	dir := t.TempDir()
	ok := filepath.Join(dir, "notes.md")
	if err := os.WriteFile(ok, []byte("# hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := readFileAttachment(ok, nil)
	if err != nil {
		t.Fatalf("readFileAttachment: %v", err)
	}
	if a.Name != "notes.md" || !bytes.Equal(a.Data, []byte("# hello")) {
		t.Errorf("attachment = %+v, want the file's name and bytes", a)
	}
	if a.MimeType == "" {
		t.Error("the attachment has no MIME type, so the server cannot decide vision-vs-inline")
	}

	// A QUOTED path, as a shell or a drag-drop hands it over.
	if _, err := readFileAttachment(`"`+ok+`"`, nil); err != nil {
		t.Errorf("a quoted path was not accepted: %v", err)
	}

	// A DIRECTORY is refused with a reason, not read.
	if _, err := readFileAttachment(dir, nil); err == nil {
		t.Error("a directory was accepted as an attachment")
	} else if !strings.Contains(err.Error(), "directory") {
		t.Errorf("the directory refusal does not say why: %v", err)
	}

	// A MISSING file is refused with a reason.
	if _, err := readFileAttachment(filepath.Join(dir, "nope.png"), nil); err == nil {
		t.Error("a missing file was accepted")
	}

	// A NAME WITH NO PATH is refused rather than resolving to the process's own file.
	if _, err := readFileAttachment("", nil); err == nil {
		t.Error("an empty path was accepted")
	}

	// And an UNACCEPTED TYPE is refused, naming the type it saw.
	exe := filepath.Join(dir, "run.exe")
	if err := os.WriteFile(exe, []byte("MZ"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readFileAttachment(exe, nil); err == nil {
		t.Error("an executable was accepted as an attachment")
	} else if !strings.Contains(err.Error(), "accepted type") {
		t.Errorf("the type refusal does not say why: %v", err)
	}
}

// --- the pending set and the send ----------------------------------------------------------------

// ATTACHMENTS BELONG TO ONE TURN. They are frozen into the request at send, the markers are what the
// transcript shows, and the pending set is CLEARED so the next message does not carry them again.
func TestPendingAttachmentsAreClearedOnSend(t *testing.T) {
	m := askRelaunched(t, "c1")

	m.pendingAttach = []attachment{
		{Name: "shot.png", MimeType: "image/png", Data: []byte("png")},
		{Name: "notes.md", MimeType: "text/markdown", Data: []byte("md")},
	}

	markers := m.pendingAttachMarkers()
	if len(markers) != 2 || markers[0] != "[image]" || markers[1] != "[file: notes.md]" {
		t.Errorf("markers = %v, want the operator's [image] / [file: …] vocabulary", markers)
	}

	wire := m.wireAttachments()
	if len(wire) != 2 {
		t.Fatalf("wire attachments = %d, want 2", len(wire))
	}
	if wire[0].GetName() != "shot.png" || wire[0].GetMimeType() != "image/png" {
		t.Errorf("wire[0] = %+v", wire[0])
	}

	m.clearPendingAttachments()
	if n := len(m.wireAttachments()); n != 0 {
		t.Errorf("the pending set survived a send (%d left) — the next message would carry them again", n)
	}
}

// A FAILED SEND PUTS THEM BACK. The bytes are the operator's work: a transport failure must not silently
// discard a screenshot they chose, because re-acquiring it is the expensive part.
func TestFailedSendRestoresAttachments(t *testing.T) {
	m := askRelaunched(t, "c1")
	m.pendingAttach = []attachment{{Name: "shot.png", MimeType: "image/png", Data: []byte("png")}}

	// What the send path does: freeze into the request, then clear.
	frozen := m.wireAttachments()
	m.lastSentAttachments = frozen
	m.clearPendingAttachments()
	if len(m.pendingAttach) != 0 {
		t.Fatal("fixture: the pending set was not cleared")
	}

	// The turn fails.
	m.restoreAttachments()

	back := m.wireAttachments()
	if len(back) != 1 || back[0].GetName() != "shot.png" {
		t.Fatalf("attachments were not restored after a failed send: %+v", back)
	}
	if !bytes.Equal(back[0].GetData(), []byte("png")) {
		t.Errorf("the restored attachment lost its bytes: %v", back[0].GetData())
	}
}

// THE TRANSCRIPT RECORDS WHAT THE TURN CARRIED. The composer line is cleared on send, so the marker is the
// only on-screen record that a screenshot went with the message.
func TestSentMessageCarriesItsAttachmentMarkers(t *testing.T) {
	m := askRelaunched(t, "c1")
	m.pendingAttach = []attachment{{Name: "shot.png", MimeType: "image/png", Data: []byte("png")}}

	// The shape the send path builds: the composer text with its markers appended.
	text := "what is wrong with this screen?" + m.attachmentPromptMarkers()
	if !strings.Contains(text, "[image]") {
		t.Errorf("the sent message does not record the image: %q", text)
	}
	if !strings.Contains(text, "what is wrong") {
		t.Errorf("the operator's own words were lost: %q", text)
	}
}

// --- the chords ----------------------------------------------------------------------------------

// THE CHORDS ARE BOUND AND REACHABLE. A chord nobody can see is a chord nobody has, so the two routes must
// exist in the registry the help overlay renders FROM, and they must be in the composer's bypass list —
// otherwise the textarea eats the keystroke before any route sees it.
func TestAttachmentChordsAreRegisteredAndBypassTheComposer(t *testing.T) {
	m := askRelaunched(t, "c1")

	var haveFile, haveImage bool
	for _, r := range m.routes {
		switch r.Keys {
		case "ctrl+f":
			haveFile = true
		case "ctrl+v":
			haveImage = true
		}
	}
	if !haveFile {
		t.Error("ctrl+f (attach a file by path) is not registered, so the help overlay never lists it")
	}
	if !haveImage {
		t.Error("ctrl+v (paste an image) is not registered, so the help overlay never lists it")
	}

	// The bypass list is what gets the chord past the textarea.
	for _, k := range []string{"ctrl+v", "ctrl+f"} {
		if !composerBypassKeys[k] {
			t.Errorf("%s is not in composerBypassKeys — the composer would consume it before the route "+
				"ran, which is exactly how a bound chord ends up unreachable", k)
		}
	}
}

// NOTE ON THE NIL HOOK. The dock offers every bracketed paste to a hook that the SHELL installs, and that
// hook is a nil func field — so a dock built without a shell (several tests, and any embedder) must treat a
// paste as text rather than panicking on the nil call. That case is pinned at the widget, in
// internal/tui/dock's TestPasteRendersWithoutSending; it is deliberately NOT re-asserted here, because the
// shell has no accessor for "was the hook installed" and inventing one to satisfy a test would be adding
// production code for the test's benefit.
