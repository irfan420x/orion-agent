// F-V1 (vision report §7, P2): the CLI Telegram path (`orion
// daemon` / `orion telegram poll`) silently dropped photos —
// newTelegramGateway never wired WithModelInfo, and
// daemonTurnHandler was text-only, so a photo turn ran on its
// caption alone once vision was wired. These tests prove the fix
// at three levels: the handler forwards images to the agent (T4),
// the real gateway wires vision end to end (T6), and the
// photo-captioned-command edge is pinned, not accidental (T7).
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/irfan/orion/internal/budget"
	"github.com/irfan/orion/internal/messaging"
	"github.com/irfan/orion/internal/model"
	"github.com/irfan/orion/internal/permissions"
)

// f1VisionJPEG is a minimal JPEG (header + a few bytes): enough
// for the MIME sniffer, small enough for tests.
var f1VisionJPEG = []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0x00, 0x01, 0x02, 0x03}

// TestDaemonTurnHandlerForwardsImagesToAgent (T4): the handler
// passes a photo turn's images through to the provider — same
// bytes, image/jpeg MIME — and returns footer stats. The
// image-capture assertion is load-bearing: a test that only
// checked the reply would pass while the image was dropped.
func TestDaemonTurnHandlerForwardsImagesToAgent(t *testing.T) {
	prov := &daemonScriptProvider{}
	a := daemonTestApp(t, prov)
	defer a.Close()
	a.Budget = budget.New(100000)
	handler := daemonTurnHandler(a)

	reply, stats, err := handler(context.Background(), 42, "এই ছবিতে কী আছে?",
		[]model.ImageContent{{Data: f1VisionJPEG, MIMEType: "image/jpeg"}}, nil)
	if err != nil {
		t.Fatalf("image turn: err = %v, want a reply", err)
	}
	if reply != "ok" {
		t.Fatalf("image turn reply = %q, want %q", reply, "ok")
	}
	if stats == nil {
		t.Error("image turn stats = nil, want non-nil (a real turn carries footer data)")
	}
	if got := prov.Calls(); got != 1 {
		t.Fatalf("provider calls = %d, want 1", got)
	}
	imageCalls := prov.ImageCalls()
	if len(imageCalls) != 1 || len(imageCalls[0]) != 1 {
		t.Fatalf("provider image calls = %v, want exactly one call carrying exactly one image", imageCalls)
	}
	got := imageCalls[0][0]
	if string(got.Data) != string(f1VisionJPEG) || got.MIMEType != "image/jpeg" {
		t.Errorf("provider image = %d bytes %q, want the JPEG bytes as image/jpeg", len(got.Data), got.MIMEType)
	}

	// A nil-images text turn still runs, with no images.
	reply, stats, err = handler(context.Background(), 42, "hi", nil, nil)
	if err != nil {
		t.Fatalf("text turn: err = %v, want a reply", err)
	}
	if reply != "ok" {
		t.Fatalf("text turn reply = %q, want %q", reply, "ok")
	}
	if stats == nil {
		t.Error("text turn stats = nil, want non-nil (a real turn carries footer data)")
	}
	if got := prov.Calls(); got != 2 {
		t.Fatalf("provider calls = %d, want 2", got)
	}
	imageCalls = prov.ImageCalls()
	if len(imageCalls) != 2 || len(imageCalls[1]) != 0 {
		t.Errorf("text turn carried images %v, want none", imageCalls[1])
	}
}

// f1VisionFakeProvider is a fake OpenAI-compatible provider that
// answers every turn with plain "ok" (no tool calls, so no
// approval gate is involved) and records every image_url part it
// receives, so the end-to-end test can prove the photo's bytes
// actually reached the provider wire.
type f1VisionFakeProvider struct {
	mu       sync.Mutex
	calls    int
	imageURL []string // data: URLs of every image_url part seen
}

func (p *f1VisionFakeProvider) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/chat/completions" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	data, _ := io.ReadAll(r.Body)
	var req struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	_ = json.Unmarshal(data, &req)
	p.mu.Lock()
	p.calls++
	for _, m := range req.Messages {
		var parts []struct {
			Type     string `json:"type"`
			ImageURL struct {
				URL string `json:"url"`
			} `json:"image_url"`
		}
		if err := json.Unmarshal(m.Content, &parts); err != nil {
			continue // string content: no image parts
		}
		for _, part := range parts {
			if part.Type == "image_url" {
				p.imageURL = append(p.imageURL, part.ImageURL.URL)
			}
		}
	}
	p.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
}

