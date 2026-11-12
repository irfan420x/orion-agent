package messaging

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// test-token-123 is a fake token used only against httptest servers. It is
// never sent to Telegram.
const testToken = "test-token-123"

func newTestGateway(t *testing.T, srv *httptest.Server) *TelegramGateway {
	t.Helper()
	g, err := NewTelegramGateway(testToken, WithBaseURL(srv.URL))
	if err != nil {
		t.Fatalf("NewTelegramGateway: %v", err)
	}
	return g
}

// TestNewTelegramGatewayEmptyToken verifies an empty token is rejected.
func TestNewTelegramGatewayEmptyToken(t *testing.T) {
	for _, token := range []string{"", "   "} {
		if _, err := NewTelegramGateway(token); err == nil {
			t.Fatalf("NewTelegramGateway(%q): expected error, got nil", token)
		}
	}
}

// TestGetMeSuccess verifies GetMe parses username from an ok response.
func TestGetMeSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/bot"+testToken+"/getMe" {
			t.Errorf("GetMe path = %q, want /bot%s/getMe", r.URL.Path, testToken)
		}
		if r.Method != http.MethodGet {
			t.Errorf("GetMe method = %q, want GET", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true,"result":{"id":123,"is_bot":true,"first_name":"Orion","username":"orion_test_bot"}}`)
	}))
	defer srv.Close()

	username, err := newTestGateway(t, srv).GetMe(context.Background())
	if err != nil {
		t.Fatalf("GetMe: %v", err)
	}
	if username != "orion_test_bot" {
		t.Fatalf("GetMe username = %q, want %q", username, "orion_test_bot")
	}
}

// TestCallRedactsTokenOnTransportError verifies a transport failure
// (here: connection refused, whose *url.Error embeds the full request
// URL including the token) never leaks the token into the returned
// error string: the token is replaced with "[redacted]".
func TestCallRedactsTokenOnTransportError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // every request now fails with connection refused

	g, err := NewTelegramGateway(testToken, WithBaseURL(url))
	if err != nil {
		t.Fatalf("NewTelegramGateway: %v", err)
	}
	_, err = g.GetMe(context.Background())
	if err == nil {
		t.Fatal("GetMe against a closed server: expected error, got nil")
	}
	if strings.Contains(err.Error(), testToken) {
		t.Fatalf("GetMe error leaks the token: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "[redacted]") {
		t.Fatalf("GetMe error = %q, want it to contain %q", err.Error(), "[redacted]")
	}
}

// TestCallRedactsTokenOnTimeout verifies a client-timeout error (whose
// *url.Error also embeds the request URL) is redacted the same way,
// and that the timeout is still detectable via errors.As (net.Error
// with Timeout() true) despite the redaction wrapper.
func TestCallRedactsTokenOnTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true,"result":{"id":1,"username":"x","first_name":"X"}}`)
	}))
	defer srv.Close()

	g, err := NewTelegramGateway(testToken,
		WithBaseURL(srv.URL),
		WithHTTPClient(&http.Client{Timeout: 50 * time.Millisecond}))
	if err != nil {
		t.Fatalf("NewTelegramGateway: %v", err)
	}
	_, err = g.GetMe(context.Background())
	if err == nil {
		t.Fatal("GetMe against a slow server: expected timeout error, got nil")
	}
	if strings.Contains(err.Error(), testToken) {
		t.Fatalf("GetMe error leaks the token: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "[redacted]") {
		t.Fatalf("GetMe error = %q, want it to contain %q", err.Error(), "[redacted]")
	}
	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Fatalf("GetMe error = %v, want a timeout error detectable via errors.As(net.Error)", err)
	}
}

