// Tests for image-bearing messages (computer-use M1): the three
// providers must serialise Message.Images into the shape each API
// accepts, and text-only messages must serialise exactly as before.
package model

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func testImageMessage() Message {
	return Message{
		Role:       "tool",
		Content:    `{"backend":"spectacle","width":1280,"height":720}`,
		ToolCallID: "call-1",
		Images: []ImageContent{
			{MIMEType: "image/jpeg", Data: []byte{0xFF, 0xD8, 0xFF, 0xE0}},
		},
	}
}

func TestOpenAIContentTextOnlyUnchanged(t *testing.T) {
	msgs := toOpenAIMessages([]Message{{Role: "user", Content: "hello"}})
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1", len(msgs))
	}
	content, ok := msgs[0].Content.(string)
	if !ok || content != "hello" {
		t.Errorf("text-only content = %#v, want the plain string %q", msgs[0].Content, "hello")
	}
}

func TestOpenAIContentWithImage(t *testing.T) {
	msgs := toOpenAIMessages([]Message{testImageMessage()})
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1", len(msgs))
	}
	parts, ok := msgs[0].Content.([]openAIContentPart)
	if !ok {
		t.Fatalf("content = %#v, want []openAIContentPart", msgs[0].Content)
	}
	if len(parts) != 2 {
		t.Fatalf("got %d parts, want 2 (text + image)", len(parts))
	}
	if parts[0].Type != "text" || !strings.Contains(parts[0].Text, "spectacle") {
		t.Errorf("part 0 = %+v, want the text part", parts[0])
	}
	if parts[1].Type != "image_url" || parts[1].ImageURL == nil {
		t.Fatalf("part 1 = %+v, want an image_url part", parts[1])
	}
	want := "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString([]byte{0xFF, 0xD8, 0xFF, 0xE0})
	if parts[1].ImageURL.URL != want {
		t.Errorf("image URL = %q, want %q", parts[1].ImageURL.URL, want)
	}
	// The wire shape must marshal as content parts, not a string.
	raw, err := json.Marshal(msgs[0])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var wire struct {
		Content []struct {
			Type     string `json:"type"`
			ImageURL *struct {
				URL string `json:"url"`
			} `json:"image_url,omitempty"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("wire content does not unmarshal as parts: %v (raw %s)", err, raw)
	}
	if len(wire.Content) != 2 || wire.Content[1].ImageURL == nil {
		t.Errorf("wire content = %s, want 2 parts with an image_url", raw)
	}
}

func TestOpenAIContentSkipsBadImages(t *testing.T) {
	m := testImageMessage()
	m.Images = []ImageContent{
		{MIMEType: "image/gif", Data: []byte{1}},
		{MIMEType: "image/png", Data: nil},
	}
	content := openAIContent(m)
	parts, ok := content.([]openAIContentPart)
	if !ok {
		t.Fatalf("content = %#v, want parts (text only)", content)
	}
	if len(parts) != 1 || parts[0].Type != "text" {
		t.Errorf("parts = %+v, want just the text part", parts)
	}
}

func TestAnthropicToolResultWithImage(t *testing.T) {
	_, out, err := toAnthropicMessages([]Message{
		{Role: "assistant", Content: "", ToolCalls: []ToolCall{{ID: "call-1", Name: "screenshot", Arguments: "{}"}}},
		testImageMessage(),
	})
	if err != nil {
		t.Fatalf("toAnthropicMessages: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("got %d turns, want 2", len(out))
	}
	blocks, ok := out[1].Content.([]anthropicContentBlock)
	if !ok {
		t.Fatalf("turn content = %#v, want []anthropicContentBlock", out[1].Content)
	}
	if len(blocks) != 1 || blocks[0].Type != "tool_result" {
		t.Fatalf("blocks = %+v, want one tool_result block", blocks)
	}
	inner, ok := blocks[0].Content.([]anthropicContentBlock)
	if !ok {
		t.Fatalf("tool_result content = %#v, want []anthropicContentBlock", blocks[0].Content)
	}
	if len(inner) != 2 {
		t.Fatalf("got %d inner blocks, want 2 (text + image)", len(inner))
	}
	if inner[1].Type != "image" || inner[1].Source == nil || inner[1].Source.MediaType != "image/jpeg" {
		t.Errorf("inner block 1 = %+v, want an image block", inner[1])
	}
	want := base64.StdEncoding.EncodeToString([]byte{0xFF, 0xD8, 0xFF, 0xE0})
	if inner[1].Source.Data != want {
		t.Errorf("image data = %q, want base64 %q", inner[1].Source.Data, want)
	}
}

func TestAnthropicToolResultTextOnlyUnchanged(t *testing.T) {
	_, out, err := toAnthropicMessages([]Message{
		{Role: "assistant", Content: "", ToolCalls: []ToolCall{{ID: "call-1", Name: "calculator", Arguments: "{}"}}},
		{Role: "tool", Content: "2", ToolCallID: "call-1"},
	})
	if err != nil {
		t.Fatalf("toAnthropicMessages: %v", err)
	}
	blocks, ok := out[1].Content.([]anthropicContentBlock)
	if !ok || len(blocks) != 1 {
		t.Fatalf("turn content = %#v, want one tool_result block", out[1].Content)
	}
	if blocks[0].Content != "2" {
		t.Errorf("tool_result content = %#v, want the plain string %q", blocks[0].Content, "2")
	}
}

func TestOllamaMessagesWithImage(t *testing.T) {
	msgs, err := toOllamaMessages([]Message{testImageMessage()})
	if err != nil {
		t.Fatalf("toOllamaMessages: %v", err)
	}
	if len(msgs) != 1 || len(msgs[0].Images) != 1 {
		t.Fatalf("images = %v, want 1 base64 image", msgs[0].Images)
	}
	want := base64.StdEncoding.EncodeToString([]byte{0xFF, 0xD8, 0xFF, 0xE0})
	if msgs[0].Images[0] != want {
		t.Errorf("ollama image = %q, want %q", msgs[0].Images[0], want)
	}
}

func TestOllamaMessagesTextOnlyUnchanged(t *testing.T) {
	msgs, err := toOllamaMessages([]Message{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatalf("toOllamaMessages: %v", err)
	}
	if len(msgs[0].Images) != 0 {
		t.Errorf("text-only message carried %d images, want 0", len(msgs[0].Images))
	}
}
