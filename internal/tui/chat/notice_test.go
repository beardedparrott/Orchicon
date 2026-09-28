package chat

// notice_test.go — a platform notice must not be drawn as the model's own words.
//
// A `system` row is something the PLATFORM said about the conversation — today, the
// record that its context was collapsed because it reached the model's window. Both
// clients used to map every unrecognised role to their assistant rendering, so a
// compaction record ("compacted 2343 messages into 1 summary + 7 recent messages")
// appeared in Orchicon's own band, reading as a claim the model had made. On the one
// event where attribution is the whole point, the attribution was wrong.

import (
	"strings"
	"testing"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
)

// noticeContent mirrors what the service writes: short paragraphs, the last of which
// carries the archive path.
const noticeContent = "Context compacted to keep this conversation inside the model's window. " +
	"compacted 2343 messages into 1 summary + 7 recent messages.\n\n" +
	"This ran automatically: the conversation reached the model's context window.\n\n" +
	"The pre-collapse transcript was preserved at /var/lib/orchicon/ask-history/x.bak and can be read directly."

func TestASystemRowBecomesANoticeNotTheModelsText(t *testing.T) {
	items := conversationItems([]*apiv1.ChatMessage{
		{Id: "m1", Role: "user", Content: "please fix the crash"},
		{Id: "m2", Role: "system", Content: noticeContent},
		{Id: "m3", Role: "assistant", Content: "looking at it now"},
	})

	var notice *ChatItem
	for i := range items {
		if items[i].Key == "m-m2" {
			notice = &items[i]
		}
	}
	if notice == nil {
		t.Fatal("the system row produced no transcript item")
	}
	if notice.Kind != KindNotice {
		t.Fatalf("system row became Kind=%q — it must be %q. KindText is the assistant's band, so "+
			"the notice would read as though the model had said it", notice.Kind, KindNotice)
	}
	// The real conversation is untouched: the notice did not swallow its neighbours.
	var sawUser, sawAssistant bool
	for _, it := range items {
		if it.Kind == KindUser && it.Text == "please fix the crash" {
			sawUser = true
		}
		if it.Kind == KindText && it.Text == "looking at it now" {
			sawAssistant = true
		}
	}
	if !sawUser || !sawAssistant {
		t.Fatalf("the notice displaced real transcript items (user=%v assistant=%v)", sawUser, sawAssistant)
	}
}

func TestANoticeRendersAsANoticeAndNotAsAModelBubble(t *testing.T) {
	notice, _, _ := renderItem(ChatItem{Kind: KindNotice, Text: noticeContent, Key: "m-m2"}, 80, false, "")
	if strings.TrimSpace(notice) == "" {
		t.Fatal("a notice rendered to nothing")
	}
	if !strings.Contains(notice, noticeLabel) {
		t.Errorf("the notice carries no %q label, so it is indistinguishable from surrounding prose:\n%s",
			noticeLabel, notice)
	}
	// The archive path must survive the render: it is the one actionable thing in the
	// notice, and wrapping that ate it would defeat the marker.
	if !strings.Contains(notice, "x.bak") {
		t.Errorf("the render dropped the archive path:\n%s", notice)
	}
	// And it must not be the same output as the assistant rendering of the same text,
	// which is what the fall-through produced.
	asAssistant, _, _ := renderItem(ChatItem{Kind: KindText, Text: noticeContent, Key: "m-m2"}, 80, false, "")
	if notice == asAssistant {
		t.Fatal("a notice renders identically to the assistant's own bubble — the distinction the " +
			"operator has to make is exactly this one")
	}
}

func TestAnEmptyNoticeRendersNothingRatherThanABareLabel(t *testing.T) {
	if out := renderNotice("   \n\n  ", 80); strings.TrimSpace(out) != "" {
		t.Fatalf("an empty notice drew %q — a label with no notice under it is worse than nothing", out)
	}
}
