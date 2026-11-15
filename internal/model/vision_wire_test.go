package model

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

// tiny valid image headers, one per supported format.
var (
	testJPEG = []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0x00, 0x01}
	testPNG  = []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A, 0x00, 0x00, 0x00, 0x0D}
	testGIF  = []byte{'G', 'I', 'F', '8', '9', 'a', 0x01, 0x00}
	testWebP = []byte{'R', 'I', 'F', 'F', 0x10, 0x00, 0x00, 0x00, 'W', 'E', 'B', 'P', 0x00}
)

func TestSniffImageMIME(t *testing.T) {
	cases := []struct {
		data []byte
		want string
	}{
		{testJPEG, "image/jpeg"},
		{testPNG, "image/png"},
		{testGIF, "image/gif"},
		{testWebP, "image/webp"},
		{[]byte("GIF87a"), "image/gif"},
		{[]byte("not an image at all"), ""},
		{[]byte{}, ""},
		{nil, ""},
		{[]byte{0xFF, 0xD8}, ""},         // truncated JPEG header
		{[]byte{'R', 'I', 'F', 'F'}, ""}, // truncated WebP header
	}
	for _, tc := range cases {
		if got := SniffImageMIME(tc.data); got != tc.want {
			t.Errorf("SniffImageMIME(%q) = %q, want %q", tc.data, got, tc.want)
		}
	}
}

// marshalWire renders toOpenAIMessages as the JSON a provider would
// receive, so the tests assert the wire shape, not Go values.
func marshalWire(t *testing.T, msgs []Message) string {
	t.Helper()
	out := toOpenAIMessages(msgs)
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal wire messages: %v", err)
	}
	return string(b)
}

func TestToOpenAIMessagesNoImagesStaysString(t *testing.T) {
	// Backward compatibility: a message with no images serialises
	// content as a plain JSON string, exactly as before.
	got := marshalWire(t, []Message{
		{Role: "system", Content: "be helpful"},
		{Role: "user", Content: "hello"},
	})
	want := `[{"role":"system","content":"be helpful"},{"role":"user","content":"hello"}]`
	if got != want {
		t.Errorf("wire = %s, want %s", got, want)
	}
}

func TestToOpenAIMessagesWithImage(t *testing.T) {
	got := marshalWire(t, []Message{
		{Role: "user", Content: "what is this?", Images: []ImageContent{{Data: testJPEG}}},
	})
	// Text part first, then the image_url part with a base64 data URL.
	if !strings.Contains(got, `{"type":"text","text":"what is this?"}`) {
		t.Errorf("wire missing text part: %s", got)
	}
	wantURL := "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(testJPEG)
	if !strings.Contains(got, `"type":"image_url","image_url":{"url":"`+wantURL+`"}`) {
		t.Errorf("wire missing image_url part: %s", got)
	}
}

func TestToOpenAIMessagesDeclaredMIMENeverTrusted(t *testing.T) {
	// PNG bytes declared as image/jpeg must go out as image/png:
	// the sniffed type wins over the declared one.
	got := marshalWire(t, []Message{
		{Role: "user", Content: "x", Images: []ImageContent{{
			Data: testPNG, MIMEType: "image/jpeg",
		}}},
	})
	if !strings.Contains(got, "data:image/png;base64,") {
		t.Errorf("wire must use sniffed image/png, got: %s", got)
	}
	if strings.Contains(got, "data:image/jpeg;base64,") {
		t.Errorf("wire must not use the declared image/jpeg, got: %s", got)
	}
}

func TestToOpenAIMessagesUnknownImageSkipped(t *testing.T) {
	// Bytes of an unknown type are dropped, never sent with a
	// guessed MIME type; with text present the message stays a
	// plain string (the historic shape).
	got := marshalWire(t, []Message{
		{Role: "user", Content: "x", Images: []ImageContent{{Data: []byte("not an image")}}},
	})
	want := `[{"role":"user","content":"x"}]`
	if got != want {
		t.Errorf("wire = %s, want %s", got, want)
	}
}

func TestToOpenAIMessagesHistoryRule(t *testing.T) {
	// Only the most recent user message keeps its image parts.
	// The older one is sent text-only with the [ছবি] placeholder,
	// so its base64 is never re-sent on a later turn.
	msgs := []Message{
		{Role: "user", Content: "first photo", Images: []ImageContent{{Data: testJPEG}}},
		{Role: "assistant", Content: "a cat"},
		{Role: "user", Content: "second photo", Images: []ImageContent{{Data: testPNG}}},
	}
	got := marshalWire(t, msgs)
	if !strings.Contains(got, `"content":"first photo [ছবি]"`) {
		t.Errorf("older image message must be text-only with placeholder, got: %s", got)
	}
	if strings.Contains(got, base64.StdEncoding.EncodeToString(testJPEG)) {
		t.Errorf("older image bytes must not be re-sent, got: %s", got)
	}
	if !strings.Contains(got, "data:image/png;base64,") {
		t.Errorf("most recent image must be sent, got: %s", got)
	}

	// An older image-only message (no text) becomes just the placeholder.
	got = marshalWire(t, []Message{
		{Role: "user", Images: []ImageContent{{Data: testJPEG}}},
		{Role: "user", Content: "now this", Images: []ImageContent{{Data: testPNG}}},
	})
	if !strings.Contains(got, `"content":"[ছবি]"`) {
		t.Errorf("older image-only message must become the placeholder, got: %s", got)
	}
}

func TestToOpenAIMessagesImageInAssistantMessageStripped(t *testing.T) {
	// DeepSeek rejects images in assistant messages with a 400,
	// so an assistant message carrying images is sent text-only
	// with the placeholder, never with parts.
	got := marshalWire(t, []Message{
		{Role: "assistant", Content: "here it is", Images: []ImageContent{{Data: testJPEG}}},
		{Role: "user", Content: "thanks"},
	})
	if strings.Contains(got, "image_url") {
		t.Errorf("assistant image must never be sent, got: %s", got)
	}
	if !strings.Contains(got, `"content":"here it is [ছবি]"`) {
		t.Errorf("assistant image message must keep text + placeholder, got: %s", got)
	}
}

func TestBuildRequestWithImage(t *testing.T) {
	// End-to-end through buildRequest: the model name and the
	// parts array both reach the request body.
	p := &openAIProvider{cfg: ProviderConfig{Name: "deepseek", Model: "deepseek-flash"}}
	req := p.buildRequest(ChatRequest{Messages: []Message{
		{Role: "user", Content: "describe", Images: []ImageContent{{Data: testGIF}}},
	}}, false)
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	if !strings.Contains(string(b), `"model":"deepseek-flash"`) {
		t.Errorf("request missing model: %s", b)
	}
	if !strings.Contains(string(b), "data:image/gif;base64,") {
		t.Errorf("request missing image part: %s", b)
	}
}