func (p *f1VisionFakeProvider) Calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func (p *f1VisionFakeProvider) ImageURLs() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.imageURL...)
}

// f1WriteVisionDaemonTestConfig is rc11WriteDaemonTestConfig with
// the vision-capable model: deepseek-flash, so the CLI gateway's
// WithModelInfo reports vision support for the allowlisted chat.
func f1WriteVisionDaemonTestConfig(t *testing.T, home string, providerURL string) {
	t.Helper()
	body := `{
  "default_provider": "deepseek",
  "providers": {"deepseek": {"api_key": "fake-test-key", "base_url": "` + providerURL + `", "model": "deepseek-flash"}},
  "telegram_token": "123456:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
  "telegram_allowed_chats": [111],
  "data_dir": "` + home + `/.orion",
  "max_steps": 5,
  "timeout_seconds": 30,
  "sandbox": {"enabled": false}
}`
	if err := os.MkdirAll(home+"/.orion", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(home+"/.orion/config.json", []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestCLIGatewayWiresVisionEndToEnd (T6): the real
// newTelegramGateway (via `orion telegram poll`) reports the
// vision-capable model, so a captioned photo downloads, runs a
// real vision turn, and the provider receives exactly one image
// with the photo's bytes — and the chat never gets the
// cannot-see reply. This is the only test that proves the wiring
// (Change 1), as opposed to the handler (T4) or the guard (T1).
func TestCLIGatewayWiresVisionEndToEnd(t *testing.T) {
	home := scratchHome(t)
	provider := &f1VisionFakeProvider{}
	provSrv := httptest.NewServer(provider)
	defer provSrv.Close()
	bot := &rc11FakeBot{fileBytes: map[string][]byte{"big": f1VisionJPEG, "small": f1VisionJPEG}}
	botSrv := httptest.NewServer(bot)
	defer botSrv.Close()
	f1WriteVisionDaemonTestConfig(t, home, provSrv.URL)
	t.Setenv("ORION_TELEGRAM_BASE_URL", botSrv.URL)

	// The photo is queued before polling starts, so the first
	// getUpdates delivers it.
	bot.enqueue(messaging.Update{
		UpdateID: 1,
		Message: &messaging.TGMessage{
			MessageID: 1,
			Chat:      messaging.TGChat{ID: 111},
			Caption:   "এই ছবিতে কী আছে?",
			Photo: []messaging.TGPhotoSize{
				{FileID: "small", Width: 90, Height: 67},
				{FileID: "big", Width: 1280, Height: 960},
			},
			From: &messaging.TGUser{ID: 111},
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		_, err := capture(func() error { return cmdTelegram(ctx, []string{"poll"}) })
		errCh <- err
	}()

	// The vision turn's reply is the agent's "ok" plus the
	// footer (oriond parity: stats are returned, so the footer
	// is appended). Waiting on the reply proves the turn ran;
	// the image assertions below prove it ran WITH the photo.
	rc11WaitFor(t, "vision turn reply for chat 111", func() bool {
		bot.mu.Lock()
		defer bot.mu.Unlock()
		for _, s := range bot.sent {
			if s.ChatID == 111 && strings.Contains(s.Text, "ok") {
				return true
			}
		}
		return false
	})
	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("cmdTelegram poll after cancel: err = %v, want nil (PollStream returns nil on ctx cancellation)", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("cmdTelegram poll did not return after cancelling its context")
	}

	// The provider received exactly one image: the photo's bytes
	// as a JPEG data URL — no caption-only turn, no re-send.
	wantURL := "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(f1VisionJPEG)
	urls := provider.ImageURLs()
	if len(urls) != 1 || urls[0] != wantURL {
		t.Errorf("provider image URLs = %v, want exactly [%s]", urls, wantURL)
	}
	if got := provider.Calls(); got == 0 {
		t.Error("provider was never called — the turn never started")
	}
	// The largest size was downloaded, not the small one.
	bot.mu.Lock()
	ids := append([]string(nil), bot.getFileIDs...)
	sent := append([]rc11BotSent(nil), bot.sent...)
	bot.mu.Unlock()
	if len(ids) != 1 || ids[0] != "big" {
		t.Errorf("getFile file_ids = %v, want [big] (largest size)", ids)
	}
	// No cannot-see reply, and the agent's reply came back with
	// its footer (stats != nil on a real turn).
	sawOK, sawFooter := false, false
	for _, m := range sent {
		if m.Text == messaging.CannotSeeImagesMessage {
			t.Errorf("vision-capable chat got the cannot-see reply")
		}
		if strings.Contains(m.Text, "ok") {
			sawOK = true
			if strings.Contains(m.Text, "tokens") {
				sawFooter = true
			}
		}
	}
	if !sawOK {
		t.Errorf("chat got no agent reply; sent = %v", sent)
	}
	if !sawFooter {
		t.Errorf("agent reply carries no footer; sent = %v (CLI turns return stats, so the footer must be appended)", sent)
	}
}

// TestPhotoCaptionedRulesPinsCommandWins (T7): a photo captioned
// /rules pins the CURRENT behaviour — the command wins and the
// image is dropped, exactly as oriond's buildTurnHandler does
// (it dispatches the registry before running the turn). This is
// a deliberate decision record, not a fix: changing dispatch
// order would have to change oriond too, and is out of F-V1's
// scope. If dispatch order ever changes, this test must change
// with it — it must never silently pass while the behaviour
// drifts.
func TestPhotoCaptionedRulesPinsCommandWins(t *testing.T) {
	const owner = 111
	prov := &daemonScriptProvider{}
	a := daemonTestApp(t, prov)
	defer a.Close()
	a.Budget = budget.New(100000)
	a.Config.TelegramAllowedChats = []int64{owner}
	// The app's effective model must be vision-capable, so the
	// photo passes the vision gate and reaches the handler with
	// its image — the /rules dispatch inside the handler is
	// what drops it. (config.Default() is ollama/llama3.1, which
	// has no vision support; the deepseek entry in the default
	// config already names deepseek-flash, so only the default
	// provider needs switching.)
	a.Config.DefaultProvider = "deepseek"
	gate, err := a.NewPermissionGate(nil)
	if err != nil {
		t.Fatalf("NewPermissionGate: %v", err)
	}
	if gate == nil || gate.Rules() == nil {
		t.Fatal("NewPermissionGate returned no usable rule store")
	}
	if err := gate.Rules().Set("shell_exec", permissions.ActionAllow); err != nil {
		t.Fatalf("seed shell_exec rule: %v", err)
	}

	bot := &rc11FakeBot{fileBytes: map[string][]byte{"big": f1VisionJPEG, "small": f1VisionJPEG}}
	botSrv := httptest.NewServer(bot)
	defer botSrv.Close()
	gw, err := messaging.NewTelegramGateway("123456:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		messaging.WithBaseURL(botSrv.URL),
		messaging.WithAllowedChats(owner),
		messaging.WithModelInfo(func(chatID int64) (string, string) {
			provider, modelName, _ := a.ModelInfoForChat(chatID)
			return provider, modelName
		}))
	if err != nil {
		t.Fatalf("NewTelegramGateway: %v", err)
	}

	bot.enqueue(messaging.Update{
		UpdateID: 1,
		Message: &messaging.TGMessage{
			MessageID: 1,
			Chat:      messaging.TGChat{ID: owner},
			Caption:   "/rules",
			Photo: []messaging.TGPhotoSize{
				{FileID: "small", Width: 90, Height: 67},
				{FileID: "big", Width: 1280, Height: 960},
			},
			From: &messaging.TGUser{ID: owner},
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- gw.PollStream(ctx, daemonTurnHandler(a)) }()

	// The rules list is the reply: the command won.
	rc11WaitFor(t, "rules list reply for chat 111", func() bool {
		bot.mu.Lock()
		defer bot.mu.Unlock()
		for _, s := range bot.sent {
			if s.ChatID == owner && strings.Contains(s.Text, "shell_exec") {
				return true
			}
		}
		return false
	})
	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("PollStream after cancel: %v, want nil", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("PollStream did not return after cancelling its context")
	}

	// No agent turn ran: the provider never saw the photo (or
	// anything else) — the image was dropped with the command.
	if got := prov.Calls(); got != 0 {
		t.Errorf("provider calls = %d, want 0 (a /rules caption must not run a turn)", got)
	}
	if imgs := prov.ImageCalls(); len(imgs) != 0 {
		t.Errorf("provider image calls = %v, want none", imgs)
	}
	bot.mu.Lock()
	sent := append([]rc11BotSent(nil), bot.sent...)
	bot.mu.Unlock()
	for _, m := range sent {
		if m.Text == messaging.CannotSeeImagesMessage {
			t.Errorf("chat got the cannot-see reply for a /rules caption, want the rules list")
		}
	}
}
