// Tests for screenshot history eviction (computer-use M1,
// plan §4.7-3): at most the last 3 screenshot images stay in the
// provider-facing history; earlier ones become a text summary.
package core

import (
	"strings"
	"testing"

	"github.com/irfan/orion/internal/model"
)

func shotMessage(id string) model.Message {
	return model.Message{
		Role:       "tool",
		Content:    `{"backend":"spectacle","file":"` + id + `.jpg"}`,
		ToolCallID: id,
		Images:     []model.ImageContent{{MIMEType: "image/jpeg", Data: []byte{1, 2, 3}}},
	}
}

func TestEvictOldScreenshotImagesKeepsLastThree(t *testing.T) {
	msgs := []model.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "hi"},
		shotMessage("a"), shotMessage("b"), shotMessage("c"), shotMessage("d"), shotMessage("e"),
		{Role: "user", Content: "what do you see?"},
	}
	out := evictOldScreenshotImages(msgs)
	if len(out) != len(msgs) {
		t.Fatalf("evicted %d messages, want %d (eviction never drops messages)", len(out), len(msgs))
	}
	// a and b lose their images; c, d, e keep them.
	for i, id := range []string{"a", "b"} {
		m := out[2+i]
		if len(m.Images) != 0 {
			t.Errorf("message %s kept %d images, want 0", id, len(m.Images))
		}
		if !strings.Contains(m.Content, "evicted from history") {
			t.Errorf("message %s content = %q, want an eviction note", id, m.Content)
		}
		if !strings.Contains(m.Content, id+".jpg") {
			t.Errorf("message %s lost its JSON summary: %q", id, m.Content)
		}
	}
	for i, id := range []string{"c", "d", "e"} {
		if m := out[4+i]; len(m.Images) != 1 {
			t.Errorf("message %s kept %d images, want 1", id, len(m.Images))
		}
	}
	// The input slice is never modified.
	if len(msgs[2].Images) != 1 {
		t.Errorf("input message a was modified by eviction")
	}
	// Non-image messages pass through untouched.
	if out[0].Content != "sys" || out[7].Content != "what do you see?" {
		t.Errorf("non-image messages changed: %+v", out)
	}
}

func TestEvictOldScreenshotImagesUnderLimit(t *testing.T) {
	msgs := []model.Message{shotMessage("a"), shotMessage("b"), shotMessage("c")}
	out := evictOldScreenshotImages(msgs)
	for i, m := range out {
		if len(m.Images) != 1 {
			t.Errorf("message %d kept %d images, want 1", i, len(m.Images))
		}
		if strings.Contains(m.Content, "evicted") {
			t.Errorf("message %d gained an eviction note under the limit", i)
		}
	}
}

func TestStripImagesForStorage(t *testing.T) {
	m := shotMessage("a")
	stored := stripImagesForStorage(m)
	if len(stored.Images) != 0 {
		t.Errorf("stored message kept %d images, want 0", len(stored.Images))
	}
	if !strings.Contains(stored.Content, "not stored") {
		t.Errorf("stored content = %q, want a not-stored note", stored.Content)
	}
	// Text-only messages pass through unchanged.
	plain := model.Message{Role: "user", Content: "hi"}
	if got := stripImagesForStorage(plain); got.Content != "hi" || len(got.Images) != 0 {
		t.Errorf("stripImagesForStorage(text-only) = %+v, want unchanged", got)
	}
}

func TestMessageTokensCountsImages(t *testing.T) {
	plain := model.Message{Role: "tool", Content: "x"}
	withImage := shotMessage("a")
	withImage.Content = "x"
	got := messageTokens(withImage) - messageTokens(plain)
	if got != model.EstimatedTokensPerImage {
		t.Errorf("one image added %d tokens, want %d", got, model.EstimatedTokensPerImage)
	}
}