// TestGetMeFailure verifies a non-ok response is an error carrying the description.
func TestGetMeFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"ok":false,"error_code":401,"description":"Unauthorized"}`)
	}))
	defer srv.Close()

	_, err := newTestGateway(t, srv).GetMe(context.Background())
	if err == nil {
		t.Fatal("GetMe: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "Unauthorized") {
		t.Fatalf("GetMe error = %q, want it to contain %q", err.Error(), "Unauthorized")
	}
}

// sentMessage records one sendMessage call seen by a fake server.
type sentMessage struct {
	ChatID int64  `json:"chat_id"`
	Text   string `json:"text"`
}

// fakeTelegram records sendMessage calls and serves scripted getUpdates.
type fakeTelegram struct {
	mu          sync.Mutex
	sent        []sentMessage
	sendCh      chan sentMessage // buffered; one value per send, for Poll tests
	updates     []Update         // returned on the first getUpdates call
	updatesMu   sync.Mutex
	updateCalls int
	offsets     []string
}

func (f *fakeTelegram) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch {
	case strings.HasSuffix(r.URL.Path, "/sendMessage"):
		body, _ := io.ReadAll(r.Body)
		var msg sentMessage
		if err := json.Unmarshal(body, &msg); err != nil {
			t := http.StatusBadRequest
			w.WriteHeader(t)
			fmt.Fprintf(w, `{"ok":false,"description":"bad json: %v"}`, err)
			return
		}
		f.mu.Lock()
		f.sent = append(f.sent, msg)
		f.mu.Unlock()
		select {
		case f.sendCh <- msg:
		default:
		}
		fmt.Fprint(w, `{"ok":true,"result":{"message_id":1}}`)
	case strings.HasSuffix(r.URL.Path, "/getUpdates"):
		f.updatesMu.Lock()
		f.updateCalls++
		call := f.updateCalls
		f.offsets = append(f.offsets, r.URL.Query().Get("offset"))
		f.updatesMu.Unlock()
		var out []Update
		if call == 1 {
			out = f.updates
		} else {
			out = []Update{}
		}
		data, _ := json.Marshal(map[string]any{"ok": true, "result": out})
		w.Write(data)
	case strings.HasSuffix(r.URL.Path, "/getMe"):
		fmt.Fprint(w, `{"ok":true,"result":{"id":1,"username":"orion_test_bot","first_name":"Orion"}}`)
	default:
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"ok":false,"description":"not found"}`)
	}
}

func (f *fakeTelegram) sentSnapshot() []sentMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]sentMessage, len(f.sent))
	copy(out, f.sent)
	return out
}

// TestSendMessagePostsJSON verifies sendMessage posts the correct JSON body.
func TestSendMessagePostsJSON(t *testing.T) {
	fake := &fakeTelegram{sendCh: make(chan sentMessage, 10)}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	err := newTestGateway(t, srv).SendMessage(context.Background(), 42, "hello world")
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	sent := fake.sentSnapshot()
	if len(sent) != 1 {
		t.Fatalf("sendMessage calls = %d, want 1", len(sent))
	}
	if sent[0].ChatID != 42 || sent[0].Text != "hello world" {
		t.Fatalf("sendMessage = %+v, want {ChatID:42 Text:hello world}", sent[0])
	}
}

// TestSendMessageSplitsLongMessage verifies a >4096-char message is split.
func TestSendMessageSplitsLongMessage(t *testing.T) {
	fake := &fakeTelegram{sendCh: make(chan sentMessage, 10)}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	text := strings.Repeat("a", 5000)
	err := newTestGateway(t, srv).SendMessage(context.Background(), 7, text)
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	sent := fake.sentSnapshot()
	if len(sent) != 2 {
		t.Fatalf("sendMessage calls = %d, want 2", len(sent))
	}
	if len([]rune(sent[0].Text)) != 4096 {
		t.Fatalf("first chunk length = %d runes, want 4096", len([]rune(sent[0].Text)))
	}
	if len([]rune(sent[1].Text)) != 904 {
		t.Fatalf("second chunk length = %d runes, want 904", len([]rune(sent[1].Text)))
	}
	if sent[0].Text+sent[1].Text != text {
		t.Fatal("split chunks do not reassemble to the original text")
	}
	for _, m := range sent {
		if m.ChatID != 7 {
			t.Fatalf("chunk chat_id = %d, want 7", m.ChatID)
		}
	}
}

