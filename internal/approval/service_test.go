package approval

import (
	"strings"
	"testing"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
)

func TestValidateAttachments(t *testing.T) {
	att := func(name, ct string, n int) *apiv1.ApprovalAttachment {
		return &apiv1.ApprovalAttachment{
			Filename:    name,
			ContentType: ct,
			Data:        []byte(strings.Repeat("x", n)),
		}
	}

	cases := []struct {
		name    string
		in      []*apiv1.ApprovalAttachment
		wantErr bool
	}{
		{name: "nil slice ok", in: nil, wantErr: false},
		{name: "valid png", in: []*apiv1.ApprovalAttachment{att("shot.png", "image/png", 1000)}, wantErr: false},
		{name: "path traversal rejected", in: []*apiv1.ApprovalAttachment{att("../../etc/passwd", "text/plain", 10)}, wantErr: true},
		{name: "slash rejected", in: []*apiv1.ApprovalAttachment{att("a/b.txt", "text/plain", 10)}, wantErr: true},
		{name: "empty data rejected", in: []*apiv1.ApprovalAttachment{att("a.txt", "text/plain", 0)}, wantErr: true},
		{name: "oversized plain rejected", in: []*apiv1.ApprovalAttachment{att("a.txt", "text/plain", maxAttachmentBytes + 1)}, wantErr: true},
		{name: "large image ok", in: []*apiv1.ApprovalAttachment{att("a.png", "image/png", maxAttachmentBytes + 1000)}, wantErr: false},
		{name: "too many attachments", in: []*apiv1.ApprovalAttachment{
			att("a.txt", "text/plain", 1), att("b.txt", "text/plain", 1),
			att("c.txt", "text/plain", 1), att("d.txt", "text/plain", 1),
			att("e.txt", "text/plain", 1), att("f.txt", "text/plain", 1),
			att("g.txt", "text/plain", 1), att("h.txt", "text/plain", 1),
			att("i.txt", "text/plain", 1), att("j.txt", "text/plain", 1),
			att("k.txt", "text/plain", 1), att("l.txt", "text/plain", 1),
			att("m.txt", "text/plain", 1), att("n.txt", "text/plain", 1),
			att("o.txt", "text/plain", 1), att("p.txt", "text/plain", 1),
			att("q.txt", "text/plain", 1), att("r.txt", "text/plain", 1),
			att("s.txt", "text/plain", 1), att("t.txt", "text/plain", 1),
			att("u.txt", "text/plain", 1),
		}, wantErr: true},
	}

	for _, c := range cases {
		_, err := validateAttachments(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("%s: validateAttachments err=%v wantErr=%v", c.name, err, c.wantErr)
		}
	}
}

func TestValidateAttachmentsDedupesNames(t *testing.T) {
	out, err := validateAttachments([]*apiv1.ApprovalAttachment{
		{Filename: "shot.png", ContentType: "image/png", Data: []byte("aaa")},
		{Filename: "shot.png", ContentType: "image/png", Data: []byte("bbb")},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("want 2 attachments, got %d", len(out))
	}
	if out[0].Filename == out[1].Filename {
		t.Errorf("colliding filenames were not de-duplicated: %q vs %q", out[0].Filename, out[1].Filename)
	}
	if out[0].Data[0] != 'a' || out[1].Data[0] != 'b' {
		t.Errorf("attachment data order not preserved")
	}
}
