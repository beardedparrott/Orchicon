package opencode

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// mockPromptAsyncServe stands up a fake opencode serve that validates the
// prompt_async part schema the way the real serve does: every part must be
// {type:"text", text} or {type:"file", mime, url} (the vendored SDK
// FilePartInput shape). Unknown types or legacy keys (mimeType/data) get a
// 400 with a validation message — the exact failure Attachments hit in the
// wild. It records the decoded parts for wire-key assertions.
func mockPromptAsyncServe(t *testing.T, status int, message string) (*httptest.Server, *[]map[string]any) {
	t.Helper()
	var got []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/session/ses_1/prompt_async" || r.Method != http.MethodPost {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Parts []map[string]any `json:"parts"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"message":"invalid JSON body"}`))
			return
		}
		got = req.Parts
		if status != 0 {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(message))
			return
		}
		for i, p := range req.Parts {
			pt, _ := p["type"].(string)
			switch pt {
			case "text":
				if _, ok := p["text"].(string); !ok {
					w.WriteHeader(http.StatusBadRequest)
					_, _ = w.Write([]byte(`{"message":"part ` + strings.Repeat("x", 0) + `text requires text field"}`))
					return
				}
			case "file":
				mime, _ := p["mime"].(string)
				url, _ := p["url"].(string)
				if mime == "" || url == "" {
					w.WriteHeader(http.StatusBadRequest)
					_, _ = w.Write([]byte(`{"message":"file part requires mime and url"}`))
					return
				}
				if _, bad := p["mimeType"]; bad {
					w.WriteHeader(http.StatusBadRequest)
					_, _ = w.Write([]byte(`{"message":"unknown key mimeType (want mime)"}`))
					return
				}
				if _, bad := p["data"]; bad {
					w.WriteHeader(http.StatusBadRequest)
					_, _ = w.Write([]byte(`{"message":"data+url together rejected (want url only)"}`))
					return
				}
				_ = i
			default:
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"message":"unknown part type ` + pt + `"}`))
				return
			}
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

// Acceptance B: text + PNG image + .md text-file parts POST to
// /session/{id}/prompt_async and the serve returns 2xx for the exact
// serialized shape; wire keys match the SDK FilePartInput schema.
func TestSendMessageWithAttachmentsRoundTrip(t *testing.T) {
	srv, got := mockPromptAsyncServe(t, 0, "")
	c := NewSessionClient(srv.URL, "", "")
	png := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}
	md := []byte("# notes\nhello\n")
	err := c.SendMessageWithAttachments(context.Background(), "ses_1", "", "", "see attached",
		[]AttachmentPart{
			{Name: "shot.png", MimeType: "image/png", Data: png},
			{Name: "notes.md", MimeType: "text/markdown", Data: md},
		})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if len(*got) != 3 {
		t.Fatalf("parts = %d, want 3 (text + png + md)", len(*got))
	}
	if (*got)[0]["type"] != "text" || (*got)[0]["text"] != "see attached" {
		t.Errorf("parts[0] = %v, want text part", (*got)[0])
	}
	for i, want := range []struct{ name, mime string }{
		{"shot.png", "image/png"},
		{"notes.md", "text/markdown"},
	} {
		p := (*got)[i+1]
		if p["type"] != "file" {
			t.Errorf("parts[%d].type = %v, want file", i+1, p["type"])
		}
		if p["mime"] != want.mime {
			t.Errorf("parts[%d].mime = %v, want %s", i+1, p["mime"], want.mime)
		}
		u, _ := p["url"].(string)
		if !strings.HasPrefix(u, "data:"+want.mime+";base64,") {
			t.Errorf("parts[%d].url = %q, want data: URL", i+1, u)
		}
		if p["filename"] != want.name {
			t.Errorf("parts[%d].filename = %v, want %s", i+1, p["filename"], want.name)
		}
		for _, bad := range []string{"mimeType", "data"} {
			if _, ok := p[bad]; ok {
				t.Errorf("parts[%d] carries legacy key %q", i+1, bad)
			}
		}
	}
}

// Acceptance B: a non-image file (.pdf) sends without 400.
func TestSendMessageWithAttachmentsPDF(t *testing.T) {
	srv, got := mockPromptAsyncServe(t, 0, "")
	c := NewSessionClient(srv.URL, "", "")
	err := c.SendMessageWithAttachments(context.Background(), "ses_1", "", "", "read this",
		[]AttachmentPart{{Name: "doc.pdf", MimeType: "application/pdf", Data: []byte("%PDF-1.4 fake")}})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if len(*got) != 2 || (*got)[1]["type"] != "file" || (*got)[1]["mime"] != "application/pdf" {
		t.Errorf("parts = %v, want text + pdf file part", *got)
	}
}

// Acceptance B: the serve error body is surfaced — a mock 400 reads as
// "http 400: <serve message>" instead of bare "http 400".
func TestSendAttachmentsSurfacesServeErrorBody(t *testing.T) {
	srv, _ := mockPromptAsyncServe(t, http.StatusBadRequest, `{"message":"file part requires mime and url"}`)
	c := NewSessionClient(srv.URL, "", "")
	err := c.SendMessageWithAttachments(context.Background(), "ses_1", "", "", "x",
		[]AttachmentPart{{Name: "a.png", MimeType: "image/png", Data: []byte{1, 2, 3}}})
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if !strings.Contains(err.Error(), "http 400") || !strings.Contains(err.Error(), "requires mime and url") {
		t.Errorf("error = %q, want http 400 + serve message", err.Error())
	}
}