// TestGetUpdatesParsesAndAdvancesOffset verifies parsing and offset advance.
func TestGetUpdatesParsesAndAdvancesOffset(t *testing.T) {
	fake := &fakeTelegram{
		sendCh: make(chan sentMessage, 10),
		updates: []Update{
			{UpdateID: 10, Message: &TGMessage{MessageID: 1, Chat: TGChat{ID: 99}, Text: "first", From: &TGUser{ID: 5, Username: "alice"}}},
			{UpdateID: 12, Message: &TGMessage{MessageID: 2, Chat: TGChat{ID: 99}, Text: "second"}},
		},
	}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	g := newTestGateway(t, srv)

	updates, err := g.GetUpdates(context.Background())
	if err != nil {
		t.Fatalf("GetUpdates: %v", err)
	}
	if len(updates) != 2 {
		t.Fatalf("updates = %d, want 2", len(updates))
	}
	if updates[0].Message.Text != "first" || updates[1].Message.Text != "second" {
		t.Fatalf("updates texts = %q, %q", updates[0].Message.Text, updates[1].Message.Text)
	}
	if updates[0].Message.From == nil || updates[0].Message.From.Username != "alice" {
		t.Fatalf("first update From = %+v, want username alice", updates[0].Message.From)
	}
	if got := g.Offset(); got != 13 {
		t.Fatalf("offset after GetUpdates = %d, want 13", got)
	}

	// Second call must request the advanced offset and return nothing new.
	updates, err = g.GetUpdates(context.Background())
	if err != nil {
		t.Fatalf("second GetUpdates: %v", err)
	}
	if len(updates) != 0 {
		t.Fatalf("second GetUpdates = %d updates, want 0", len(updates))
	}
	fake.updatesMu.Lock()
	offsets := append([]string(nil), fake.offsets...)
	fake.updatesMu.Unlock()
	if len(offsets) != 2 || offsets[0] != "0" || offsets[1] != "13" {
		t.Fatalf("getUpdates offsets = %v, want [0 13]", offsets)
	}
}

// TestPollDeliversMessageAndSendsReply verifies Poll delivers a message
// to the handler and sends the handler's reply.
func TestPollDeliversMessageAndSendsReply(t *testing.T) {
	fake := &fakeTelegram{
		sendCh: make(chan sentMessage, 10),
		updates: []Update{
			{UpdateID: 1, Message: &TGMessage{MessageID: 1, Chat: TGChat{ID: 77}, Text: "hi"}},
		},
	}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	g := newTestGateway(t, srv)

	var handlerCalls []string
	var handlerMu sync.Mutex
	handler := func(ctx context.Context, chatID int64, text string) (string, error) {
		handlerMu.Lock()
		handlerCalls = append(handlerCalls, text)
		handlerMu.Unlock()
		if chatID != 77 {
			t.Errorf("handler chatID = %d, want 77", chatID)
		}
		return "pong: " + text, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- g.Poll(ctx, handler) }()

	// Wait for the reply to be sent, then stop polling.
	var sent sentMessage
	select {
	case sent = <-fake.sendCh:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("timed out waiting for Poll to send a reply")
	}
	if sent.ChatID != 77 || sent.Text != "pong: hi" {
		t.Fatalf("Poll reply = %+v, want {ChatID:77 Text:pong: hi}", sent)
	}
	handlerMu.Lock()
	calls := append([]string(nil), handlerCalls...)
	handlerMu.Unlock()
	if len(calls) != 1 || calls[0] != "hi" {
		t.Fatalf("handler calls = %v, want [hi]", calls)
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Poll after cancel: %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Poll did not return after ctx cancel")
	}
}

// TestPollHandlerError verifies a handler error is reported to the user
// and polling continues.
func TestPollHandlerError(t *testing.T) {
	fake := &fakeTelegram{
		sendCh: make(chan sentMessage, 10),
		updates: []Update{
			{UpdateID: 1, Message: &TGMessage{MessageID: 1, Chat: TGChat{ID: 8}, Text: "boom"}},
		},
	}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	g := newTestGateway(t, srv)

	handler := func(ctx context.Context, chatID int64, text string) (string, error) {
		return "", errors.New("kaboom")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- g.Poll(ctx, handler) }()

	select {
	case sent := <-fake.sendCh:
		if sent.ChatID != 8 || !strings.Contains(sent.Text, "Sorry, I hit an error: kaboom") {
			t.Fatalf("error reply = %+v, want it to contain %q", sent, "Sorry, I hit an error: kaboom")
		}
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("timed out waiting for Poll error reply")
	}
	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Poll after cancel: %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Poll did not return after ctx cancel")
	}
}

// TestPollStopsCleanlyOnCancel verifies Poll returns nil when ctx is cancelled.
func TestPollStopsCleanlyOnCancel(t *testing.T) {
	fake := &fakeTelegram{sendCh: make(chan sentMessage, 10)}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	g := newTestGateway(t, srv)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- g.Poll(ctx, func(ctx context.Context, chatID int64, text string) (string, error) { return "", nil })
	}()
	time.AfterFunc(200*time.Millisecond, cancel)

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Poll: %v, want nil on ctx cancel", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Poll did not return after ctx cancel")
	}
}

// TestPollNilHandler verifies Poll rejects a nil handler.
func TestPollNilHandler(t *testing.T) {
	g, err := NewTelegramGateway(testToken)
	if err != nil {
		t.Fatalf("NewTelegramGateway: %v", err)
	}
	if err := g.Poll(context.Background(), nil); err == nil {
		t.Fatal("Poll(nil handler): expected error, got nil")
	}
}

// TestRunCLI verifies the prompt loop echoes input and quits on /quit.
func TestRunCLI(t *testing.T) {
	in := strings.NewReader("hello\n/quit\n")
	var out strings.Builder
	handler := func(ctx context.Context, input string) (string, error) {
		return "echo: " + input, nil
	}
	if err := RunCLI(context.Background(), in, &out, handler); err != nil {
		t.Fatalf("RunCLI: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "orion> ") {
		t.Fatalf("RunCLI output = %q, want it to contain the prompt %q", got, "orion> ")
	}
	if !strings.Contains(got, "echo: hello") {
		t.Fatalf("RunCLI output = %q, want it to contain %q", got, "echo: hello")
	}
}

// TestRunCLIEmptyLineAndExit verifies empty lines reprompt and /exit quits.
func TestRunCLIEmptyLineAndExit(t *testing.T) {
	in := strings.NewReader("\n  \nhello\n/exit\n")
	var out strings.Builder
	calls := 0
	handler := func(ctx context.Context, input string) (string, error) {
		calls++
		return "got " + input, nil
	}
	if err := RunCLI(context.Background(), in, &out, handler); err != nil {
		t.Fatalf("RunCLI: %v", err)
	}
	if calls != 1 {
		t.Fatalf("handler calls = %d, want 1 (empty lines must not call it)", calls)
	}
	if got := strings.Count(out.String(), "orion> "); got < 3 {
		t.Fatalf("prompt count = %d, want >= 3, output = %q", got, out.String())
	}
}

// TestRunCLIHandlerErrorContinues verifies a handler error is printed
// and the loop continues with the next line.
func TestRunCLIHandlerErrorContinues(t *testing.T) {
	in := strings.NewReader("bad\ngood\n/quit\n")
	var out strings.Builder
	handler := func(ctx context.Context, input string) (string, error) {
		if input == "bad" {
			return "", errors.New("nope")
		}
		return "ok: " + input, nil
	}
	if err := RunCLI(context.Background(), in, &out, handler); err != nil {
		t.Fatalf("RunCLI: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "error: nope") {
		t.Fatalf("RunCLI output = %q, want it to contain %q", got, "error: nope")
	}
	if !strings.Contains(got, "ok: good") {
		t.Fatalf("RunCLI output = %q, want it to contain %q", got, "ok: good")
	}
}

// TestRunCLIEOF verifies EOF without /quit returns nil.
func TestRunCLIEOF(t *testing.T) {
	in := strings.NewReader("hello\n")
	var out strings.Builder
	handler := func(ctx context.Context, input string) (string, error) {
		return "echo: " + input, nil
	}
	if err := RunCLI(context.Background(), in, &out, handler); err != nil {
		t.Fatalf("RunCLI on EOF: %v, want nil", err)
	}
	if !strings.Contains(out.String(), "echo: hello") {
		t.Fatalf("RunCLI output = %q, want echo", out.String())
	}
}

// TestCLIChannelRun verifies the CLIChannel wrapper delegates to RunCLI.
func TestCLIChannelRun(t *testing.T) {
	ch := NewCLIChannel(func(ctx context.Context, input string) (string, error) {
		return "via channel: " + input, nil
	})
	var out strings.Builder
	if err := ch.Run(context.Background(), strings.NewReader("hi\n/quit\n"), &out); err != nil {
		t.Fatalf("CLIChannel.Run: %v", err)
	}
	if !strings.Contains(out.String(), "via channel: hi") {
		t.Fatalf("CLIChannel output = %q, want reply", out.String())
	}
}
