// Package messaging provides Orion's messaging channels.
//
// Telegram is the only remote messaging channel. There is deliberately no
// WhatsApp / Baileys support in this package.
//
// The Telegram gateway talks to the Bot API over plain HTTP with no
// external dependencies, so it can be tested against an httptest server
// by overriding the base URL (see WithBaseURL / SetDefaultBaseURL).
package messaging

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/irfan/orion/internal/config"
	"github.com/irfan/orion/internal/core"
	"github.com/irfan/orion/internal/model"
	"github.com/irfan/orion/internal/permissions"
	"github.com/irfan/orion/internal/security"
)

// DefaultBaseURL is the Telegram Bot API endpoint. It is read by
// NewTelegramGateway and may be changed with SetDefaultBaseURL —
// typically by tests pointing gateways at a fake server, or embedders
// that need a proxy. The value lives in an atomic.Value so a
// concurrent SetDefaultBaseURL and NewTelegramGateway can never race;
// production code should leave it alone.
var defaultBaseURL atomic.Value // string

func init() {
	defaultBaseURL.Store("https://api.telegram.org")
}

// DefaultBaseURLValue returns the current default Bot API base URL.
func DefaultBaseURLValue() string {
	if v, ok := defaultBaseURL.Load().(string); ok && strings.TrimSpace(v) != "" {
		return strings.TrimRight(strings.TrimSpace(v), "/")
	}
	return "https://api.telegram.org"
}

// SetDefaultBaseURL changes the default Bot API base URL used by
// gateways created without an explicit WithBaseURL option. It is safe
// for concurrent use with NewTelegramGateway. An empty value resets
// the default to https://api.telegram.org.
func SetDefaultBaseURL(baseURL string) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = "https://api.telegram.org"
	}
	defaultBaseURL.Store(baseURL)
}

// MaxMessageLength is Telegram's per-message text limit. Longer texts are
// split into multiple messages by SendMessage.
const MaxMessageLength = 4096

// defaultHTTPTimeout bounds a single non-poll Bot API request (sendMessage,
// getMe, ...). Telegram answers those in ~1s, so 35s is generous; it is
// NOT the poll deadline — getUpdates has its own client and deadline
// below, because the two wait on different clocks.
const defaultHTTPTimeout = 35 * time.Second

// pollHTTPTimeout bounds one getUpdates long poll: the 30s server window
// plus 15s of grace (talon: 30s window + 15s grace; OpenClaw ships the
// same 45s getUpdates guard, arrived at independently). The old 35s
// deadline left only 5s of slack — the cc-connect flush-timeout trap in
// miniature: a slow-but-healthy response arriving at 31–35s was killed
// by the client and misread as a dead connection. The deadline, not the
// window, is the detection instrument: the window stays at 30s.
const pollHTTPTimeout = 45 * time.Second

// pollRebuildAfterTimeouts is the number of CONSECUTIVE getUpdates
// timeouts after which the poll transport is rebuilt with a fresh dialer.
// NOVEL COMPOSITION — no upstream project ships this N: it is reasoned,
// not upstream-validated. The live incident (2026-11-15 01:28–01:36) ran
// 26 failed attempts at ~35s each before self-recovering on a fresh dial;
// N=3 cuts that black hole by an order of magnitude, and talon's
// auto-retry uses 3 attempts. Three 45s attempts is ~135s of continuous
// failure, far past any transient blip.
const pollRebuildAfterTimeouts = 3

// pollWatchdogInterval is the backstop: if this long passes with no
// completed poll (success or empty result), the poll transport is rebuilt
// even if the consecutive-timeout count did not trip — e.g. attempts
// failing fast. 120s is OpenClaw's shipped watchdog number (default
// 120,000ms); it is 4x the 30s window. Note the ordering with N=3: with
// a 45s per-attempt deadline the N=3 path takes ~135s + backoff
// (~138s), while the watchdog is only checked at the top of the poll
// loop, so its first due opportunity is ~142s — N=3 fires FIRST in a
// pure slow-timeout black hole (live-measured 2m18.2s, reason "3
// consecutive getUpdates timeouts"). The watchdog is the backstop for
// NON-consecutive-timeout stalls: polls that neither complete nor time
// out three times in a row. The rebuild is the same either way, and
// N=3 is what bounds fast-timeout storms (failures that return in
// milliseconds, where 120s of wall clock could hold dozens of
// attempts).
const pollWatchdogInterval = 120 * time.Second

// getUpdatesTimeout is the long-poll timeout requested from Telegram.
const getUpdatesTimeout = 30

// allowedUpdates is the update-type filter Orion subscribes to
// (F-A16-2). Telegram persists allowed_updates server-side per bot:
// the rc8 incident was a stale persisted filter of ["message"],
// which meant Telegram never delivered callback_query and every
// approval button silently died. Sending this filter on the first
// successful getUpdates resets any such stale filter, so the bot
// both receives taps and keeps receiving them. Only "message" and
// "callback_query" are needed — Orion handles nothing else.
var allowedUpdates = []string{"message", "callback_query"}

// TGUser is a Telegram user.
type TGUser struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
}

// TGChat is a Telegram chat.
type TGChat struct {
	ID int64 `json:"id"`
}

// TGPhotoSize is one size variant of a photo attached to a message.
// Telegram sends the same image in several sizes; the largest is
// last. Only FileID matters to Orion — the largest size is
// downloaded (see downloadPhoto) when the chat's model supports
// vision. Width/Height are kept so tests and logs can tell a photo
// from any other attachment.
type TGPhotoSize struct {
	FileID   string `json:"file_id"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	FileSize int    `json:"file_size,omitempty"`
}

// TGSticker is a sticker attached to a message (F-A18-9). Only
// FileID matters to Orion — the sticker bytes are never fetched:
// a sticker is answered with an honest cannot-process reply, never
// downloaded (stickers are not photo input).
type TGSticker struct {
	FileID string `json:"file_id"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

// TGVoice is a voice note attached to a message (F-A18-9). Orion
// has no speech-to-text support; the bytes are never fetched.
type TGVoice struct {
	FileID   string `json:"file_id"`
	Duration int    `json:"duration"`
}

// TGVideo is a video attached to a message (F-A18-9). Orion has
// no video support; the bytes are never fetched.
type TGVideo struct {
	FileID   string `json:"file_id"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	Duration int    `json:"duration"`
}

// TGDocument is a file attached to a message (F-A18-9). Orion
// cannot read files sent here; the bytes are never fetched.
type TGDocument struct {
	FileID   string `json:"file_id"`
	FileName string `json:"file_name,omitempty"`
	MimeType string `json:"mime_type,omitempty"`
}

// TGLocation is a location attached to a message (F-A18-9). Orion
// has no maps support.
type TGLocation struct {
	Longitude float64 `json:"longitude"`
	Latitude  float64 `json:"latitude"`
}

// TGAnimation is an animation (GIF or H.264/MPEG-4 AVC video
// without sound) attached to a message (F-A19-1). Orion cannot
// see it; the bytes are never fetched.
type TGAnimation struct {
	FileID   string `json:"file_id"`
	FileName string `json:"file_name,omitempty"`
	MimeType string `json:"mime_type,omitempty"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	Duration int    `json:"duration"`
}

// TGAudio is an audio file attached to a message (F-A19-1). Orion
// cannot hear it; the bytes are never fetched.
type TGAudio struct {
	FileID    string `json:"file_id"`
	Duration  int    `json:"duration"`
	Performer string `json:"performer,omitempty"`
	Title     string `json:"title,omitempty"`
	MimeType  string `json:"mime_type,omitempty"`
}

// TGVideoNote is a round video message attached to a message
// (F-A19-1). Orion cannot see or hear it; the bytes are never
// fetched.
type TGVideoNote struct {
	FileID   string `json:"file_id"`
	Length   int    `json:"length"`
	Duration int    `json:"duration"`
}

// TGContact is a contact shared in a message (F-A19-1). Orion
// cannot read the contact card.
type TGContact struct {
	PhoneNumber string `json:"phone_number"`
	FirstName   string `json:"first_name"`
	LastName    string `json:"last_name,omitempty"`
	UserID      int64  `json:"user_id,omitempty"`
}

// TGPoll is a poll attached to a message (F-A19-1). Orion cannot
// see the question, options or votes.
type TGPoll struct {
	ID              string `json:"id"`
	Question        string `json:"question"`
	TotalVoterCount int    `json:"total_voter_count"`
	IsClosed        bool   `json:"is_closed"`
}

// TGVenue is a venue shared in a message (F-A19-1). Orion has no
// maps support.
type TGVenue struct {
	Location TGLocation `json:"location"`
	Title    string     `json:"title"`
	Address  string     `json:"address"`
}

// TGDice is a dice roll attached to a message (F-A19-1). Orion
// cannot see the result.
type TGDice struct {
	Emoji string `json:"emoji"`
	Value int    `json:"value"`
}

// TGGame is a game attached to a message (F-A19-1). Orion cannot
// play or see it.
type TGGame struct {
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
}

// TGMessage is a Telegram message.
type TGMessage struct {
	MessageID int64   `json:"message_id"`
	Chat      TGChat  `json:"chat"`
	Text      string  `json:"text"`
	From      *TGUser `json:"from,omitempty"`
	// Photo holds the size variants of an attached photo, if any
	// (F-A17-9). A photo message carries its caption in "caption",
	// not "text", so Text is empty for a photo even with a caption
	// — HasPhoto is the only reliable photo test. Caption is kept
	// (vision fix): it is the user's question about the photo.
	// The largest size (last element) is the one downloaded when
	// the chat's model supports vision.
	Photo []TGPhotoSize `json:"photo,omitempty"`
	// Caption is the caption of a photo message, if any. It is
	// empty for a plain-text message and for a caption-less photo.
	Caption string `json:"caption,omitempty"`
	// Sticker, Voice, Video, Document, Location, Animation, Audio,
	// VideoNote, Contact, Poll, Venue, Dice and Game hold a
	// non-photo attachment, if any (F-A18-9, F-A19-1). Like a photo,
	// the bytes are never fetched — see MediaKind and
	// cannotProcessMediaMessage. A message carries at most one
	// of these in practice; MediaKind reports the first in a
	// fixed order when several are somehow present.
	Sticker   *TGSticker   `json:"sticker,omitempty"`
	Voice     *TGVoice     `json:"voice,omitempty"`
	Video     *TGVideo     `json:"video,omitempty"`
	Document  *TGDocument  `json:"document,omitempty"`
	Location  *TGLocation  `json:"location,omitempty"`
	Animation *TGAnimation `json:"animation,omitempty"`
	Audio     *TGAudio     `json:"audio,omitempty"`
	VideoNote *TGVideoNote `json:"video_note,omitempty"`
	Contact   *TGContact   `json:"contact,omitempty"`
	Poll      *TGPoll      `json:"poll,omitempty"`
	Venue     *TGVenue     `json:"venue,omitempty"`
	Dice      *TGDice      `json:"dice,omitempty"`
	Game      *TGGame      `json:"game,omitempty"`
}

// HasPhoto reports whether the message carries a photo attachment.
func (m *TGMessage) HasPhoto() bool {
	return m != nil && len(m.Photo) > 0
}

// MediaKind returns the kind of non-photo media attached to the
// message ("sticker", "voice", "video", "document", "location",
// "animation", "audio", "video_note", "contact", "poll", "venue",
// "dice" or "game"), or "" when the message carries no non-photo
// media (F-A18-9, F-A19-1). A photo is not media here — HasPhoto
// covers it.
func (m *TGMessage) MediaKind() string {
	if m == nil {
		return ""
	}
	switch {
	case m.Sticker != nil:
		return "sticker"
	case m.Voice != nil:
		return "voice"
	case m.Video != nil:
		return "video"
	case m.Document != nil:
		return "document"
	case m.Location != nil:
		return "location"
	case m.Animation != nil:
		return "animation"
	case m.Audio != nil:
		return "audio"
	case m.VideoNote != nil:
		return "video_note"
	case m.Contact != nil:
		return "contact"
	case m.Poll != nil:
		return "poll"
	case m.Venue != nil:
		return "venue"
	case m.Dice != nil:
		return "dice"
	case m.Game != nil:
		return "game"
	default:
		return ""
	}
}

// HasMedia reports whether the message carries non-photo media.
func (m *TGMessage) HasMedia() bool {
	return m.MediaKind() != ""
}

// cannotProcessMediaMessage is the honest reply sent when a
// non-photo media message arrives (F-A18-9): Orion cannot see,
// hear, watch, read or map the attachment, so it must say so —
// and ask for words — instead of silently dropping the message
// or running a turn on an attachment the model never received.
// kind is one of MediaKind's values; anything else gets the
// generic form.
func cannotProcessMediaMessage(kind string) string {
	switch kind {
	case "sticker":
		return "😀 I can't process stickers yet — Orion can't interpret sticker images, so please describe what you meant in words and I'll help from that."
	case "voice":
		return "🎙️ I can't process voice notes yet — Orion has no speech-to-text support, so please type your message in words and I'll help from that."
	case "video":
		return "🎬 I can't process videos yet — Orion has no video support, so I can't watch video clips sent here. Please describe what you need in words and I'll help from that."
	case "document":
		return "📄 I can't process documents yet — Orion can't read files sent here. Please paste the text you need help with and I'll help from that."
	case "location":
		return "📍 I can't process locations yet — Orion has no maps support, so please type the place name or address in words and I'll help from that."
	case "animation":
		return "🎞️ I can't process animations yet — Orion can't see GIFs or video clips sent here, so please describe what's in it in words and I'll help from that."
	case "audio":
		return "🎵 I can't process audio files yet — Orion has no speech-to-text support, so please type what's in the audio in words and I'll help from that."
	case "video_note":
		return "🎥 I can't process video messages yet — Orion can't see or hear round video messages, so please describe what's in it in words and I'll help from that."
	case "contact":
		return "👤 I can't process shared contacts yet — Orion can't read contact cards, so please type the name and number in words and I'll help from that."
	case "poll":
		return "📊 I can't process polls yet — Orion can't see the question, options or votes, so please type the question and options in words and I'll help from that."
	case "venue":
		return "🏢 I can't process shared venues yet — Orion has no maps support, so please type the venue name and address in words and I'll help from that."
	case "dice":
		return "🎲 I can't process dice rolls yet — Orion can't see the result, so please type the result in words and I'll help from that."
	case "game":
		return "🎮 I can't process games yet — Orion can't play or see games sent here, so please describe what you need in words and I'll help from that."
	default:
		return "I can't process that kind of message yet — please send it in words and I'll help from that."
	}
}

// CannotSeeImagesMessage is the honest reply sent when a photo
// arrives but the chat's provider/model cannot see images
// (F-A17-9), and when a provider rejects image input (F-A17-10).
// It is in Bengali — the user's language — and it names the fix:
// DeepSeek's deepseek-flash has vision, so switching the model
// enables it. It never claims the photo was seen, and never asks
// the user to re-send what the bot cannot process.
const CannotSeeImagesMessage = "📷 আমি এই ছবিটি দেখতে পারছি না — এই চ্যাটের মডেলের ভিশন সাপোর্ট নেই। মডেলটি DeepSeek `deepseek-flash`-এ বদলালে আমি ছবি দেখতে পারব। আপাতত ছবিটি কথায় বর্ণনা করলে আমি সেখান থেকে সাহায্য করতে পারব।"

// PhotoTooLargeMessage is the honest reply when a photo exceeds
// MaxPhotoBytes: the bot refuses the download instead of sending a
// multi-megabyte base64 blob to the provider.
const PhotoTooLargeMessage = "📷 ছবিটি অনেক বড় — আমি ১০ MiB পর্যন্ত ছবি নিতে পারি। ছোট করে পাঠালে দেখতে পারব।"

// PhotoDownloadFailedMessage is the honest reply when a photo
// could not be downloaded from Telegram (network error, expired
// file_id, ...): the bot says so instead of running a turn on a
// caption about an image it never received.
const PhotoDownloadFailedMessage = "📷 ছবিটি ডাউনলোড করা যায়নি, তাই দেখতে পারছি না। আবার পাঠিয়ে দেখুন, অথবা ছবিটি কথায় বর্ণনা করুন।"

// PhotoDefaultPrompt is the user text for a photo sent with no
// caption: the model is asked to describe the image.
const PhotoDefaultPrompt = "এই ছবিটা বর্ণনা করো।"

// MaxPhotoBytes caps a downloaded Telegram photo (vision fix).
// Telegram photos are small (a 1280px photo is typically well
// under a few MiB) and DeepSeek allows 32 MiB per image, but the
// request body caps at 48 MiB and base64 inflates bytes by ~33%,
// so 10 MiB keeps a photo turn comfortably inside the request
// budget even with history attached.
const MaxPhotoBytes = 10 << 20

// TGCallbackQuery is a Telegram callback query: a tap on an inline
// keyboard button. Message is the message the button belongs to
// (nil for buttons on messages the bot can no longer see); Data is
// the button's callback_data, which the bot defined when it sent
// the keyboard.
type TGCallbackQuery struct {
	ID      string     `json:"id"`
	From    *TGUser    `json:"from"`
	Message *TGMessage `json:"message,omitempty"`
	Data    string     `json:"data"`
}

// Update is a single Telegram update.
type Update struct {
	UpdateID      int64            `json:"update_id"`
	Message       *TGMessage       `json:"message,omitempty"`
	CallbackQuery *TGCallbackQuery `json:"callback_query,omitempty"`
}

// InlineKeyboardButton is one button in an inline keyboard. Only
// Text and CallbackData are used: tapping the button delivers a
// callback query carrying CallbackData.
type InlineKeyboardButton struct {
	Text         string `json:"text"`
	CallbackData string `json:"callback_data"`
}

// InlineKeyboardMarkup is Telegram's reply_markup for inline
// keyboards: rows of buttons.
type InlineKeyboardMarkup struct {
	InlineKeyboard [][]InlineKeyboardButton `json:"inline_keyboard"`
}

// CallbackHandler handles a callback query (an inline-keyboard tap).
// It is invoked by PollStream outside any per-chat lock — a tap
// must be able to resolve a pending approval while the turn that
// requested it is still blocked waiting for the answer, and the
// per-chat lock is held by that turn. A handler error is logged, it
// never kills polling.
type CallbackHandler func(ctx context.Context, q TGCallbackQuery) error

// TelegramGateway is a client for the Telegram Bot API.
//
// It is safe for concurrent use: the getUpdates offset is guarded by a
// mutex. A gateway is created with NewTelegramGateway; the token is never
// logged.
//
// Sender authorisation: when an allowlist is set (WithAllowedChats),
// Poll drops messages from any other chat before the handler runs.
// Anyone who discovers the bot username could otherwise drive the OS
// agent, including its shell tool.
//
// The allowlist is ALSO the sender allowlist (P1-O4): a message is
// accepted only when its SENDER's user ID is allowlisted, not merely
// when its chat is. In a private chat the chat ID equals the user
// ID, so existing single-user configs behave exactly as before. In
// a group chat, only allowlisted members may drive the bot — other
// members' messages are dropped even though the group itself is
// allowlisted. A message with no sender (channel posts, anonymous
// admins) is dropped: there is nobody to authorise. See
// IsSenderAllowed; IsChatAllowed is unchanged (another worker owns
// it) and still runs first.
type TelegramGateway struct {
	token   string
	baseURL string
	client  *http.Client
	// pollClient is getUpdates' OWN http.Client, separate from the
	// send/message client (spool, PTB): the poll waits 30s by design
	// while every other call should answer in ~1s, so sharing one
	// pool lets a dead poll connection poison sends and vice versa.
	// Its transport is pinned to HTTP/1.1 (see newPollTransport):
	// over HTTP/2 every call multiplexes onto one shared connection
	// that stays pooled after a timeout, while over HTTP/1.1 a
	// timed-out call closes its connection and the next attempt
	// dials fresh. Only call() routes getUpdates here; every other
	// method uses client.
	pollClient *http.Client
	// pollTimeout is the per-attempt getUpdates deadline (default
	// pollHTTPTimeout). A field, not a constant read, so tests can
	// shrink it without waiting out a real 45s deadline.
	pollTimeout time.Duration
	// pollWatchdogInterval is this gateway's watchdog (default
	// pollWatchdogInterval). A field so tests can shrink it.
	pollWatchdogInterval time.Duration
	// pollStateMu guards the poll-health fields below. Lock order:
	// fetchMu (outer) -> pollStateMu (inner); never the reverse.
	// PollHealth takes only pollStateMu.
	pollStateMu sync.Mutex
	// lastPollCompletedAt is when a getUpdates fetch last succeeded
	// (empty result counts). Zero until the first success.
	lastPollCompletedAt time.Time
	// pollLoopStartedAt is when PollStream started (zero when it
	// never ran). It is the watchdog baseline before the first
	// completed poll.
	pollLoopStartedAt time.Time
	// lastPollRebuildAt is when the poll transport was last rebuilt.
	// It also resets the watchdog baseline: a rebuild is the
	// recovery action, so the watchdog must not fire again
	// immediately after one.
	lastPollRebuildAt time.Time
	// pollRebuilds counts transport rebuilds over this gateway's life.
	pollRebuilds int
	// pollConsecutiveTimeouts counts back-to-back getUpdates
	// TIMEOUT-class failures (see isTimeoutError). Any success — or
	// any non-timeout transient error, which proves the connection
	// answered — resets it. It is the N in pollRebuildAfterTimeouts.
	pollConsecutiveTimeouts int
	// pollInBackoff is true while PollStream sleeps between attempts
	// after a transient failure. pollRebuilding is true while a
	// rebuild is swapping the transport. Both feed PollHealth: a
	// poller in either state must NOT report healthy.
	pollInBackoff  bool
	pollRebuilding bool
	offset         int64
	mu             sync.Mutex
	// updatesFilterSent records whether a getUpdates call has
	// already carried allowedUpdates (F-A16-2). It is set only
	// after a fetch SUCCEEDS: a failed fetch may never have
	// reached Telegram, so the next fetch retries the filter
	// instead of assuming it landed. It is read and written only
	// inside fetchUpdatesLocked, whose callers all hold fetchMu,
	// so no separate lock is needed. One gateway sends the filter
	// once per lifetime — a gateway is built once per daemon
	// start, so that is once per startup.
	updatesFilterSent bool
	// fetchMu serialises getUpdates fetches. The offset alone is not
	// enough: two concurrent fetches could both read the same
	// offset before either advanced it, and Telegram would return
	// the same updates to both. One poller at a time also matches
	// the Bot API's own rule (409 Conflict when a second
	// getUpdates is in flight). GetUpdates holds it across the
	// fetch and the offset advance (one atomic step); PollStream
	// holds it only for the fetch and confirms per dispatched
	// update afterwards (F-A7-P2-3) — production runs a single
	// poller, so no second fetcher can observe the un-advanced
	// offset mid-batch.
	fetchMu sync.Mutex
	// allowedChats is nil when no allowlist is set (all chats pass).
	allowedChats map[int64]bool
	// allowAll is set by WithAllowAll when the operator explicitly
	// opted into allowing every chat (P2-O5). With no allowlist the
	// opt-in allows all chats — but only inside allowAllWindow from
	// allowAllSince (daemon start); IsChatAllowed is the only
	// enforcement point. allowAllExpiredLogged makes the expiry log
	// line fire once, not on every dropped message.
	// allowAllStatePath, when set by WithAllowAllPersisted, is the
	// data-dir file holding the window's deadline (F-A17-4): the
	// deadline survives restarts, so a restart resumes the recorded
	// window instead of opening a fresh one.
	allowAll              bool
	allowAllSince         time.Time
	allowAllWindow        time.Duration
	allowAllExpiredLogged bool
	allowAllStatePath     string
	// callbackHandler, when set via SetCallbackHandler, receives
	// callback-query updates (inline-keyboard taps). It is invoked
	// outside any per-chat lock (see CallbackHandler).
	callbackHandler CallbackHandler
	// modelInfo, when set via WithModelInfo, reports the chat's
	// current (provider, model) for vision decisions: a photo is
	// downloaded and run as a turn only when SupportsVision says
	// that pair can see images. Nil (the default, e.g. in tests
	// that never set it) means "unknown": vision is treated as
	// unsupported and the photo gets the honest Bengali
	// cannot-see reply — fail closed, never pretend.
	modelInfo func(chatID int64) (provider, model string)
	// router, when installed via InstallCallbackRouter, dispatches
	// taps by callback_data prefix to the tool that owns the
	// prefix (framework-registered tools) and falls back to the
	// approval handler for anything else. It is the installed
	// callbackHandler's implementation; created on demand.
	router *callbackRouter
	// callbackRouterInstalled records that InstallCallbackRouter
	// installed the router dispatch (F-A20-6): a second install
	// is a no-op instead of a silent replacement, and a
	// pre-installed different handler fails loudly.
	callbackRouterInstalled bool
	// replyChain holds the typed-reply interceptors PollStream
	// consults before the per-chat lock (a reply that answers a
	// pending question must not wait on the lock the asking turn
	// holds). Created on demand.
	replyChain *replyChain
	// questions is the pending-question registry installed by
	// SetQuestionRegistry (nil when none): the busy-input
	// interrupt path cancels a pending question through it, so
	// an interrupted run never leaves a live-looking question
	// card behind. Guarded by mu, like the other installed
	// hooks below.
	questions *QuestionRegistry
	// busyModeHook, when set via SetBusyModeFor, reads a chat's
	// busy-input mode (nil = interrupt for every chat).
	// planPendingHook, when set via SetPlanPendingFor, reports
	// whether a plan decision is pending for a chat (nil =
	// never pending). steerHook, when set via SetSteerFor,
	// offers a message to a chat's running turn (nil = never
	// accepted). steerLeftoversHook, when set via
	// SetSteerLeftoversFor, collects a chat's unconsumed
	// steered messages after a turn returns (nil = none).
	// All four are installed before PollStream starts and never
	// race polling; each accessor copies the hook under mu and
	// calls it without holding mu. Guarded by mu.
	busyModeHook       func(chatID int64) BusyMode
	planPendingHook    func(chatID int64) bool
	steerHook          func(chatID int64, text string, updateID int64) (accepted bool)
	steerLeftoversHook func(chatID int64) []core.SteeredMessage
	// busy tracks the running turn's busy state per chat (see
	// busy.go): registered after the per-chat lock, removed
	// when the turn's handler finishes. Guarded by busyMu.
	busyMu sync.Mutex
	busy   map[int64]*busyState
	// busyStamps snapshots, at dispatch time, which busy state
	// an update arrived during and which FIFO ticket orders its
	// turn (see stampDispatch): the poll loop dispatches in
	// batch order, but a handler goroutine can start late —
	// after the run it arrived during ended and the next turn
	// registered — and a lookup at handler time would then ack
	// against the wrong (fresh) state, producing a spurious
	// second ack, or take the per-chat lock ahead of an earlier
	// update. The stamp pins the arrival-time state and the
	// dispatch order; the ack is still coalesced per state (see
	// sendBusyAck). Keyed by update id (never reused); entries
	// are deleted when the handler reads them, and a dispatch
	// that never spawns a handler (ctx cancelled at the
	// semaphore) leaves at most one stale entry behind.
	// Guarded by stampMu.
	stampMu    sync.Mutex
	busyStamps map[int64]dispatchStamp
	// tickets holds one FIFO ticket queue per chat (see
	// ticketQueue in busy.go): queued text turns run in
	// dispatch order, not in goroutine-scheduling order.
	// Guarded by ticketMu.
	ticketMu sync.Mutex
	tickets  map[int64]*ticketQueue
	// logger, when set via WithLogger, receives polling-health notes:
	// transient getUpdates failures and the recovery after them. It is
	// nil by default (silent). Only redacted errors are ever passed
	// to it — call() wraps every token-bearing error — so the token
	// can never appear in a log line. Fatal poll errors are NOT
	// logged here; PollStream returns them to the caller.
	logger func(format string, args ...any)
	// mediaCfg holds the resolved media-delivery config (see
	// WithMediaDelivery in media.go). When Enabled is false —
	// including when no option was ever passed — deliverMedia is a
	// no-op and MEDIA: tags stay as visible text.
	mediaCfg config.MediaDeliveryConfig
	// mediaAllow holds the resolved allowlist directories (see
	// resolveMediaAllowDirs). Empty means no allowlist: a file may
	// be sent from anywhere after passing the denylist. A
	// non-empty list restricts delivery to those dirs.
	mediaAllow []string
}

// Gateway is an alias for TelegramGateway.
type Gateway = TelegramGateway

// GatewayOption configures a TelegramGateway at construction time.
type GatewayOption func(*TelegramGateway)

// TelegramGatewayOption is an alias for GatewayOption.
type TelegramGatewayOption = GatewayOption

// WithBaseURL overrides the Bot API base URL (default
// https://api.telegram.org). Primarily for tests pointing at a fake
// Telegram server. A trailing slash is stripped.
func WithBaseURL(baseURL string) GatewayOption {
	return func(g *TelegramGateway) {
		if strings.TrimSpace(baseURL) != "" {
			g.baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
		}
	}
}

// WithHTTPClient overrides the HTTP client used for non-poll Bot API
// requests (sendMessage, getMe, ...). If nil, the option is ignored and
// the default client is kept. It does NOT touch the getUpdates poll
// client: the poll has its own client with its own HTTP/1.1-only
// transport and 45s deadline (see pollClient), and sharing one client
// for both is exactly the pooled-connection failure this split fixes.
func WithHTTPClient(client *http.Client) GatewayOption {
	return func(g *TelegramGateway) {
		if client != nil {
			g.client = client
		}
	}
}

// WithLogger sets the polling-health logger: transient getUpdates
// failures (with the backoff about to be waited) and the recovery
// once polling succeeds again. A nil logger is ignored and the
// gateway stays silent. The logger must never receive an unredacted
// error; PollStream only passes it errors that call() already
// redacted, so the bot token can never leak into a log line.
func WithLogger(logf func(format string, args ...any)) GatewayOption {
	return func(g *TelegramGateway) {
		if logf != nil {
			g.logger = logf
		}
	}
}

// WithModelInfo installs the per-chat (provider, model) lookup the
// gateway uses for vision decisions (see modelInfo): a photo is
// downloaded and run as a turn only when SupportsVision says that
// pair can see images. The daemon wires this to
// App.ModelInfoForChat, so a /model switch in one chat decides
// vision for that chat only. A nil lookup is ignored and the
// gateway keeps treating vision as unsupported.
func WithModelInfo(fn func(chatID int64) (provider, model string)) GatewayOption {
	return func(g *TelegramGateway) {
		if fn != nil {
			g.modelInfo = fn
		}
	}
}

// logf reports a polling-health note when a logger is set; it is a
// no-op otherwise.
func (g *TelegramGateway) logf(format string, args ...any) {
	if g == nil || g.logger == nil {
		return
	}
	g.logger(format, args...)
}

// logReceivedUpdate records one update GetUpdates returned: its id,
// its kind, and the sender/chat ids. Message text and callback data
// are NEVER logged (the text is user/model content; the data holds a
// pending-request id). The line exists so a lost tap is
// diagnosable: if getUpdates returned the callback query, this line
// is in the log and the loss is downstream of the poll; if it is
// absent, the update never reached this poller at all (another
// getUpdates consumer confirmed it server-side, or Telegram never
// delivered it) — no code downstream can fix that, and guessing
// otherwise wastes the next incident.
func (g *TelegramGateway) logReceivedUpdate(upd Update) {
	switch {
	case upd.CallbackQuery != nil:
		from := int64(0)
		if upd.CallbackQuery.From != nil {
			from = upd.CallbackQuery.From.ID
		}
		g.logf("telegram poll: received update %d callback_query id=%s from=%d", upd.UpdateID, upd.CallbackQuery.ID, from)
	case upd.Message != nil:
		from := int64(0)
		if upd.Message.From != nil {
			from = upd.Message.From.ID
		}
		g.logf("telegram poll: received update %d message chat=%d from=%d", upd.UpdateID, upd.Message.Chat.ID, from)
	default:
		g.logf("telegram poll: received update %d (other type, skipped)", upd.UpdateID)
	}
}

// WithAllowedChats restricts Poll to messages from the given chat IDs
// (private chats are positive, groups negative). With no IDs the option
// is a no-op; with at least one, every other chat is dropped before the
// handler runs. Use config.TelegramAllowedChats to drive this.
func WithAllowedChats(ids ...int64) GatewayOption {
	return func(g *TelegramGateway) {
		if len(ids) == 0 {
			return
		}
		if g.allowedChats == nil {
			g.allowedChats = map[int64]bool{}
		}
		for _, id := range ids {
			if id != 0 {
				g.allowedChats[id] = true
			}
		}
	}
}

// DefaultAllowAllWindow is the allow-all opt-in window when the
// caller passes no (or a non-positive) window to WithAllowAll: 10
// minutes from gateway creation. It mirrors
// config.DefaultAllowAllTelegramWindowMinutes; keep the two in sync.
const DefaultAllowAllWindow = 10 * time.Minute

// WithAllowAll marks the gateway as running under the explicit
// allow-all opt-in (P2-O5): with no allowlist, IsChatAllowed allows
// every chat — but only for window from gateway creation (daemon
// start). Once the window elapses IsChatAllowed refuses every chat
// until the daemon restarts with a real allowlist. A non-positive
// window means DefaultAllowAllWindow. When an allowlist is also set
// (WithAllowedChats), the allowlist wins: only listed chats pass,
// and the window is irrelevant. The window is measured from
// creation, not from first use, so an idle daemon still closes.
//
// Prefer WithAllowAllPersisted for the daemon: it records the window
// deadline in the data dir, so a restart resumes the same window
// instead of opening a fresh one (F-A17-4). This in-memory form
// stays for tests and hand-built gateways.
func WithAllowAll(window time.Duration) GatewayOption {
	return func(g *TelegramGateway) {
		if window <= 0 {
			window = DefaultAllowAllWindow
		}
		g.allowAll = true
		g.allowAllSince = time.Now()
		g.allowAllWindow = window
	}
}

// AllowAllStateFileName is the file in the data dir that records the
// allow-all opt-in window's deadline (F-A17-4). One line, the
// deadline in RFC3339Nano, written atomically (0600) when the window
// first opens. The file is the window's memory across restarts: a
// daemon that restarts resumes the recorded deadline instead of
// opening a fresh window.
const AllowAllStateFileName = "allow-all-until"

// WithAllowAllPersisted is WithAllowAll with the window deadline
// persisted in the data dir (F-A17-4). The first start with the
// opt-in opens a fresh window and writes its deadline to
// <dataDir>/allow-all-until before polling starts; every later
// start reads that file back:
//
//   - deadline still in the future: the window resumes with the
//     recorded deadline — a restart neither extends it nor reopens
//     it. The recorded deadline wins over a later change to the
//     configured window length; delete the file to start a fresh
//     window.
//   - deadline already past: the window stays shut. The file is
//     NOT rewritten, so a restart, a crash-loop, or a reboot can
//     never reopen the bot to every chat — the F-A17-4 bug, where
//     each restart silently granted a fresh 10 minutes.
//   - file missing: first start, fresh window (as above).
//   - file unreadable or corrupt, or unreadable for any other
//     reason: fail closed — the window stays shut and the log names
//     the file, so the operator can delete it to start over.
//     A torn write cannot produce this: the write is atomic (temp
//     file + fsync + rename), so a corrupt file means tampering or
//     disk trouble, never a half-written deadline.
//
// With an empty dataDir this behaves exactly like WithAllowAll
// (in-memory window only), for hand-built gateways and tests. When
// an allowlist is also set, the allowlist wins and the window is
// irrelevant, exactly as with WithAllowAll.
func WithAllowAllPersisted(window time.Duration, dataDir string) GatewayOption {
	return func(g *TelegramGateway) {
		if window <= 0 {
			window = DefaultAllowAllWindow
		}
		g.allowAll = true
		g.allowAllWindow = window
		if strings.TrimSpace(dataDir) == "" {
			g.allowAllSince = time.Now()
			return
		}
		path := filepath.Join(dataDir, AllowAllStateFileName)
		g.allowAllStatePath = path
		now := time.Now()
		raw, err := os.ReadFile(path)
		switch {
		case err == nil:
			deadline, perr := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(raw)))
			if perr != nil {
				// Corrupt state: fail closed (since is set
				// already past the window) instead of silently
				// opening a fresh one.
				g.logf("telegram allow-all state %q is unreadable (%v); refusing all chats until telegram_allowed_chats is set (delete the file to start a fresh window)", path, perr)
				g.allowAllSince = now.Add(-window)
				return
			}
			// Resume the recorded window: the deadline is
			// absolute, so a restart neither extends it nor
			// reopens it once it has passed.
			g.allowAllSince = deadline.Add(-window)
		case os.IsNotExist(err):
			// First start with the opt-in: open a fresh window
			// and persist its deadline before polling starts.
			g.allowAllSince = now
			if err := persistAllowAllDeadline(path, now.Add(window)); err != nil {
				// Best effort: a failed persist must not stop
				// the daemon, but it is logged loudly — without
				// the file the window WILL reopen on restart,
				// which is exactly the F-A17-4 bug.
				g.logf("telegram allow-all window could not be persisted (%v); it will reopen on restart", err)
			}
		default:
			// The file exists but cannot be read: fail closed,
			// like the corrupt case.
			g.logf("telegram allow-all state %q could not be read (%v); refusing all chats until telegram_allowed_chats is set", path, err)
			g.allowAllSince = now.Add(-window)
		}
	}
}

// persistAllowAllDeadline atomically writes the allow-all deadline
// (RFC3339Nano, one line, 0600): the same temp-file + fsync +
// rename + dir-sync idiom as the session and permission stores, so
// a crash mid-write can never leave a torn file behind.
func persistAllowAllDeadline(path string, deadline time.Time) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("messaging: create allow-all state dir %q: %w", dir, err)
	}
	data := []byte(deadline.Format(time.RFC3339Nano) + "\n")
	tmp, err := os.CreateTemp(dir, ".allow-all-*.tmp")
	if err != nil {
		return fmt.Errorf("messaging: create temp allow-all state file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("messaging: chmod temp allow-all state file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("messaging: write temp allow-all state file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("messaging: sync temp allow-all state file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("messaging: close temp allow-all state file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("messaging: rename temp allow-all state file: %w", err)
	}
	if err := syncSessionDir(dir); err != nil {
		return fmt.Errorf("messaging: sync allow-all state dir %q: %w", dir, err)
	}
	return nil
}

// IsChatAllowed reports whether chatID may drive the agent through this
// gateway. With no allowlist set, every chat is allowed. That is the
// per-chat enforcement only: the daemon entry points refuse to start
// polling with an empty allowlist at all (see CheckTelegramAllowlist),
// so a gateway with no allowlist should only ever exist in tests or
// behind an explicit allow-all opt-in.
//
// TIME-BOX (P2-O5): the allow-all opt-in (WithAllowAll) is enforced
// here and nowhere else. Inside the window every chat passes; once
// it elapses every chat is refused, including chats that were
// allowed a moment earlier. The expiry is logged once (via the
// gateway logger, when set) so the daemon log shows why the bot
// went silent instead of failing silently.
func (g *TelegramGateway) IsChatAllowed(chatID int64) bool {
	if len(g.allowedChats) > 0 {
		return g.allowedChats[chatID]
	}
	if !g.allowAll {
		return true
	}
	if g.allowAllSince.IsZero() {
		// Hand-built gateway with no start time recorded: there is
		// no window to measure, so nothing to expire. Gateways
		// built by NewTelegramGateway always set the start time
		// through WithAllowAll.
		return true
	}
	window := g.allowAllWindow
	if window <= 0 {
		window = DefaultAllowAllWindow
	}
	if time.Since(g.allowAllSince) < window {
		return true
	}
	g.mu.Lock()
	logged := g.allowAllExpiredLogged
	g.allowAllExpiredLogged = true
	g.mu.Unlock()
	if !logged {
		g.logf("telegram allow-all opt-in expired after %s: refusing all chats until telegram_allowed_chats is set (a restart does not reopen the window)", window)
	}
	return false
}

// IsSenderAllowed reports whether the Telegram USER who sent a
// message may drive the agent (P1-O4). The check is on the sender,
// not the chat: the allowlist holds user IDs (in a private chat the
// user ID equals the chat ID, so existing configs are unaffected),
// and a group chat's allowlisting must never authorise every member
// of the group. A nil sender (channel posts, anonymous admins) is
// never allowed — there is nobody to authorise.
//
// When no allowlist is set, IsChatAllowed's own rule decides: a
// gateway with no allowlist and no allow-all opt-in allows
// everyone (tests and the explicit opt-in path); with the allow-all
// opt-in, the window still applies. A nil sender is refused even
// then.
func (g *TelegramGateway) IsSenderAllowed(from *TGUser) bool {
	if from == nil || from.ID == 0 {
		return false
	}
	if len(g.allowedChats) > 0 {
		return g.allowedChats[from.ID]
	}
	// No allowlist: mirror the chat decision (allow-all window
	// included) so the two checks can never disagree about an
	// open gateway.
	return g.IsChatAllowed(from.ID)
}

// OwnerIDFromAllowlist derives the owner user ID from the Telegram
// allowlist: the FIRST entry is the owner. The owner is the
// operator who installed the bot; their private chat ID is their
// user ID, and the allowlist is ordered with the owner first by
// convention (the daemon logs the derivation at startup so a
// mis-ordered list is visible). There is deliberately no config
// field for the owner: the allowlist already names them, and a
// second source of truth would drift. An empty allowlist has no
// owner (0): daemon-wide operations are then refused for everyone
// until an allowlist exists.
func OwnerIDFromAllowlist(allowedChats []int64) int64 {
	for _, id := range allowedChats {
		if id != 0 {
			return id
		}
	}
	return 0
}

// SetCallbackHandler installs the handler for callback-query
// updates (inline-keyboard taps). A nil handler removes it (taps
// are then answered with "not handled" and dropped). Replacing an
// installed handler is a loud error (F-A20-6): a second installer
// would silently re-route every tap. Safe for concurrent use with
// PollStream.
func (g *TelegramGateway) SetCallbackHandler(h CallbackHandler) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if h != nil && g.callbackHandler != nil {
		return fmt.Errorf("messaging: SetCallbackHandler: a callback handler is already installed: refusing to replace it")
	}
	g.callbackHandler = h
	return nil
}

// SetQuestionRegistry installs the pending-question registry the
// gateway consults for ask_user_question: the reply chain's
// question interceptor answers a pending question from a typed
// reply before the per-chat lock, and the callback router routes
// ask: taps through it. A nil registry means "no questions" (the
// interceptor is removed). Call before PollStream starts; it must
// not race polling.
func (g *TelegramGateway) SetQuestionRegistry(q *QuestionRegistry) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.questions = q
	// The registry edits cards through the gateway (see
	// QuestionRegistry.CancelForChat): give it the pointer here,
	// at the same install point, so the two can never drift.
	if q != nil {
		q.mu.Lock()
		q.gw = g
		q.mu.Unlock()
	}
	chain := g.replyChainForLocked()
	if q == nil {
		chain.setQuestion(nil)
		return
	}
	chain.setQuestion(&questionReplyInterceptor{gw: g, questions: q})
}

// callbackHandlerFor returns the installed callback handler, or nil.
func (g *TelegramGateway) callbackHandlerFor() CallbackHandler {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.callbackHandler
}

// senderIDKey is the context key for the Telegram sender user ID of
// the update being handled.
type senderIDKey struct{}

// WithSenderID returns ctx carrying the Telegram sender user ID
// for the update being handled. PollStream sets it before invoking
// the turn handler, so the turn (and the permission gate, via the
// approval context) knows who asked, not just which chat.
func WithSenderID(ctx context.Context, senderID int64) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, senderIDKey{}, senderID)
}

// SenderIDFromContext reads the sender user ID carried by ctx. ok
// is false when ctx carries none.
func SenderIDFromContext(ctx context.Context) (int64, bool) {
	if ctx == nil {
		return 0, false
	}
	id, ok := ctx.Value(senderIDKey{}).(int64)
	return id, ok
}

// CheckTelegramAllowlist enforces the daemon startup rule for the
// Telegram allowlist: polling may start only when at least one chat
// is allowlisted, or when allowAll was explicitly opted into. An
// empty allowlist with allowAll false is an error, because with no
// allowlist any Telegram chat could drive the agent (including
// shell_exec). The error tells the operator how to find their chat
// ID and how to fix the config; every polling entry point
// (oriond, `orion telegram poll`, `orion daemon`) reports it and
// exits non-zero.
func CheckTelegramAllowlist(allowedChats []int64, allowAll bool) error {
	if len(allowedChats) > 0 || allowAll {
		return nil
	}
	return errors.New("refusing to start Telegram polling: telegram_allowed_chats is empty, so ANY Telegram chat could drive the agent (including shell_exec); set telegram_allowed_chats in config.json to your chat ID(s), or set allow_all_telegram_chats=true only if you really mean to allow every chat — to find your chat ID, start the daemon once with allow_all_telegram_chats=true and send /whoami to the bot, or check the daemon log for the dropped-chat ID logged when a non-allowlisted chat messages the bot while it is polling")
}

// NewTelegramGateway creates a TelegramGateway for the given bot token.
// An empty (or whitespace-only) token is an error.
func NewTelegramGateway(token string, opts ...GatewayOption) (*TelegramGateway, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, errors.New("messaging: telegram token must not be empty")
	}
	baseURL := DefaultBaseURLValue()
	g := &TelegramGateway{
		token:                token,
		baseURL:              baseURL,
		client:               &http.Client{Timeout: defaultHTTPTimeout},
		pollClient:           newPollHTTPClient(),
		pollTimeout:          pollHTTPTimeout,
		pollWatchdogInterval: pollWatchdogInterval,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(g)
		}
	}
	if g.client == nil {
		g.client = &http.Client{Timeout: defaultHTTPTimeout}
	}
	if g.pollClient == nil {
		g.pollClient = newPollHTTPClient()
	}
	if g.pollTimeout <= 0 {
		g.pollTimeout = pollHTTPTimeout
	}
	if g.pollWatchdogInterval <= 0 {
		g.pollWatchdogInterval = pollWatchdogInterval
	}
	if g.baseURL == "" {
		g.baseURL = "https://api.telegram.org"
	}
	return g, nil
}

// Offset returns the next update_id GetUpdates will request. It is
// max(seen update_id) + 1, or 0 before any updates have been seen.
func (g *TelegramGateway) Offset() int64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.offset
}

// --- getUpdates poll recovery (live incident 2026-11-15 01:28–01:36) ---
//
// The incident: 26 failed getUpdates attempts ("context deadline
// exceeded", bytes stuck in the socket send queue), then self-recovery
// on a fresh dial — ~15 minutes of black hole on one pooled connection
// the transport kept reusing. The pieces below make that recovery
// deterministic instead of accidental. Provenance (open-source research,
// fixbatch-research/OPEN-SOURCE-RESEARCH.md §2, workers/W2): split
// clients + HTTP/1.1 pin from spool (commit cf1c03e, issue #559: 164
// timeouts x 70s into the same dead connection); per-attempt deadline
// window+grace from talon (commit f6f4d3a: 30s + 15s = 45s, 312 bytes
// unacked) and OpenClaw's 45s guard; the 120s watchdog from OpenClaw.
// The N=3 rebuild threshold is a NOVEL COMPOSITION of those parts —
// see pollRebuildAfterTimeouts — not a shipped upstream behaviour.

// newPollTransport clones the default transport and pins it to HTTP/1.1
// only. Over HTTP/2 all calls multiplex onto one shared connection that
// stays pooled after a timeout; over HTTP/1.1 a timed-out call closes
// its connection, so the next attempt dials fresh. Everything else
// (dial timeout, keep-alive, idle pool, TLS) is the default
// transport's: only the protocol set changes.
//
// The pin needs TWO mechanisms, because ForceAttemptHTTP2 = false alone
// does NOT pin anything (regression 2026-11-15, three live deploys of
// b6aaa62: every getUpdates failed with `malformed HTTP response
// "\x00\x00\x12\x04…"`):
//
//   - TLSClientConfig.NextProtos = ["http/1.1"]: Transport.Clone()
//     copies the source's TLSClientConfig — whose NextProtos is
//     ["h2", "http/1.1"] once the default transport's lazy HTTP/2
//     setup has fired — but does NOT copy its TLSNextProto["h2"]
//     handler. A clone with only ForceAttemptHTTP2 = false therefore
//     still OFFERS h2 in ALPN while having no HTTP/2 implementation;
//     a server that selects h2 sends HTTP/2 SETTINGS frames that the
//     HTTP/1.1 reader rejects as a malformed response
//     (golang/go#39302). The config is cloned before editing so the
//     shared http.DefaultTransport config is never mutated.
//   - TLSNextProto = non-nil empty map: the stdlib's documented way
//     to disable HTTP/2 on a Transport (protocols() only enables h2
//     when TLSNextProto["h2"] is set), so the clone can never
//     re-acquire an h2 implementation on first use either.
//
// Do NOT "simplify" this to Protocols.SetHTTP1(true): that was
// measured to fail (it leaves the ALPN offer at ["h2", "http/1.1"]).
func newPollTransport() *http.Transport {
	if dt, ok := http.DefaultTransport.(*http.Transport); ok && dt != nil {
		tr := dt.Clone()
		tr.ForceAttemptHTTP2 = false
		// HTTP/2 off at the implementation level: a non-nil (empty)
		// TLSNextProto map stops the transport ever configuring h2.
		tr.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
		// HTTP/2 off at the negotiation level: stop offering h2 in
		// ALPN. Clone the config before editing it, so the shared
		// default transport's config is never mutated — Clone()
		// copies it in this Go version, but the re-clone keeps the
		// pin correct even if a future Clone() ever shares it.
		if tr.TLSClientConfig != nil {
			cfg := tr.TLSClientConfig.Clone()
			cfg.NextProtos = []string{"http/1.1"}
			tr.TLSClientConfig = cfg
		}
		// (If TLSClientConfig is nil there is no ALPN offer to
		// restrict — the transport is already HTTP/1.1-only.)
		return tr
	}
	// Unreachable with the stdlib default transport, but a fresh
	// transport pinned the same way (HTTP/1.1 only, both
	// mechanisms) is safer than a nil one. ForceAttemptHTTP2 =
	// false alone does NOT pin anything: with all dialers/config
	// nil, the stdlib enables h2 by default (protocols()), so a
	// bare literal would still offer h2 in ALPN and negotiate
	// HTTP/2.0 against an h2 server.
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:     false,
		TLSNextProto:          map[string]func(string, *tls.Conn) http.RoundTripper{},
		TLSClientConfig:       &tls.Config{NextProtos: []string{"http/1.1"}},
	}
	return tr
}

// newPollHTTPClient builds the dedicated getUpdates client: its own
// HTTP/1.1-only transport, whole-call deadline pollHTTPTimeout.
func newPollHTTPClient() *http.Client {
	return &http.Client{Timeout: pollHTTPTimeout, Transport: newPollTransport()}
}

// isTimeoutError reports whether err is a timeout-class failure: a
// context deadline, or a net.Error with Timeout() true. Detection is by
// error inspection only — NEVER by matching error strings. No prior art
// distinguishes "stuck" from "normal" timeout by string (research §6
// item 10): detection is always elapsed-time based (window + grace, or
// time since last success), and Orion sees "context deadline exceeded",
// cc-connect saw "unexpected EOF", talon saw nothing at all until its
// own deadline fired. A 429/5xx apiError is NOT a timeout, so it never
// triggers a rebuild — only backoff.
func isTimeoutError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// notePollLoopStarted records PollStream's start as the watchdog
// baseline before any poll has completed.
func (g *TelegramGateway) notePollLoopStarted() {
	g.pollStateMu.Lock()
	defer g.pollStateMu.Unlock()
	if g.pollLoopStartedAt.IsZero() {
		g.pollLoopStartedAt = time.Now()
	}
}

// notePollCompleted records a successful getUpdates fetch: the
// watchdog baseline moves to now and the consecutive-timeout streak
// resets. Called from fetchUpdatesLocked, the single fetch funnel, so
// both PollStream and one-shot GetUpdates report completions.
func (g *TelegramGateway) notePollCompleted() {
	g.pollStateMu.Lock()
	defer g.pollStateMu.Unlock()
	g.lastPollCompletedAt = time.Now()
	g.pollConsecutiveTimeouts = 0
}

// notePollTimeout counts one more consecutive getUpdates timeout and
// returns the new streak length.
func (g *TelegramGateway) notePollTimeout() int {
	g.pollStateMu.Lock()
	defer g.pollStateMu.Unlock()
	g.pollConsecutiveTimeouts++
	return g.pollConsecutiveTimeouts
}

// resetPollTimeouts clears the consecutive-timeout streak. A non-timeout
// transient error (429, 5xx, DNS, refused) proves the connection
// answered at that moment, so the streak of *consecutive* timeouts is
// broken even though polling as a whole has not recovered.
func (g *TelegramGateway) resetPollTimeouts() {
	g.pollStateMu.Lock()
	defer g.pollStateMu.Unlock()
	g.pollConsecutiveTimeouts = 0
}

// setPollBackoff records whether PollStream is sleeping between attempts
// after a transient failure. It feeds PollHealth only.
func (g *TelegramGateway) setPollBackoff(inBackoff bool) {
	g.pollStateMu.Lock()
	defer g.pollStateMu.Unlock()
	g.pollInBackoff = inBackoff
}

// pollWatchdogDueLocked reports whether the watchdog has tripped, given
// pollStateMu is already held. The baseline is the latest of loop start,
// last completed poll, and last rebuild: a rebuild is the recovery
// action, so the watchdog must not fire again immediately after one.
func (g *TelegramGateway) pollWatchdogDueLocked(now time.Time) bool {
	baseline := g.pollLoopStartedAt
	if g.lastPollCompletedAt.After(baseline) {
		baseline = g.lastPollCompletedAt
	}
	if g.lastPollRebuildAt.After(baseline) {
		baseline = g.lastPollRebuildAt
	}
	if baseline.IsZero() {
		return false
	}
	return now.Sub(baseline) >= g.pollWatchdogInterval
}

// pollWatchdogDue reports whether the watchdog has tripped.
func (g *TelegramGateway) pollWatchdogDue(now time.Time) bool {
	g.pollStateMu.Lock()
	defer g.pollStateMu.Unlock()
	return g.pollWatchdogDueLocked(now)
}

// rebuildPollTransport swaps the poll client's transport for a fresh one
// built with a fresh dialer, and resets the consecutive-timeout streak.
// It MUST run between attempts, never while a poll is in flight: it
// takes fetchMu, which serialises it against any in-flight getUpdates
// (PollStream holds fetchMu only for the fetch itself; GetUpdates holds
// it across fetch-and-advance). CloseIdleConnections on the OLD transport
// is hygiene only — it closes idle pooled connections and CANNOT touch
// the in-flight dead one; the rebuild itself is the fix, because the new
// transport starts with an empty pool and the next attempt dials fresh.
// The swap is safe while other goroutines use the client: http.Client
// is safe for concurrent use and the Transport field is only ever
// written here, under fetchMu.
func (g *TelegramGateway) rebuildPollTransport(reason string) {
	g.fetchMu.Lock()
	defer g.fetchMu.Unlock()
	g.pollStateMu.Lock()
	g.pollRebuilding = true
	g.pollStateMu.Unlock()
	defer func() {
		g.pollStateMu.Lock()
		g.pollRebuilding = false
		g.pollStateMu.Unlock()
	}()
	if g.pollClient == nil {
		g.pollClient = newPollHTTPClient()
	} else {
		old := g.pollClient.Transport
		g.pollClient.Transport = newPollTransport()
		if old != nil {
			if tr, ok := old.(*http.Transport); ok {
				tr.CloseIdleConnections()
			}
		}
	}
	g.pollStateMu.Lock()
	g.pollRebuilds++
	rebuilds := g.pollRebuilds
	timeouts := g.pollConsecutiveTimeouts
	g.pollConsecutiveTimeouts = 0
	g.lastPollRebuildAt = time.Now()
	g.pollStateMu.Unlock()
	g.logf("telegram poll: rebuilt poll transport (%s; %d consecutive timeout(s); rebuild #%d)", reason, timeouts, rebuilds)
}

// PollHealth is a snapshot of the getUpdates poller's state for the
// daemon's health/debug surface. Healthy is false whenever the poller is
// in backoff or rebuilding, has any consecutive timeouts outstanding, has
// never started, or has gone a full watchdog interval without a completed
// poll — a poller in rebuild/backoff must NOT report healthy.
type PollHealth struct {
	Healthy             bool
	Reason              string
	ConsecutiveTimeouts int
	LastCompletedAt     time.Time
	LastRebuildAt       time.Time
	Rebuilds            int
	InBackoff           bool
	Rebuilding          bool
}

// String renders the snapshot as one log/debug line. It carries counts
// and timestamps only — no token, no message content.
func (h PollHealth) String() string {
	return fmt.Sprintf("healthy=%v reason=%q consecutive_timeouts=%d rebuilds=%d last_completed=%s last_rebuild=%s in_backoff=%v rebuilding=%v",
		h.Healthy, h.Reason, h.ConsecutiveTimeouts, h.Rebuilds,
		h.LastCompletedAt.Format(time.RFC3339), h.LastRebuildAt.Format(time.RFC3339),
		h.InBackoff, h.Rebuilding)
}

// PollHealth returns the current poller snapshot.
func (g *TelegramGateway) PollHealth() PollHealth {
	g.pollStateMu.Lock()
	defer g.pollStateMu.Unlock()
	h := PollHealth{
		ConsecutiveTimeouts: g.pollConsecutiveTimeouts,
		LastCompletedAt:     g.lastPollCompletedAt,
		LastRebuildAt:       g.lastPollRebuildAt,
		Rebuilds:            g.pollRebuilds,
		InBackoff:           g.pollInBackoff,
		Rebuilding:          g.pollRebuilding,
	}
	switch {
	case g.pollLoopStartedAt.IsZero():
		h.Reason = "polling not started"
	case h.Rebuilding:
		h.Reason = "rebuilding poll transport"
	case h.InBackoff:
		h.Reason = "backing off after transient poll error"
	case h.ConsecutiveTimeouts > 0:
		h.Reason = fmt.Sprintf("%d consecutive poll timeout(s)", h.ConsecutiveTimeouts)
	case g.pollWatchdogDueLocked(time.Now()):
		h.Reason = fmt.Sprintf("no completed poll for >= %s", g.pollWatchdogInterval)
	default:
		h.Healthy = true
	}
	return h
}

// redactedError wraps an error whose message may contain the bot token
// (notably *url.Error, which embeds the full request URL including the
// token in the path).
//
// Safety rule: ONLY the wrapped error (the *redactedError itself) is
// safe to log or to format with %v / %+v. Its Error() returns the
// message with the token replaced by "[redacted]". Never log
// errors.Unwrap(err): the raw wrapped error still contains the token,
// and the only safe handle on it is this wrapper.
//
// errors.Is / errors.As keep working without exposing the raw error:
// Is and As forward to the wrapped error's own chain, and Unwrap
// returns a redacted view of the wrapped error's cause (recursively
// redacted), never the raw error itself. A timeout is therefore still
// detectable via errors.As(err, &net.Error) while every level of the
// chain stays token-free.
type redactedError struct {
	err   error
	token string
}

func (e *redactedError) Error() string {
	if e.err == nil {
		return ""
	}
	return strings.ReplaceAll(e.err.Error(), e.token, "[redacted]")
}

// Is forwards errors.Is to the wrapped error, so sentinel matching
// (e.g. context.DeadlineExceeded) works without unwrapping to the raw,
// token-bearing error.
func (e *redactedError) Is(target error) bool {
	if e.err == nil {
		return false
	}
	return errors.Is(e.err, target)
}

// As forwards errors.As to the wrapped error, so type matching (e.g.
// net.Error for a timed-out request) works without unwrapping to the
// raw, token-bearing error.
func (e *redactedError) As(target any) bool {
	if e.err == nil {
		return false
	}
	return errors.As(e.err, target)
}

// Unwrap returns a redacted view of the wrapped error's cause — never
// the wrapped error itself. Code that unwraps one level (fmt %w
// chains, errors.As traversal) therefore still only ever sees
// token-free messages, while the cause chain remains walkable.
func (e *redactedError) Unwrap() error {
	if e.err == nil {
		return nil
	}
	u, ok := e.err.(interface{ Unwrap() error })
	if !ok {
		return nil
	}
	inner := u.Unwrap()
	if inner == nil {
		return nil
	}
	return &redactedError{err: inner, token: e.token}
}

// redact wraps err so the token never appears in its Error() string.
// A nil err stays nil; an empty token is a no-op.
//
// Callers must log, return, and format ONLY the error redact returns.
// The raw err (and errors.Unwrap of the result) still contains the
// token and must never be logged.
func (g *TelegramGateway) redact(err error) error {
	if err == nil || g.token == "" {
		return err
	}
	return &redactedError{err: err, token: g.token}
}

// apiResponse is the envelope every Bot API call returns.
type apiResponse struct {
	OK          bool            `json:"ok"`
	Description string          `json:"description"`
	ErrorCode   int             `json:"error_code"`
	Result      json.RawMessage `json:"result"`
}

// call performs one Bot API call and returns the decoded result payload.
// method is the Bot API method name (e.g. "getMe"). query holds URL query
// parameters for GET calls; body, when non-nil, is JSON-encoded and sent
// as a POST. All errors are wrapped with the method name.
//
// Token safety: every error that can carry the token (notably the
// *url.Error from client.Do, which embeds the request URL) leaves this
// function wrapped by g.redact. Only that wrapped error is safe to log;
// the raw error underneath it still contains the token (see
// redactedError).
func (g *TelegramGateway) call(ctx context.Context, method string, query url.Values, body any) (json.RawMessage, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	// The token is URL-escaped before it is interpolated into the path
	// (see P2-30): a malformed token containing "/" or "?" must not
	// alter the request path or query.
	endpoint := fmt.Sprintf("%s/bot%s/%s", g.baseURL, url.PathEscape(g.token), method)
	u, err := url.Parse(endpoint)
	if err != nil {
		// Safe to log: g.redact returns the token-free wrapper; the
		// raw err must never be logged.
		return nil, g.redact(fmt.Errorf("messaging: telegram %s: invalid endpoint: %w", method, err))
	}
	if len(query) > 0 {
		u.RawQuery = query.Encode()
	}

	var reqBody io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("messaging: telegram %s: marshal request: %w", method, err)
		}
		reqBody = bytes.NewReader(data)
	}

	httpMethod := http.MethodGet
	if body != nil {
		httpMethod = http.MethodPost
	}
	req, err := http.NewRequestWithContext(ctx, httpMethod, u.String(), reqBody)
	if err != nil {
		// Safe to log: g.redact returns the token-free wrapper; the
		// raw err must never be logged.
		return nil, g.redact(fmt.Errorf("messaging: telegram %s: create request: %w", method, err))
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")

	// getUpdates rides the dedicated poll client (own HTTP/1.1-only
	// transport, 45s deadline); every other method rides the shared
	// send/message client. A nil poll client (a Gateway built without
	// the constructor) falls back to the shared client rather than
	// crashing — production gateways always have one.
	client := g.client
	if method == "getUpdates" && g.pollClient != nil {
		client = g.pollClient
	}
	resp, err := client.Do(req)
	if err != nil {
		// Safe to log: g.redact returns the token-free wrapper. The
		// raw *url.Error embeds the request URL (and the token) and
		// must never be logged or unwrapped for logging.
		return nil, g.redact(fmt.Errorf("messaging: telegram %s: request: %w", method, err))
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return nil, fmt.Errorf("messaging: telegram %s: read response: %w", method, err)
	}

	var env apiResponse
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("messaging: telegram %s: decode response (HTTP %d): %w", method, resp.StatusCode, err)
	}
	if resp.StatusCode != http.StatusOK || !env.OK {
		desc := env.Description
		if desc == "" {
			desc = strings.TrimSpace(string(data))
			if desc == "" {
				desc = http.StatusText(resp.StatusCode)
			}
		}
		// Cap the description: the raw-body fallback above can be up
		// to the 10 MiB read cap, and an uncapped body would land in
		// the returned error (and from there in stderr, logs, or a
		// reply sent back to the chat). The message shape is
		// unchanged — apiError formats exactly like the old
		// fmt.Errorf calls did.
		apiErr := &apiError{
			Method:      method,
			StatusCode:  resp.StatusCode,
			Code:        env.ErrorCode,
			Description: truncateErrorDescription(desc),
		}
		return nil, g.redact(fmt.Errorf("messaging: telegram %s: %w", method, apiErr))
	}
	return env.Result, nil
}

// apiError is a Bot API error: either a non-2xx HTTP status, a
// response with "ok": false, or both. Code is Telegram's error_code
// (0 when the response carried none); Description is Telegram's
// description, capped so a large error body can never produce a
// multi-megabyte error string (see maxErrorDescriptionRunes).
//
// Poll treats 401/403/409 as fatal (revoked token, blocked bot,
// another poller holding the connection): retrying those forever at
// the backoff cadence looks alive while being dead.
type apiError struct {
	Method      string
	StatusCode  int
	Code        int
	Description string
}

func (e *apiError) Error() string {
	if e.Code != 0 {
		return fmt.Sprintf("HTTP %d, error %d: %s", e.StatusCode, e.Code, e.Description)
	}
	return fmt.Sprintf("HTTP %d: %s", e.StatusCode, e.Description)
}

// isFatalForPoll reports whether this error can never heal by
// retrying: an unauthorised/forbidden token or a conflicting
// getUpdates (another instance polling with the same token). The
// Telegram error_code is checked first; when the response carried
// none (e.g. a proxy error page) the HTTP status decides.
func (e *apiError) isFatalForPoll() bool {
	for _, c := range []int{e.Code, e.StatusCode} {
		if c == http.StatusUnauthorized || c == http.StatusForbidden || c == http.StatusConflict {
			return true
		}
	}
	return false
}

// maxErrorDescriptionRunes caps an error description taken from a Bot
// API failure. The raw body fallback in call can be up to the 10 MiB
// read cap; without a cap that whole body lands in the returned
// error, which callers print to stderr (orion telegram test, oriond)
// or send back to the chat (Poll's handler-error reply).
const maxErrorDescriptionRunes = 512

// truncateErrorDescription shortens s to at most
// maxErrorDescriptionRunes runes, appending "..." when it truncates.
// Rune-based so the result stays valid UTF-8.
func truncateErrorDescription(s string) string {
	runes := []rune(s)
	if len(runes) <= maxErrorDescriptionRunes {
		return s
	}
	return string(runes[:maxErrorDescriptionRunes]) + "..."
}

type getMeResult struct {
	ID        int64  `json:"id"`
	Username  string `json:"username"`
	FirstName string `json:"first_name"`
}

// GetMe returns the bot's username, verifying the token against the Bot
// API. A non-ok response (e.g. a bad token) is returned as an error
// carrying Telegram's description.
func (g *TelegramGateway) GetMe(ctx context.Context) (string, error) {
	result, err := g.call(ctx, "getMe", nil, nil)
	if err != nil {
		return "", err
	}
	var me getMeResult
	if err := json.Unmarshal(result, &me); err != nil {
		return "", fmt.Errorf("messaging: telegram getMe: decode result: %w", err)
	}
	return me.Username, nil
}

// sendMessageRequest is the JSON body for sendMessage.
type sendMessageRequest struct {
	ChatID    int64  `json:"chat_id"`
	Text      string `json:"text"`
	ParseMode string `json:"parse_mode,omitempty"`
}

// sendMessageResult is the message object sendMessage returns.
type sendMessageResult struct {
	MessageID int64 `json:"message_id"`
}

// sendMessageRaw posts one sendMessage call and returns the new
// message's id. parseMode is "" for plain text or "HTML" for the
// terminal card.
func (g *TelegramGateway) sendMessageRaw(ctx context.Context, chatID int64, text, parseMode string) (int64, error) {
	result, err := g.call(ctx, "sendMessage", nil, sendMessageRequest{ChatID: chatID, Text: text, ParseMode: parseMode})
	if err != nil {
		return 0, err
	}
	var msg sendMessageResult
	if err := json.Unmarshal(result, &msg); err != nil {
		return 0, fmt.Errorf("messaging: telegram sendMessage: decode result: %w", err)
	}
	return msg.MessageID, nil
}

// sendMessageOnce sends a single message of at most MaxMessageLength runes.
func (g *TelegramGateway) sendMessageOnce(ctx context.Context, chatID int64, text string) error {
	_, err := g.sendMessageRaw(ctx, chatID, text, "")
	return err
}

// SendMessageHTML sends one HTML message to chatID and returns the new
// message's id, so the caller can edit it in place later. Unlike
// SendMessage it does not split: card HTML is already capped at 4000
// runes by TerminalCard.Render.
func (g *TelegramGateway) SendMessageHTML(ctx context.Context, chatID int64, htmlText string) (int64, error) {
	return g.sendMessageRaw(ctx, chatID, htmlText, "HTML")
}

// SendMessageReturningID sends one plain-text message to chatID and
// returns the new message's id, so the caller can edit it in place
// later. It is SendMessage for callers that need the id (the
// ask_user_question transport, for its expiry edit) without an
// inline keyboard: unlike SendMessage it does not split, so
// callers must keep the text under 4096 runes.
func (g *TelegramGateway) SendMessageReturningID(ctx context.Context, chatID int64, text string) (int64, error) {
	return g.sendMessageRaw(ctx, chatID, text, "")
}

// SendMessageWithKeyboard sends one plain-text message with an inline
// keyboard to chatID and returns the new message's id. It is the
// approval prompt's transport: the keyboard's buttons carry
// callback_data the callback handler resolves. The text is sent as
// plain text (no parse mode), so approval summaries never need HTML
// escaping. Long texts are sent as-is (approval prompts are short
// by construction); callers must keep them under 4096 runes.
func (g *TelegramGateway) SendMessageWithKeyboard(ctx context.Context, chatID int64, text string, keyboard InlineKeyboardMarkup) (int64, error) {
	result, err := g.call(ctx, "sendMessage", nil, struct {
		ChatID      int64                `json:"chat_id"`
		Text        string               `json:"text"`
		ReplyMarkup InlineKeyboardMarkup `json:"reply_markup"`
	}{ChatID: chatID, Text: text, ReplyMarkup: keyboard})
	if err != nil {
		return 0, err
	}
	var msg sendMessageResult
	if err := json.Unmarshal(result, &msg); err != nil {
		return 0, fmt.Errorf("messaging: telegram sendMessage: decode result: %w", err)
	}
	return msg.MessageID, nil
}

// AnswerCallbackQuery answers a callback query (removes the loading
// spinner on the tapped button, and optionally shows a short toast).
// text is shown as a toast when non-empty (Telegram caps it at 200
// runes). It is best-effort from the caller's view: a failed answer
// must never fail the approval it acknowledges.
func (g *TelegramGateway) AnswerCallbackQuery(ctx context.Context, callbackID, text string) error {
	_, err := g.call(ctx, "answerCallbackQuery", nil, struct {
		CallbackQueryID string `json:"callback_query_id"`
		Text            string `json:"text,omitempty"`
	}{CallbackQueryID: callbackID, Text: text})
	return err
}

// Permission callback data format: "perm:<decision>:<requestID>",
// where decision is approve, deny, always or never. The request ID
// is the gate's opaque pending-request ID (16 hex chars).
const permissionCallbackPrefix = "perm:"

// approvalNoLongerPendingNotice is the toast for a tap on an
// approval card whose request is no longer pending (F-A20-9).
// The pending approvals live only in memory, so a daemon
// restart voids every live card; a settled card has its keyboard
// removed, so nobody can tap it; and a double-tap's second tap
// races the first tap's resolve. The handler therefore cannot
// tell WHICH of the three happened — already answered, expired,
// or lost to a restart — and says so honestly instead of
// picking one (a bare "expired" blamed the user for being slow
// when the bot had restarted, and "ask again" would be wrong
// for a double-tap whose request was already approved). One
// deterministic toast covers all three, so the outcome never
// depends on the tap/resolve interleave.
const approvalNoLongerPendingNotice = "This approval request is no longer pending — it may have been answered, expired, or lost when the bot restarted."

// ApprovalKeyboard builds the inline keyboard for an approval
// prompt: Approve once / Deny on the first row, Always allow /
// Never allow on the second. "Always"/"Never" persist a standing
// rule for the tool, so the user is not asked again.
func ApprovalKeyboard(requestID string) InlineKeyboardMarkup {
	return InlineKeyboardMarkup{InlineKeyboard: [][]InlineKeyboardButton{
		{
			{Text: "✅ Approve once", CallbackData: permissionCallbackPrefix + "approve:" + requestID},
			{Text: "❌ Deny", CallbackData: permissionCallbackPrefix + "deny:" + requestID},
		},
		{
			{Text: "✅ Always allow", CallbackData: permissionCallbackPrefix + "always:" + requestID},
			{Text: "⛔ Never allow", CallbackData: permissionCallbackPrefix + "never:" + requestID},
		},
	}}
}

// ParsePermissionCallback parses callback data from an approval
// keyboard button. It returns the decision ("approve", "deny",
// "always" or "never") and the pending-request ID. ok is false for
// any other callback data (other buttons, other features).
func ParsePermissionCallback(data string) (decision, requestID string, ok bool) {
	if !strings.HasPrefix(data, permissionCallbackPrefix) {
		return "", "", false
	}
	rest := strings.TrimPrefix(data, permissionCallbackPrefix)
	decision, requestID, found := strings.Cut(rest, ":")
	if !found || requestID == "" {
		return "", "", false
	}
	switch decision {
	case "approve", "deny", "always", "never":
		return decision, requestID, true
	default:
		return "", "", false
	}
}

// --- Shared permission-gate Telegram wiring (F-A7-P2-2) -------------
//
// Before rc12 the approval wiring existed twice — once in cmd/orion
// (wireGate) and once in cmd/oriond (buildApprovalGate) — as two
// near-identical copies of the prompter text, the expiry hook, the
// card tracker and the tap resolver, plus two separate gate
// constructions (oriond called permissions.NewGate directly while
// cmd/orion called app.NewPermissionGate). The security property
// held on both paths, but the copies could drift apart again, and
// the wireGate comment claiming oriond used the shared constructor
// was false. The wiring below is the single implementation both
// binaries use: each binary still builds its gate through the
// shared constructor (app.NewPermissionGate) and passes the
// prompter from ApprovalPrompter, then installs the expiry hook
// and the callback handler from here. The only per-binary
// difference is the log prefix ("orion" vs "oriond"), passed in so
// log lines keep naming the binary that emitted them. Gate
// behaviour is unchanged: same prompt text, same expiry edit,
// same requester-or-owner tap rule.

// ApprovalCards tracks live approval-card message IDs by pending
// request ID. Note records a card when the prompter sends it;
// Take removes and returns it when the request expires (so the
// expiry edit runs once); Forget removes it when the request is
// answered (so a resolved card is never expired later). All
// methods are nil-safe: a nil *ApprovalCards means "no tracking"
// (tests that call HandleApprovalCallback directly pass nil).
type ApprovalCards struct {
	mu sync.Mutex
	m  map[string]int64
	// pending holds request ids whose card send has started but
	// whose message id is not yet known (F-A20-11): the prompter
	// Notes the id BEFORE sending, so a card is never observable
	// (in the Telegram outbox) before it is tracked. Take only
	// ever returns a real card (msgID != 0); Peek reports the
	// tracked message id without removing it.
	pending map[string]bool
}

// NewApprovalCards returns an empty card tracker.
func NewApprovalCards() *ApprovalCards {
	return &ApprovalCards{m: map[string]int64{}, pending: map[string]bool{}}
}

// NotePending records id as "card send in flight" BEFORE the
// prompter sends it (F-A20-11). A test (or the expiry hook) that
// observes the card in the outbox can then know the tracker
// already knows the request — the old race was that the card was
// observable before Note ran, so a Take in between returned
// (0, false) for a card that really existed.
func (c *ApprovalCards) NotePending(id string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]int64{}
	}
	if c.pending == nil {
		c.pending = map[string]bool{}
	}
	if _, ok := c.m[id]; !ok {
		c.pending[id] = true
	}
}

// Note records msgID as the live card for pending request id.
func (c *ApprovalCards) Note(id string, msgID int64) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]int64{}
	}
	c.m[id] = msgID
	delete(c.pending, id)
}

// Peek returns the tracked message id for id without removing it:
// (msgID, true) once the card has been sent and Noted, (0, false)
// while the send is still in flight or the id is unknown.
func (c *ApprovalCards) Peek(id string) (int64, bool) {
	if c == nil {
		return 0, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	msgID, ok := c.m[id]
	if !ok || msgID == 0 {
		return 0, false
	}
	return msgID, true
}

// Take removes and returns the card for id, reporting whether one
// was tracked. A send still in flight (NotePending, no Note yet)
// is NOT a card: Take returns (0, false) and leaves the pending
// mark in place, so the Note that follows still lands.
func (c *ApprovalCards) Take(id string) (int64, bool) {
	if c == nil {
		return 0, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	msgID, ok := c.m[id]
	if !ok || msgID == 0 {
		return 0, false
	}
	delete(c.m, id)
	return msgID, ok
}

// Forget removes any tracked card for id without returning it.
func (c *ApprovalCards) Forget(id string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, id)
	delete(c.pending, id)
}

// ApprovalPromptText renders the approval-card text for req: what
// is being asked, which chat it is for, and who requested it, plus
// the button instructions. Shared by both binaries so the card a
// user sees never depends on which entrypoint is running.
func ApprovalPromptText(req permissions.ApprovalRequest) string {
	return fmt.Sprintf("⚠️ Approval needed\n\n%s\n\nChat: %d\nRequested by user: %d\n\nTap ✅ to run it once, ❌ to refuse it, or use Always/Never to remember the choice for this tool in this chat only.", req.Summary(), req.ChatID, req.SenderID)
}

// ApprovalPrompter builds the gate prompter for gw: it sends the
// Telegram inline-keyboard approval card (recording its message ID
// in cards so the expiry hook can edit it in place) and logs the
// prompt. A send failure is returned so the gate can treat the
// prompt as undelivered.
func ApprovalPrompter(gw *TelegramGateway, cards *ApprovalCards, logPrefix string) permissions.Prompter {
	// Capture stderr once, here: the prompter runs on turn
	// goroutines, and reading the os.Stderr global on every
	// prompt races with any test (or embedder) that swaps it
	// while the daemon runs (data race found by -race in
	// TestW3DaemonAndTelegramPollGatePrompts, pre-existing on
	// rc12). The captured writer is the same destination in
	// production; only the read moves to setup time.
	stderr := os.Stderr
	return func(ctx context.Context, req permissions.ApprovalRequest) error {
		// Note BEFORE sending (F-A20-11): the request is tracked
		// from the moment its send starts, so a card is never
		// observable in the outbox before the tracker knows it.
		// The old order (send, then Note) let a fast observer
		// Take the card before Note ran and see (0, false) for
		// a card that really existed.
		cards.NotePending(req.ID)
		msgID, err := gw.SendMessageWithKeyboard(ctx, req.ChatID, ApprovalPromptText(req), ApprovalKeyboard(req.ID))
		if err == nil {
			cards.Note(req.ID, msgID)
			fmt.Fprintf(stderr, "%s: approval prompt: tool %q request %s chat %d card %d\n", logPrefix, req.Tool, req.ID, req.ChatID, msgID)
		} else {
			// The send failed: drop the pending mark, so a
			// later request with this id is never confused
			// with a card that was never delivered.
			cards.Forget(req.ID)
		}
		return err
	}
}

// InstallApprovalExpiry installs gate's expiry hook: when a
// request dies unanswered, its card is edited in place — text
// replaced and keyboard REMOVED, so nobody can tap a dead button.
// The edit runs in a goroutine (the hook must return promptly)
// with a background context — the turn's context may already be
// cancelled, which is exactly why the request expired.
// Best-effort: a failed edit is logged, never fatal.
func InstallApprovalExpiry(gate *permissions.Gate, gw *TelegramGateway, cards *ApprovalCards, logPrefix string) {
	// Capture stderr once, here: the hook and its edit goroutine
	// run on background goroutines, and reading the os.Stderr
	// global there races with any test (or embedder) that swaps
	// it while the daemon runs (data race found by -race in
	// TestW3DaemonAndTelegramPollGatePrompts, pre-existing on
	// rc12: the expiry goroutine's Fprintf read os.Stderr while
	// the test's capture helper restored it). The captured
	// writer is the same destination in production.
	stderr := os.Stderr
	gate.SetOnExpire(func(req permissions.ApprovalRequest) {
		msgID, ok := cards.Take(req.ID)
		if !ok {
			return
		}
		fmt.Fprintf(stderr, "%s: approval expired: request %s chat %d card %d\n", logPrefix, req.ID, req.ChatID, msgID)
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			// Log an edit failure: a failed expiry edit (e.g.
			// message deleted) must leave a trace instead of
			// being swallowed.
			if err := gw.EditMessageTextMarkup(ctx, req.ChatID, msgID, "⚠️ Approval request: ⏰ Expired — no answer within the approval window. Send the request again to retry.", InlineKeyboardMarkup{InlineKeyboard: [][]InlineKeyboardButton{}}); err != nil {
				fmt.Fprintf(stderr, "%s: approval expired: request %s card %d edit failed: %v\n", logPrefix, req.ID, msgID, err)
			}
		}()
	})
}

// HandleApprovalCallback resolves one permission-gate approval tap
// (P1-O2): the decision comes from the button's callback data
// (perm:<decision>:<requestID>); the pending request is looked up
// in the gate, and only the user whose turn requested the tool call
// — or the owner — may answer it. A tap from anyone else is refused
// without touching the pending request, so a stranger can neither
// approve nor deny someone else's tool call. The approval message
// is edited in place to show the outcome, and the query is answered
// to clear the button spinner. A tap on an expired request is told
// so. Every tap is logged (decision, request ID, sender, outcome):
// a tap that never arrives and a tap on a dead card must be
// distinguishable in the log.
func HandleApprovalCallback(ctx context.Context, gw *TelegramGateway, gate *permissions.Gate, ownerID int64, q TGCallbackQuery, cards *ApprovalCards, logPrefix string) error {
	// Capture stderr once, here: this runs on callback-handler
	// goroutines, and reading the os.Stderr global on every tap
	// races with any test (or embedder) that swaps it while the
	// daemon runs (same class as the InstallApprovalExpiry race
	// found by -race in TestW3DaemonAndTelegramPollGatePrompts).
	// The capture is race-free for this flow: no new handler
	// starts after the poll loop exits, so the entry read can
	// never overlap the swap-back that follows it.
	stderr := os.Stderr
	decision, id, ok := ParsePermissionCallback(q.Data)
	if !ok {
		// Log the unparseable tap: without this a tap the
		// handler cannot understand is silent, and a malformed
		// button looks exactly like a dead one. The data is a
		// short callback payload (perm:<decision>:<id>), never
		// free user text, so logging it is safe.
		fmt.Fprintf(stderr, "%s: approval tap: unparseable callback data %q (query %s)\n", logPrefix, q.Data, q.ID)
		return gw.AnswerCallbackQuery(ctx, q.ID, "Unknown button.")
	}
	senderID := int64(0)
	if q.From != nil {
		senderID = q.From.ID
	}
	fmt.Fprintf(stderr, "%s: approval tap: %s request %s from %d\n", logPrefix, decision, id, senderID)
	req, found := gate.Pending(id)
	if !found {
		// The request is not pending: already answered, expired,
		// or lost to a restart (F-A20-9) — the handler cannot
		// tell which, so the toast says so (see
		// approvalNoLongerPendingNotice).
		fmt.Fprintf(stderr, "%s: approval tap: request %s no longer pending (answered, expired, or lost to a restart)\n", logPrefix, id)
		return gw.AnswerCallbackQuery(ctx, q.ID, approvalNoLongerPendingNotice)
	}
	if senderID != req.SenderID && senderID != ownerID {
		fmt.Fprintf(stderr, "%s: approval tap: request %s refused for sender %d (requester %d, owner %d)\n", logPrefix, id, senderID, req.SenderID, ownerID)
		return gw.AnswerCallbackQuery(ctx, q.ID, "Only the person who asked can approve this.")
	}
	var persist *permissions.Action
	approved := false
	outcome := ""
	switch decision {
	case "approve":
		approved, outcome = true, "✅ Approved — running it now."
	case "deny":
		approved, outcome = false, "❌ Denied."
	case "always":
		allow := permissions.ActionAllow
		persist, approved, outcome = &allow, true, "✅ Approved — I won't ask again for this tool in this chat."
	case "never":
		deny := permissions.ActionDeny
		persist, approved, outcome = &deny, false, "⛔ Denied — I won't ask again for this tool in this chat."
	}
	if !gate.Resolve(id, approved, persist) {
		// Same toast as the not-pending branch: the resolve
		// lost the race to another tap or the timeout, and the
		// handler cannot tell which — one deterministic toast
		// for both, so the outcome never depends on the
		// tap/resolve interleave (F-A20-9; the precise
		// "already approved" toast is the open P3 F-A20-23).
		fmt.Fprintf(stderr, "%s: approval tap: request %s no longer pending during resolve (answered by another tap, or expired)\n", logPrefix, id)
		return gw.AnswerCallbackQuery(ctx, q.ID, approvalNoLongerPendingNotice)
	}
	fmt.Fprintf(stderr, "%s: approval tap: request %s resolved approved=%v\n", logPrefix, id, approved)
	cards.Forget(id)
	// Edit the prompt in place so the chat shows the outcome. The
	// edit carries the outcome only (no tool arguments): the
	// prompt's summary is user/model-derived text and
	// EditMessageText sends HTML, so echoing it back could break
	// the markup. The edit uses EditMessageTextMarkup with an EMPTY
	// keyboard: omitting reply_markup leaves the old keyboard in
	// place, so a resolved card would keep live Approve/Deny
	// buttons. An edit failure is logged, never swallowed.
	if q.Message != nil {
		if err := gw.EditMessageTextMarkup(ctx, q.Message.Chat.ID, q.Message.MessageID, "⚠️ Approval request: "+outcome, InlineKeyboardMarkup{InlineKeyboard: [][]InlineKeyboardButton{}}); err != nil {
			fmt.Fprintf(stderr, "%s: approval tap: request %s outcome edit failed: %v\n", logPrefix, id, err)
		}
	}
	return gw.AnswerCallbackQuery(ctx, q.ID, outcome)
}

// InstallApprovalCallbackHandler installs the gateway callback
// handler that resolves approval taps through gate: only the
// requester or the owner may answer (see
// HandleApprovalCallback). Callers must install this (or an
// equivalent) or approvals can never be answered — the gate
// prompts, but no tap reaches it.
//
// It is implemented on the callback router: the approval handler
// is the router's fallback (perm: taps and unparseable data
// alike), and the question handler is registered under "ask:"
// (a nil questions registry means "no questions": ask: taps are
// then answered as expired, exactly as before). One installed
// handler serves both the approval gate and the question asker —
// the gateway holds a single callback handler, and a second
// installer fails loudly instead of overwriting the first
// (F-A20-5/F-A20-6). Approval behaviour is unchanged: anything
// that is not an ask: tap goes to HandleApprovalCallback exactly
// as before.
//
// This is the compatibility installer for the test harnesses and
// for callers that wire approvals directly; the binaries use the
// framework's WireTelegram, which installs the same router.
func InstallApprovalCallbackHandler(gw *TelegramGateway, gate *permissions.Gate, ownerID int64, cards *ApprovalCards, questions *QuestionRegistry, logPrefix string) error {
	if err := gw.SetApprovalFallback(gate, ownerID, cards, logPrefix); err != nil {
		return fmt.Errorf("messaging: InstallApprovalCallbackHandler: %w", err)
	}
	// A duplicate "ask:" registration (the framework already
	// installed one) is a loud error (F-A20-5): silently
	// ignoring it hid a double-wiring bug. The handler is
	// registered even for a nil registry — an ask: tap with no
	// questions is answered as expired, never silently dropped.
	if err := gw.RegisterCallbackHandler(questionCallbackPrefix, questionCallbackHandler{questions: questions, logPrefix: logPrefix}); err != nil {
		return fmt.Errorf("messaging: InstallApprovalCallbackHandler: %w", err)
	}
	if err := gw.InstallCallbackRouter(); err != nil {
		return fmt.Errorf("messaging: InstallApprovalCallbackHandler: %w", err)
	}
	return nil
}

// SendChatAction sends a chat action (e.g. "typing") to chatID, so the
// user sees the bot working while a turn runs.
func (g *TelegramGateway) SendChatAction(ctx context.Context, chatID int64, action string) error {
	_, err := g.call(ctx, "sendChatAction", nil, struct {
		ChatID int64  `json:"chat_id"`
		Action string `json:"action"`
	}{ChatID: chatID, Action: action})
	return err
}

// EditMessageText edits messageID in chatID in place with new HTML
// text. Telegram's "message is not modified" error (the new text is
// identical to the current text) is treated as success: it must not
// fail the turn.
func (g *TelegramGateway) EditMessageText(ctx context.Context, chatID, messageID int64, htmlText string) error {
	_, err := g.call(ctx, "editMessageText", nil, struct {
		ChatID    int64  `json:"chat_id"`
		MessageID int64  `json:"message_id"`
		Text      string `json:"text"`
		ParseMode string `json:"parse_mode"`
	}{ChatID: chatID, MessageID: messageID, Text: htmlText, ParseMode: "HTML"})
	if err != nil && strings.Contains(err.Error(), "message is not modified") {
		return nil
	}
	return err
}

// EditMessageTextMarkup edits messageID in chatID in place with new
// HTML text AND a replacement inline keyboard. It is EditMessageText
// with a reply_markup: pass an empty InlineKeyboardMarkup to REMOVE
// the keyboard. That is what an expired approval card needs —
// without it the dead card keeps live-looking Approve/Deny buttons,
// and a tap on them (answered only with a transient "expired"
// toast) looks exactly like a broken button (the rc8 incident:
// the user tapped Approve on a card whose request had already
// timed out). The "message is not modified" tolerance applies
// exactly as in EditMessageText.
func (g *TelegramGateway) EditMessageTextMarkup(ctx context.Context, chatID, messageID int64, htmlText string, markup InlineKeyboardMarkup) error {
	_, err := g.call(ctx, "editMessageText", nil, struct {
		ChatID      int64                `json:"chat_id"`
		MessageID   int64                `json:"message_id"`
		Text        string               `json:"text"`
		ParseMode   string               `json:"parse_mode"`
		ReplyMarkup InlineKeyboardMarkup `json:"reply_markup"`
	}{ChatID: chatID, MessageID: messageID, Text: htmlText, ParseMode: "HTML", ReplyMarkup: markup})
	if err != nil && strings.Contains(err.Error(), "message is not modified") {
		return nil
	}
	return err
}

// BotCommand is one entry of the bot's /commands menu.
type BotCommand struct {
	Name        string `json:"command"`
	Description string `json:"description"`
}

// SetMyCommands replaces the bot's command menu (the list shown when
// the user types "/" in Telegram).
func (g *TelegramGateway) SetMyCommands(ctx context.Context, cmds []BotCommand) error {
	if cmds == nil {
		cmds = []BotCommand{}
	}
	_, err := g.call(ctx, "setMyCommands", nil, struct {
		Commands []BotCommand `json:"commands"`
	}{Commands: cmds})
	return err
}

// splitMessage splits text into chunks of at most MaxMessageLength runes,
// so multi-byte characters are never split mid-rune.
func splitMessage(text string) []string {
	runes := []rune(text)
	if len(runes) <= MaxMessageLength {
		return []string{text}
	}
	chunks := make([]string, 0, len(runes)/MaxMessageLength+1)
	for len(runes) > MaxMessageLength {
		chunks = append(chunks, string(runes[:MaxMessageLength]))
		runes = runes[MaxMessageLength:]
	}
	if len(runes) > 0 {
		chunks = append(chunks, string(runes))
	}
	return chunks
}

// SendMessage sends text to chatID. Text longer than 4096 characters is
// split into multiple messages, in order. An empty text is a no-op
// returning nil. The first failing chunk aborts the send and its error
// is returned wrapped with the chunk index.
func (g *TelegramGateway) SendMessage(ctx context.Context, chatID int64, text string) error {
	if text == "" {
		return nil
	}
	for i, chunk := range splitMessage(text) {
		if err := g.sendMessageOnce(ctx, chatID, chunk); err != nil {
			return fmt.Errorf("messaging: telegram sendMessage chunk %d: %w", i, err)
		}
	}
	return nil
}

// fetchUpdatesLocked long-polls for new updates (timeout=30s on the
// server side, 45s client deadline: window + 15s grace) and returns
// them WITHOUT advancing the offset. The caller holds fetchMu and must advance
// the offset for every update it has decided (completed, dropped,
// or skipped — F-A8-2: completion, not dispatch) — see
// advanceOffsetTo and PollStream's contiguous confirmation. It
// honours ctx cancellation.
//
// The first SUCCESSFUL fetch also carries allowed_updates
// (F-A16-2): Telegram persists that filter server-side, so sending
// it once resets a stale filter left by an earlier run (the rc8
// incident: a persisted ["message"] filter meant callback_query
// was never delivered and approval buttons silently died). The
// filter rides on the ordinary long poll rather than a separate
// pre-loop call: no extra round-trip, no update is consumed by a
// throwaway call (the offset is not advanced by this call either
// way, so a discarded batch would be re-fetched — but there is no
// reason to waste the round-trip), and a failed first fetch simply
// retries the filter on the next fetch, because
// updatesFilterSent is set only on success. A failed filter can
// therefore never fail startup or stop polling: the fetch's own
// error handling (transient backoff in PollStream) already covers
// it, and polling continues with the filter unset until a fetch
// gets through.
//
// Splitting the fetch from the advance is what closes the
// crash-window loss (F-A7-P2-3): GetUpdates used to advance the
// offset the moment updates arrived, before any of them was
// dispatched, so a daemon dying mid-batch confirmed
// fetched-but-undispatched updates server-side and they were lost.
// A caller that never advances re-fetches the same updates on the
// next poll instead — at-least-once, never lost.
func (g *TelegramGateway) fetchUpdatesLocked(ctx context.Context) ([]Update, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	// Bound the long poll even if the caller supplied a client without a
	// timeout. WithTimeout never extends a shorter caller deadline: the
	// earliest deadline wins. The bound is the per-attempt deadline
	// (window + grace, 45s by default) — not the 30s window, so a
	// slow-but-healthy response arriving just after the window is not
	// killed and misread as a dead connection. A zero pollTimeout (a
	// Gateway built without the constructor) falls back to the default
	// instead of an instant deadline.
	pollTimeout := g.pollTimeout
	if pollTimeout <= 0 {
		pollTimeout = pollHTTPTimeout
	}
	pollCtx, cancel := context.WithTimeout(ctx, pollTimeout)
	defer cancel()

	g.mu.Lock()
	offset := g.offset
	g.mu.Unlock()

	query := url.Values{}
	query.Set("offset", strconv.FormatInt(offset, 10))
	query.Set("timeout", strconv.Itoa(getUpdatesTimeout))
	// F-A16-2: subscribe to message + callback_query updates on
	// the first fetch (see the method comment). Only while the
	// filter has not yet been sent successfully; every later
	// fetch is a plain long poll.
	sendFilter := !g.updatesFilterSent
	if sendFilter {
		allowed, err := json.Marshal(allowedUpdates)
		if err != nil {
			// Unreachable in practice (a []string always
			// marshals); if it ever happened, poll without
			// the filter rather than failing the fetch.
			g.logf("telegram poll: allowed_updates filter could not be encoded (%v); polling without it", err)
			sendFilter = false
		} else {
			query.Set("allowed_updates", string(allowed))
		}
	}

	result, err := g.call(pollCtx, "getUpdates", query, nil)
	if err != nil {
		return nil, err
	}
	var updates []Update
	if err := json.Unmarshal(result, &updates); err != nil {
		return nil, fmt.Errorf("messaging: telegram getUpdates: decode result: %w", err)
	}
	if updates == nil {
		updates = []Update{}
	}
	if sendFilter {
		// The filter landed: this fetch succeeded, so Telegram
		// has the subscription. Never send it again on this
		// gateway (once per startup).
		g.updatesFilterSent = true
		g.logf("telegram poll: allowed_updates filter set to [message callback_query]")
	}
	// A completed poll — empty counts — moves the watchdog baseline
	// and clears the consecutive-timeout streak.
	g.notePollCompleted()
	return updates, nil
}

// advanceOffsetTo moves the offset to next when next is ahead of it.
// The offset only ever moves forward: a stale or duplicate advance
// is a no-op. It is the single place the offset changes.
func (g *TelegramGateway) advanceOffsetTo(next int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if next > g.offset {
		g.offset = next
	}
}

// advanceOffset moves the offset past every update in updates (to
// max(update_id) + 1), so the next call only returns newer updates.
// It is GetUpdates' whole-batch confirmation: every fetched update
// is decided by the caller at once, so all of them are confirmed
// together.
func (g *TelegramGateway) advanceOffset(updates []Update) {
	if len(updates) == 0 {
		return
	}
	var maxID int64 = updates[0].UpdateID
	for _, u := range updates[1:] {
		if u.UpdateID > maxID {
			maxID = u.UpdateID
		}
	}
	g.advanceOffsetTo(maxID + 1)
}

// GetUpdates long-polls for new updates (timeout=30s on the server side,
// 45s client deadline). After a successful call the internal offset is
// advanced to max(update_id) + 1, so the next call only returns newer
// updates. It honours ctx cancellation.
//
// Serialise whole polls (see fetchMu): the offset read, the
// request, and the offset advance are one atomic step, so two
// concurrent callers can never fetch the same offset and deliver
// the same updates twice. A second caller waits here for the
// in-flight long poll (up to ~45s) instead of duplicating it.
// PollStream does NOT use this method: it fetches with
// fetchUpdatesLocked and confirms per dispatched update instead,
// so a crash mid-batch cannot confirm updates it never dispatched.
func (g *TelegramGateway) GetUpdates(ctx context.Context) ([]Update, error) {
	// Serialise whole polls (see fetchMu): the offset read, the
	// request, and the offset advance are one atomic step, so two
	// concurrent callers can never fetch the same offset and deliver
	// the same updates twice. A second caller waits here for the
	// in-flight long poll (up to ~35s) instead of duplicating it.
	g.fetchMu.Lock()
	defer g.fetchMu.Unlock()

	updates, err := g.fetchUpdatesLocked(ctx)
	if err != nil {
		return nil, err
	}
	g.advanceOffset(updates)
	return updates, nil
}

// maxPollHandlers bounds how many update handlers run at once. One
// slow handler (an agent run can take minutes) must not stall polling
// for other chats, but unbounded goroutines would let an update flood
// exhaust memory; 8 in flight with per-chat ordering (below) is the
// balance.
const maxPollHandlers = 8

// cardEditThrottle is the minimum interval between in-place edits of
// the terminal card: Telegram rate-limits edits, so bursts of tool
// events are coalesced and only the latest render is sent.
const cardEditThrottle = time.Second

// typingResendInterval re-sends the "typing" chat action this often
// while a turn runs: Telegram shows it for only a few seconds.
const typingResendInterval = 4 * time.Second

// PollStream loops on GetUpdates until ctx is cancelled, delivering
// each incoming message with non-empty text to handler and sending the
// handler's reply back to the same chat, with a live Hermes-style
// terminal card rendered from the handler's progress events.
//
//   - Before the handler runs, a "typing" chat action is sent and
//     re-sent every 4s until the handler returns.
//   - The terminal card is created lazily on the first ToolCallStarted
//     event and edited in place on later events, throttled to at most
//     one edit per second (bursts coalesce; a final edit is always
//     done when the handler returns, if the render changed since the
//     last successful edit). A failed edit falls back to sending a
//     fresh card message and editing that one instead. A turn with no
//     tool calls sends no card at all.
//   - A handler error is reported to the user as
//     "Sorry, I hit an error: <err>" (redacted, split/capped by
//     SendMessage); polling continues. If a card exists, it gets a
//     final edit first so the card shows the failed state.
//   - When the handler returns stats != nil, the footer from
//     FormatFooter(stats) is appended to the reply; nil stats means
//     no footer. An empty reply sends nothing.
//   - A transient GetUpdates error backs off (1s, doubling to a 5s max)
//     and polling continues. A fatal one (401/403/409: revoked token,
//     blocked bot, another poller) returns an error instead.
//   - ctx cancellation returns nil cleanly.
//   - A handler panic is reported to the chat like a handler error
//     and polling continues; it never kills the daemon.
//
// Updates are handled concurrently, bounded by maxPollHandlers, with
// best-effort per-chat ordering (striped locks). When ctx is cancelled
// PollStream returns nil promptly; handlers already running are left
// to finish (their reply sends use the cancelled ctx and fail
// best-effort, as before).
//
// Each batch is dispatched in two passes (F-A7-P2-3): all callback
// queries first, then messages. A message earlier in a batch must
// never delay a tap later in that batch behind a turn slot.
// The getUpdates offset advances only as updates complete,
// are dropped, or are skipped — and only past the longest
// contiguous run of decided updates — so a crash mid-turn
// re-delivers the in-flight update instead of confirming and
// losing it (F-A8-2: confirmation happens on handler
// completion, never at spawn).
// isVisionError reports whether err looks like a provider
// rejecting image input (F-A17-10): the model/provider cannot
// take images, and the provider says so with a 400/bad-request
// that mentions images, vision, or multimodal input. Both halves
// must match — a bare 400 (malformed request, over-long context)
// is a different failure and must keep its own error text, and
// an "image" mention without a rejection is not an error at all.
func isVisionError(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	mentionsImage := strings.Contains(s, "image") ||
		strings.Contains(s, "vision") ||
		strings.Contains(s, "multimodal") ||
		strings.Contains(s, "picture") ||
		strings.Contains(s, "photo")
	if !mentionsImage {
		return false
	}
	return strings.Contains(s, "400") ||
		strings.Contains(s, "bad request") ||
		strings.Contains(s, "not support") ||
		strings.Contains(s, "unsupported") ||
		strings.Contains(s, "invalid")
}

// userErrorMessage renders a handler error for the chat: a vision
// rejection becomes the honest cannot-see message instead of the
// raw provider "status 400" text (F-A17-10); anything else keeps
// the redacted "Sorry, I hit an error" form.
func userErrorMessage(err error) string {
	if isVisionError(err) {
		return CannotSeeImagesMessage
	}
	return security.RedactSecrets(fmt.Sprintf("Sorry, I hit an error: %v", err))
}

// visionSupported reports whether the chat's current provider/model
// can see images (model.SupportsVision). No modelInfo installed, or
// an unknown pair, means unsupported — fail closed, never pretend.
func (g *TelegramGateway) visionSupported(chatID int64) bool {
	if g == nil || g.modelInfo == nil {
		return false
	}
	provider, modelName := g.modelInfo(chatID)
	return model.SupportsVision(provider, modelName)
}

// errPhotoTooLarge marks a download refused for exceeding
// MaxPhotoBytes, so handlePhoto can answer with the too-large
// message instead of the generic download-failed one.
var errPhotoTooLarge = errors.New("messaging: telegram photo exceeds MaxPhotoBytes")

// downloadPhoto fetches one Telegram photo by file_id: getFile
// resolves the file path, then the bytes are downloaded from the
// file host ({baseURL}/file/bot<token>/<path>) — the same base URL
// the gateway already uses, so tests can point it at a fake server
// with WithBaseURL. Only the bytes are returned; the MIME type is
// sniffed from them by the caller (never trusted from Telegram).
// The download is capped at MaxPhotoBytes: a larger photo is
// refused with errPhotoTooLarge before its bytes are held in
// memory. The token never appears in a returned error or log line:
// the URL is built with PathEscape like call() does, and errors
// name only the method.
func (g *TelegramGateway) downloadPhoto(ctx context.Context, fileID string) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	result, err := g.call(ctx, "getFile", url.Values{"file_id": {fileID}}, nil)
	if err != nil {
		return nil, fmt.Errorf("messaging: telegram getFile: %w", err)
	}
	var file struct {
		FilePath string `json:"file_path"`
	}
	if err := json.Unmarshal(result, &file); err != nil {
		return nil, fmt.Errorf("messaging: telegram getFile: decode response: %w", err)
	}
	if strings.TrimSpace(file.FilePath) == "" {
		return nil, errors.New("messaging: telegram getFile: empty file_path")
	}
	endpoint := fmt.Sprintf("%s/file/bot%s/%s", g.baseURL, url.PathEscape(g.token), strings.TrimPrefix(file.FilePath, "/"))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("messaging: telegram download photo: %w", err)
	}
	client := g.client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("messaging: telegram download photo: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("messaging: telegram download photo: HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxPhotoBytes+1))
	if err != nil {
		return nil, fmt.Errorf("messaging: telegram download photo: read response: %w", err)
	}
	if len(data) > MaxPhotoBytes {
		return nil, errPhotoTooLarge
	}
	return data, nil
}

func (g *TelegramGateway) PollStream(ctx context.Context, handler TurnFunc) error {
	if handler == nil {
		return errors.New("messaging: telegram PollStream: handler must not be nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	backoff := time.Second
	const maxBackoff = 5 * time.Second

	// Poll-recovery baseline: the watchdog counts from loop start until
	// the first completed poll (see notePollLoopStarted).
	g.notePollLoopStarted()

	// Semaphore bounding concurrent handlers; striped locks serialise
	// handlers for the same chat (best-effort per-chat ordering).
	sem := make(chan struct{}, maxPollHandlers)
	// cbSem bounds concurrent callback handlers SEPARATELY from
	// turns (rc11 W7): a callback query (an inline-keyboard tap) is
	// a millisecond Resolve, but a turn handler holds its sem slot
	// for the whole turn — including a 90s approval wait and
	// multi-minute model runs. When callbacks shared the turn
	// semaphore, all 8 slots held meant the poll loop blocked
	// acquiring a slot for a tap and the callback was never
	// dispatched: zero tap lines, gate timed out, polling stalled
	// for every chat (the live rc10 incident, cards 245 and 253).
	// Callbacks must never wait behind turns.
	cbSem := make(chan struct{}, maxPollHandlers)
	// handlers tracks the spawned reply handlers (callback, photo,
	// media) that log via g.logf (rc25, found by the -race run:
	// TestW4's callback handler logged "context canceled" AFTER
	// PollStream returned, racing TestDaemonDebugEndpointEnabled's
	// capture() swap of the global os.Stderr — g.logf reads that
	// global at call time). PollStream waits for these handlers
	// before returning (defer handlers.Wait()), so no handler logs
	// after return. The turn handler (handle) is NOT tracked: it
	// runs the full turn and never calls g.logf, so it cannot race
	// on os.Stderr, and waiting for it would delay shutdown for
	// the whole turn.
	var handlers sync.WaitGroup
	defer handlers.Wait()
	var chatLocks [64]sync.Mutex
	chatLock := func(chatID int64) *sync.Mutex {
		return &chatLocks[uint64(chatID)%uint64(len(chatLocks))]
	}

	// Offset confirmation tracker (F-A8-2). The offset is a single
	// watermark and advances ONLY when a handler COMPLETES — never
	// at spawn. rc12 confirmed an update the moment its handler
	// goroutine was spawned, so the next getUpdates re-issued with
	// the advanced offset confirmed the in-flight turn
	// server-side; a crash/restart any time during a minutes-long
	// turn then lost the update permanently (never re-delivered,
	// no reply). Four silent VM reboots happened on 2026-11-13,
	// so this was live risk, not theory.
	//
	// Semantics (at-least-once):
	//   - "Decided" means the handler finished (reply sent, or
	//     callback answered, or panic recovered and apology
	//     sent, or photo answered with the cannot-see reply) or
	//     the update was deliberately dropped/skipped
	//     (non-allowlisted sender/chat, empty text with no
	//     photo). An update
	//     whose handler is still running is NOT decided, and the
	//     watermark stays behind it — and behind every later
	//     update too, because the watermark only advances past
	//     the longest contiguous run of decided updates.
	//   - A re-delivered update (after a crash mid-turn Telegram
	//     re-delivers every unconfirmed update for 24h) is
	//     re-dispatched and the turn runs again — but its tools
	//     do NOT re-run blindly (F-A9-1): the handler carries
	//     the update id in its context (see WithUpdateID below)
	//     and the agent loop replays recorded tool results from
	//     the durable tool journal instead of re-executing them
	//     (see core.ToolJournal). Subagent lifecycle tools are
	//     excluded from the journal — their results reference
	//     in-process state that does not survive a restart — so
	//     they re-execute against the live Manager, and the
	//     run-1 child is dead with the old process, so no work
	//     is duplicated. A re-delivered turn may still send a
	//     second reply when the killed run had already replied
	//     but was not yet confirmed (the confirmation window
	//     is microseconds, so this is rare) — the journal
	//     dedups tool execution, not replies. What IS
	//     guaranteed: a turn is never dispatched twice
	//     CONCURRENTLY — inFlight dedupes re-fetched updates
	//     while their handler is still running, and the
	//     per-chat lock serialises a re-run against any turn
	//     for the same chat.
	//   - If a handler blocks forever, the watermark stays
	//     behind it forever: later updates are still fetched
	//     and dispatched (the poll loop does not stall behind
	//     one turn), but they are never confirmed past it. The
	//     loop re-fetches the in-flight update on every poll;
	//     when a fetch makes no progress at all (every update
	//     in it was already in-flight) the loop waits for a
	//     completion signal (up to 1s) instead of spinning a
	//     tight re-fetch loop. This is the at-least-once price,
	//     and it is bounded: at most one extra getUpdates per
	//     second per stuck turn.
	//   - The offset is in-memory, so on daemon restart it
	//     starts from 0 and Telegram re-delivers everything
	//     unconfirmed within its 24h window. A re-delivered
	//     turn that had already completed before the crash is
	//     confirmed again only after its re-run completes —
	//     same semantics as above, no special case.
	var confirmMu sync.Mutex
	nextConfirm := g.Offset()
	decided := make(map[int64]bool)
	inFlight := make(map[int64]bool)
	// logged tracks update ids whose "received update" line was
	// already emitted (F-A23-POLL). The watermark only advances
	// when a handler completes, so while a turn runs every poll
	// re-fetches the in-flight batch — and the old code logged
	// every re-fetch, so one 5-minute turn produced 639 lines
	// for 8 distinct updates (update 181487784 alone 243x).
	// The diagnostic value ("did getUpdates return this
	// update?") is fully carried by the first line; re-logging
	// it adds only noise that buries real signal. Pruned with
	// decided, against the same fetch offset, so it stays
	// bounded. Only the poll loop touches it (see logOnce).
	logged := make(map[int64]bool)
	// completed is signalled (non-blocking, depth 1) on every
	// markDecided, so the poll loop can wake promptly from its
	// no-progress wait when a handler finishes.
	completed := make(chan struct{}, 1)
	notifyCompleted := func() {
		select {
		case completed <- struct{}{}:
		default:
		}
	}
	// markDecided records update id as finished and advances the
	// watermark past the longest contiguous run of decided
	// updates starting at nextConfirm. It is called only from a
	// handler's defer (on completion) or synchronously for a
	// deliberately dropped/skipped update — never at spawn.
	//
	// NOTE: markDecided deliberately does NOT delete decided
	// entries as the watermark passes them. A getUpdates call
	// issued with an older offset can still be in flight (a
	// long poll lasts up to 30s) while handlers complete and
	// the watermark advances; when that stale batch returns,
	// it still contains the already-decided updates, and the
	// dispatch loop must recognise them as decided to skip
	// them instead of running those turns a second time. The
	// entries are pruned only in the poll loop, against the
	// offset the batch being processed was fetched with (see
	// below): an id below that offset can never be returned
	// again, by this batch or any later one.
	markDecided := func(id int64) {
		confirmMu.Lock()
		decided[id] = true
		delete(inFlight, id)
		for decided[nextConfirm] {
			nextConfirm++
		}
		next := nextConfirm
		confirmMu.Unlock()
		g.advanceOffsetTo(next)
		notifyCompleted()
	}
	// isDecided reports whether update id already finished (or
	// was deliberately dropped/skipped) but is still waiting
	// behind the watermark for an earlier in-flight update.
	// Such an update is re-fetched on every poll until the
	// watermark passes it — it must be skipped, not dispatched
	// again, or a fast turn behind a slow one would re-run on
	// every poll cycle.
	isDecided := func(id int64) bool {
		confirmMu.Lock()
		defer confirmMu.Unlock()
		return decided[id]
	}
	// markInFlight claims update id for a handler that is about
	// to be spawned; it returns false when a handler for id is
	// already running (a re-fetch of an in-flight update), in
	// which case the caller must NOT dispatch it again.
	markInFlight := func(id int64) bool {
		confirmMu.Lock()
		defer confirmMu.Unlock()
		if inFlight[id] {
			return false
		}
		inFlight[id] = true
		return true
	}
	// unmarkInFlight releases a claim made by markInFlight when
	// the handler was never spawned (semaphore acquisition lost
	// to ctx cancellation), so the update can be dispatched on
	// the next poll instead of staying claimed forever.
	unmarkInFlight := func(id int64) {
		confirmMu.Lock()
		defer confirmMu.Unlock()
		delete(inFlight, id)
	}
	// logOnce reports whether update id's receipt line still
	// needs emitting, claiming it when it does: the first receipt
	// of an id logs, every re-fetch of the same id (in-flight or
	// decided-but-behind-the-watermark) is silent (F-A23-POLL).
	// Delivery semantics are untouched — this only gates the log
	// line; dispatch, isDecided/markInFlight and the watermark
	// are unchanged.
	logOnce := func(id int64) bool {
		confirmMu.Lock()
		defer confirmMu.Unlock()
		if logged[id] {
			return false
		}
		logged[id] = true
		return true
	}
	// noteBatchStart handles the first update of a fetched batch
	// starting above the watermark (a gap that cannot occur on
	// the real API, where update_ids are contiguous): the run
	// starts at that update instead of stalling forever below
	// it — unless an in-flight update sits below it, in which
	// case the watermark must stay behind that update.
	noteBatchStart := func(firstID int64) {
		confirmMu.Lock()
		jumped := false
		if firstID > nextConfirm {
			jumped = true
			for id := range inFlight {
				if id < firstID {
					jumped = false
					break
				}
			}
			if jumped {
				nextConfirm = firstID
			}
		}
		next := nextConfirm
		confirmMu.Unlock()
		// Advancing the offset to nextConfirm is safe: the
		// offset is inclusive (getUpdates returns updates with
		// id >= offset), so pointing it at the lowest undecided
		// update still re-delivers that update — it confirms
		// nothing. It keeps Offset() meaningful (the next
		// unconfirmed update id) while a turn is in-flight,
		// instead of leaving it at its pre-batch value.
		if jumped {
			g.advanceOffsetTo(next)
		}
	}

	// handleMedia answers a non-photo media message (F-A18-9):
	// the honest cannot-process reply for its kind, never a turn
	// (the model never received the attachment) and never
	// silence. It mirrors handlePhoto's confirmation discipline
	// (markDecided on completion, never at spawn) and takes the
	// per-chat lock, so a media reply cannot overtake a turn for
	// the same chat. The send is best-effort, like every other
	// reply: a failed send must not kill polling.
	handleMedia := func(upd Update, kind string) {
		// Registered first, so it runs LAST (rc25; see
		// handlePhoto).
		defer handlers.Done()
		defer markDecided(upd.UpdateID)
		defer func() { <-sem }()
		lock := chatLock(upd.Message.Chat.ID)
		lock.Lock()
		defer lock.Unlock()
		g.logf("telegram poll: %s from chat %d answered with cannot-process reply (no %s support)", kind, upd.Message.Chat.ID, kind)
		_ = g.SendMessage(ctx, upd.Message.Chat.ID, cannotProcessMediaMessage(kind))
	}

	// handleTurn runs one turn for an update: the body shared by the
	// text path (handle) and the photo path (handlePhoto, when the
	// chat's model supports vision). text is the user text — the
	// message text, or a photo's caption (or the default prompt
	// when the photo has no caption) — and images is nil for a
	// text-only turn. It owns the per-chat lock and the panic
	// recovery; the caller owns markDecided and the semaphore slot
	// (see handle / handlePhoto). A photo turn never answers a
	// pending ask_user_question: replyChainTryAnswer reads the
	// message text, which is empty for a photo, so it returns
	// false and the photo runs as a normal turn.
	handleTurn := func(upd Update, text string, images []model.ImageContent) {
		// A panic in the handler (the agent loop) must not kill the
		// daemon: report it to the chat like any other handler error
		// and keep polling.
		defer func() {
			if r := recover(); r != nil {
				var msg string
				if rerr, ok := r.(error); ok && isVisionError(rerr) {
					msg = CannotSeeImagesMessage
				} else {
					msg = fmt.Sprintf("Sorry, I hit an error: %v", r)
					// Redact before sending: a handler panic value
					// could carry a provider/config error with a key.
					msg = security.RedactSecrets(msg)
				}
				// Best effort: a failed apology must not kill polling.
				_ = g.SendMessage(ctx, upd.Message.Chat.ID, msg)
			}
		}()
		// Dispatch-time stamp (busy state + FIFO ticket), read
		// once and deleted here: the handler reads the stamp
		// instead of looking the state up live (see
		// stampDispatch). Every return path below — a consumed
		// reply-chain answer, a steered message, a normal turn,
		// a panic — releases the ticket, so the queue can never
		// stall behind a handle that left without running a
		// turn. The release runs in its own goroutine: it waits
		// for this ticket's turn in the queue (an early return
		// behind a still-running turn still releases in order),
		// and the handle itself must not block on it — its
		// semaphore slot and its offset confirmation (the defers
		// above) are due at once, not after earlier queued turns
		// finish.
		stamp, stamped := g.dispatchStampFor(upd.UpdateID)
		if stamped {
			defer func() {
				go g.ticketQueueFor(upd.Message.Chat.ID).release(stamp.ticket)
			}()
		}
		// A text message that answers a pending ask_user_question
		// in this chat is consumed as the answer (no turn, no
		// lock): the turn that asked is blocked waiting for it and
		// holds the chat's lock, so routing the reply through a
		// turn would deadlock until the question timed out. The
		// defers above (markDecided, semaphore release) still run
		// on this early return, so the update is confirmed.
		if g.replyChainTryAnswer(upd) {
			return
		}
		chatID := upd.Message.Chat.ID

		// Busy-input handling (before the per-chat lock): a new
		// message arriving while this chat's turn runs gets an
		// immediate ack and its mode's action (see busy.go).
		// Commands never trigger it — they flow to the lock wait
		// as before, so /stop and /busy sent mid-turn are not
		// mistaken for new input.
		//
		// The check reads the spawn-time stamp, not a live
		// lookup: the poll loop dispatches in batch order, but
		// this handler goroutine can start late — after the run
		// it arrived during ended and the next turn registered
		// — and a live lookup would then ack against the fresh
		// state. The stamp pins the arrival-time run; the ack is
		// still coalesced per state (see sendBusyAck).
		if !isBusyExemptCommand(upd.Message.Text) {
			if st := stamp.busy; st != nil {
				mode := g.busyModeFor(chatID)
				switch mode {
				case BusyModeQueue:
					g.sendBusyAck(ctx, chatID, st, mode, false)
				case BusyModeSteer:
					if g.steerFor(chatID, upd.Message.Text, upd.UpdateID) {
						// Accepted into the running turn: no
						// turn of its own. The defers above
						// (markDecided, semaphore release) still
						// run, so the update is confirmed
						// exactly once.
						g.sendBusyAck(ctx, chatID, st, mode, false)
						return
					}
					// Not accepted: the message queues behind
					// the lock like any other turn, so the ack
					// it gets is the queue ack — never the
					// steer ack, which would claim it joined
					// the run when it did not.
					g.sendBusyAck(ctx, chatID, st, BusyModeQueue, false)
				default: // interrupt
					if g.planPendingFor(chatID) {
						// Degraded to queue: cancelling a turn
						// that is waiting on a plan decision
						// would orphan the plan card, so the
						// message waits behind it instead.
						g.sendBusyAck(ctx, chatID, st, mode, true)
					} else {
						// Cancel any pending question FIRST, with
						// a visible card edit: the run is about
						// to be cancelled, and the wait must be
						// resolved by the card edit — not by the
						// run's own ctx-cancellation cleanup,
						// which would leave no edit behind.
						g.cancelQuestionForChat(chatID)
						// Cancel the run, then ack (F-A23W3-12):
						// the ack must only claim a redirect
						// when the actual running turn was
						// cancelled. A stamped state can
						// outlive its run (this handler started
						// late, after the turn finished): the
						// dead ctx cancels nothing, and the
						// message simply runs as its own turn
						// — no ack, never the redirect claim.
						if st.cancelRun() {
							g.sendBusyAck(ctx, chatID, st, mode, false)
						}
					}
				}
			}
		}
		// FIFO ticket (see ticketQueue): wait for this update's
		// turn in the queue before taking the per-chat lock, so
		// queued messages run in dispatch (arrival) order. The
		// lock alone cannot guarantee that — two handles spawned
		// in the same batch race to it in goroutine-scheduling
		// order. The wait sits after the busy check on purpose:
		// a reply-chain answer and a steered message never wait
		// (they return above), and an interrupt cancels the
		// running turn first, so the chain unblocks.
		if stamped {
			g.ticketQueueFor(chatID).wait(stamp.ticket)
		}
		lock := chatLock(chatID)
		lock.Lock()
		defer lock.Unlock()

		// --- terminal card state (this turn only) ---
		var cardMu sync.Mutex
		// busySt is this turn's busy-input state, registered
		// just before the handler runs (below). emit reads it
		// for the ack detail; it is nil until registration.
		var busySt *busyState
		card := &TerminalCard{}
		var cardMsgID int64 // 0 until the card is created
		var lastSent string // last rendered text successfully shown
		var lastEdit time.Time
		// syncCard renders the card and, when it changed since the
		// last successful show, pushes it to Telegram: creating the
		// card on first use, editing it in place otherwise, and
		// sending a fresh card when an edit fails. force ignores the
		// throttle (used for the final edit when the handler
		// returns). It is best-effort: every failure is swallowed
		// so a card problem never fails the turn.
		syncCard := func(force bool) {
			cardMu.Lock()
			defer cardMu.Unlock()
			rendered := card.Render()
			if rendered == lastSent {
				return
			}
			// Never create a card for a turn with no tool calls:
			// the final sync after the handler returns would
			// otherwise send a bare header (lastSent starts as ""
			// and differs from the header).
			if cardMsgID == 0 && card.IsEmpty() {
				return
			}
			if !force && !lastEdit.IsZero() && time.Since(lastEdit) < cardEditThrottle {
				return // coalesce bursts; the final edit catches up
			}
			if cardMsgID == 0 {
				id, err := g.SendMessageHTML(ctx, chatID, rendered)
				if err != nil {
					return
				}
				cardMsgID = id
			} else if err := g.EditMessageText(ctx, chatID, cardMsgID, rendered); err != nil {
				// The message may have been deleted: send a fresh
				// card and keep editing that one instead.
				id, err2 := g.SendMessageHTML(ctx, chatID, rendered)
				if err2 != nil {
					return
				}
				cardMsgID = id
			}
			lastSent = rendered
			lastEdit = time.Now()
		}
		emit := func(ev core.ProgressEvent) {
			// The Add is wrapped so a panic inside it still
			// releases cardMu (F-A9-P2-7): the deferred final
			// card edit must be able to take the lock on the
			// panic path. The unlock stays before syncCard —
			// syncCard takes cardMu itself.
			func() {
				cardMu.Lock()
				defer cardMu.Unlock()
				card.Add(ev)
			}()
			// The busy ack's live detail (current tool, step)
			// comes from the same events: a later arrival's ack
			// names what the run is doing right now.
			if busySt != nil {
				busySt.noteEvent(ev)
			}
			// Tool-call and compaction events can change the card;
			// other kinds (step/turn) are ignored by
			// TerminalCard.Add anyway. A compaction event alone
			// creates the card: a turn that auto-compacts (or
			// warns) with no tool calls still shows the user what
			// happened, via the final syncCard(true) below at the
			// latest.
			if ev.Kind == core.EventToolCallStarted || ev.Kind == core.EventToolCallFinished ||
				ev.Kind == core.EventCompactionWarning || ev.Kind == core.EventAutoCompacted {
				syncCard(false)
			}
		}

		// "typing" while the turn runs, re-sent every 4s: Telegram
		// shows a chat action for only a few seconds.
		//
		// stopTyping closes typingDone and does the final card
		// edit. It is a sync.OnceFunc registered as a defer
		// (F-A9-P2-7): the old code closed typingDone on the
		// normal return path only, so a handler panic unwound
		// past it to the recover defer and the typing goroutine
		// leaked — one goroutine plus one Telegram POST every
		// 4s per panicked turn, until daemon restart. The
		// explicit stopTyping() call after the handler returns
		// runs it on the normal path (before the reply, as
		// before); the defer runs it on the panic path, and
		// the Once makes the second call a no-op.
		typingDone := make(chan struct{})
		stopTyping := sync.OnceFunc(func() {
			close(typingDone)
			// Final card edit: show the finished state
			// (including any failed tool call) even if the
			// last event was inside the throttle window.
			// syncCard is a no-op when the card was never
			// created or nothing changed.
			syncCard(true)
		})
		defer stopTyping()
		go func() {
			ticker := time.NewTicker(typingResendInterval)
			defer ticker.Stop()
			for {
				select {
				case <-typingDone:
					return
				case <-ticker.C:
					_ = g.SendChatAction(ctx, chatID, "typing")
				}
			}
		}()
		_ = g.SendChatAction(ctx, chatID, "typing")

		// The handler runs with the sender in its context (P1-O4):
		// the turn handler reads it for the owner tier and the
		// permission gate's approval context. A nil From cannot
		// reach here — the sender check above drops it.
		senderID := int64(0)
		if upd.Message.From != nil {
			senderID = upd.Message.From.ID
		}
		handlerCtx := WithSenderID(ctx, senderID)
		// The update id travels with the turn (F-A9-1): the
		// agent loop reads it from the context and replays
		// recorded tool results from the durable tool journal
		// instead of re-executing them, so a turn killed
		// mid-execution and re-delivered after a restart runs
		// each tool exactly once. Callbacks carry no turn and
		// get no update id.
		handlerCtx = core.WithUpdateID(handlerCtx, upd.UpdateID)
		// The turn runs on its own cancellable context: a later
		// arrival in interrupt mode cancels exactly this turn
		// (the provider call aborts via the step ctx), and the
		// daemon's turn handler suppresses the cancelled turn's
		// reply, so the old turn sends nothing. The busy state
		// is registered here — after the lock, just before the
		// handler runs — and unregistered by defer when the
		// handler finishes (LIFO: unregister runs before the
		// lock is released, so the next turn never sees a stale
		// state, and a new arrival in the microsecond teardown
		// window sees no busy run and simply waits on the lock).
		turnCtx, turnCancel := context.WithCancel(handlerCtx)
		defer turnCancel()
		busySt = g.registerBusy(chatID, turnCtx, turnCancel)
		defer g.unregisterBusy(chatID, busySt)
		reply, stats, herr := handler(turnCtx, chatID, text, images, emit)
		stopTyping()

		// sendTurnResult is the reply tail shared by the main
		// turn and every leftover turn below: an error becomes
		// the apology (a vision rejection renders as the honest
		// cannot-see message, not the raw provider 400 —
		// F-A17-10), an empty reply sends nothing, otherwise
		// media delivery runs on the model reply BEFORE the
		// footer is appended, so the footer can never contain a
		// MEDIA tag and the tag strip can never eat footer text.
		// The delivery report is appended AFTER the footer, so
		// it is the last thing the user reads. The error path
		// never gets media treatment: an apology must not
		// trigger a file send. Best effort throughout: a failed
		// send never fails the turn or polling.
		sendTurnResult := func(reply string, stats *core.TurnStats, herr error) {
			if herr != nil {
				msg := userErrorMessage(herr)
				_ = g.SendMessage(ctx, chatID, msg)
				return
			}
			if reply == "" {
				return
			}
			reply, report := g.deliverMedia(ctx, chatID, reply)
			if stats != nil {
				reply += "\n\n" + FormatFooter(stats)
			}
			if report != "" {
				reply += "\n\n" + report
			}
			_ = g.SendMessage(ctx, chatID, reply)
		}

		// Steered leftovers: messages the run did not consume
		// (they arrived during the final provider call, or after
		// the step budget ran out) run here as fresh turns
		// through the same handler, card and emit — before the
		// main reply is sent and before stopTyping, so the
		// typing indicator and card stay live for them.
		// Re-drain after each leftover turn: steers can land
		// during one. Each leftover carries its own update id
		// in ctx (tool-journal identity) and runs on the
		// PollStream ctx, not the turn ctx: an interrupt has
		// already cancelled the turn ctx by the time leftovers
		// run, and a leftover must not die with the run it
		// outlived. Each leftover runs on its own cancellable
		// ctx, whose cancel is registered in the busy state
		// (F-A23W3-12): an interrupt arriving during a leftover
		// must cancel the leftover that is actually running,
		// not the finished main turn's dead ctx — and the
		// redirect ack is only honest when that cancel lands.
		// The hook drains (returns and clears), so a
		// well-behaved hook can never make this loop spin.
		for {
			leftovers := g.steerLeftoversFor(chatID)
			if len(leftovers) == 0 {
				break
			}
			for _, lf := range leftovers {
				lfCtx, lfCancel := context.WithCancel(core.WithUpdateID(handlerCtx, lf.UpdateID))
				// Leftover steers are text-only by construction
				// (core.SteeredMessage carries Text + UpdateID,
				// no images — photos never enter the steer path),
				// so the images argument is nil.
				busySt.setCancel(lfCtx, lfCancel)
				lreply, lstats, lerr := handler(lfCtx, chatID, lf.Text, nil, emit)
				lfCancel()
				sendTurnResult(lreply, lstats, lerr)
			}
		}
		stopTyping()
		sendTurnResult(reply, stats, herr)
	}

	// handle runs one plain-text turn: the text path into
	// handleTurn. It owns the confirmation discipline (markDecided
	// on completion, never at spawn — F-A8-2) and the semaphore
	// slot; handleTurn owns the lock, the card and the recovery.
	handle := func(upd Update) {
		// Registered first, so it runs LAST (LIFO): after the
		// reply/apology is sent, after the panic recovery, and
		// after the semaphore slot is released. The update is
		// confirmed only once this handler has fully completed
		// (F-A8-2) — never at spawn.
		defer markDecided(upd.UpdateID)
		defer func() { <-sem }()
		handleTurn(upd, upd.Message.Text, nil)
	}

	// handlePhoto answers a photo message (F-A17-9). Two paths:
	//   - The chat's provider/model supports vision: the largest
	//     size is downloaded and the photo runs as a real turn
	//     (handleTurn) with the caption as the text — or the
	//     default Bengali "describe this photo" prompt when there
	//     is no caption — and the image bytes attached. Never a
	//     turn on the caption alone (the model would answer about
	//     an image it never saw: confabulation).
	//   - Anything else (non-vision model, or no modelInfo
	//     installed): the honest Bengali cannot-see reply, which
	//     names the fix (DeepSeek deepseek-flash). Never silence.
	// A download that fails, or a photo over MaxPhotoBytes, gets
	// its own honest Bengali reply instead of a turn. It mirrors
	// handle's confirmation discipline (markDecided on completion,
	// never at spawn). The download runs outside the per-chat lock
	// (it can take seconds); handleTurn takes the lock for the
	// turn, and the fallback sends take it for the reply, so a
	// photo reply still cannot overtake a turn for the same chat.
	// Every send is best-effort, like every other reply: a failed
	// send must not kill polling.
	handlePhoto := func(upd Update) {
		// Registered first, so it runs LAST: the handler is
		// fully done (reply sent, update confirmed, slot
		// released) before PollStream's wait returns (rc25).
		defer handlers.Done()
		defer markDecided(upd.UpdateID)
		defer func() { <-sem }()
		chatID := upd.Message.Chat.ID
		if !g.visionSupported(chatID) {
			lock := chatLock(chatID)
			lock.Lock()
			defer lock.Unlock()
			g.logf("telegram poll: photo from chat %d answered with cannot-see reply (model has no vision support)", chatID)
			_ = g.SendMessage(ctx, chatID, CannotSeeImagesMessage)
			return
		}
		// Telegram sends the largest size last.
		sizes := upd.Message.Photo
		largest := sizes[len(sizes)-1]
		data, err := g.downloadPhoto(ctx, largest.FileID)
		if err != nil {
			msg := PhotoDownloadFailedMessage
			if errors.Is(err, errPhotoTooLarge) {
				msg = PhotoTooLargeMessage
			}
			g.logf("telegram poll: photo from chat %d could not be downloaded: %v", chatID, err)
			lock := chatLock(chatID)
			lock.Lock()
			defer lock.Unlock()
			_ = g.SendMessage(ctx, chatID, msg)
			return
		}
		mime := model.SniffImageMIME(data)
		if mime == "" {
			// Telegram said "photo" but the bytes are none of
			// the formats a vision model accepts: say so
			// honestly instead of sending unusable bytes.
			g.logf("telegram poll: photo from chat %d has an unrecognised image format, not sent to the model", chatID)
			lock := chatLock(chatID)
			lock.Lock()
			defer lock.Unlock()
			_ = g.SendMessage(ctx, chatID, PhotoDownloadFailedMessage)
			return
		}
		text := strings.TrimSpace(upd.Message.Caption)
		if text == "" {
			text = PhotoDefaultPrompt
		}
		g.logf("telegram poll: photo from chat %d (%d bytes, %s) running as a vision turn", chatID, len(data), mime)
		handleTurn(upd, text, []model.ImageContent{{Data: data, MIMEType: mime}})
	}

	// handleCallback answers one callback query (an inline-keyboard
	// tap). It runs OUTSIDE the per-chat lock on purpose: a tap
	// resolves a pending approval while the turn that requested it
	// is still blocked waiting for the answer, and that turn holds
	// the chat's lock — handling the tap under the same lock would
	// deadlock the approval round-trip. A handler panic or error is
	// logged and polling continues; the query is always answered
	// (even when no handler is installed) so the button's spinner
	// clears instead of hanging.
	handleCallback := func(q TGCallbackQuery, updateID int64) {
		// handlers.Done is registered first, so it runs LAST:
		// the handler is fully done (query answered, update
		// confirmed, slot released) before PollStream's wait
		// returns (rc25).
		defer handlers.Done()
		// markDecided runs second-to-last (LIFO): after the
		// query is answered (or the tap failed and was logged),
		// after the panic recovery, and after the cbSem slot is
		// released. The update is confirmed only once this
		// handler has fully completed (F-A8-2) — never at spawn.
		defer markDecided(updateID)
		defer func() { <-cbSem }()
		defer func() {
			if r := recover(); r != nil {
				g.logf("telegram poll: callback handler panicked: %v", r)
				_ = g.AnswerCallbackQuery(ctx, q.ID, "Sorry, that tap failed.")
			}
		}()
		senderID := int64(0)
		if q.From != nil {
			senderID = q.From.ID
		}
		cbCtx := WithSenderID(ctx, senderID)
		h := g.callbackHandlerFor()
		if h == nil {
			_ = g.AnswerCallbackQuery(ctx, q.ID, "This button is not handled right now.")
			return
		}
		if err := h(cbCtx, q); err != nil {
			g.logf("telegram poll: callback handler error: %v", err)
			_ = g.AnswerCallbackQuery(ctx, q.ID, "Sorry, that tap failed.")
		}
	}

	// consecutiveFailures counts back-to-back transient getUpdates
	// failures, so the recovery log can say how long polling was
	// down. It is only ever read by this loop.
	consecutiveFailures := 0
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		// Outer watchdog (backstop for the N=3 trigger below): if a
		// full watchdog interval has passed with no completed poll,
		// force the same transport rebuild even if the
		// consecutive-timeout count did not trip — e.g. attempts
		// failing fast. The rebuild resets the watchdog baseline,
		// so it cannot fire again until another full interval
		// passes without a completed poll.
		if g.pollWatchdogDue(time.Now()) {
			g.rebuildPollTransport("watchdog: no completed poll")
		}

		// Fetch WITHOUT advancing the offset (F-A7-P2-3, F-A8-2):
		// the offset advances only as updates complete below
		// (see markDecided), so a crash mid-batch or mid-turn
		// cannot confirm updates this loop never finished.
		// fetchMu is held only
		// for the fetch itself: the dispatch loop below can block
		// on the turn semaphore, and holding the fetch lock across
		// it would stall any concurrent GetUpdates caller for the
		// same time. Production has a single poller, so no second
		// fetcher can observe the un-advanced offset mid-batch;
		// GetUpdates callers keep their own atomic
		// fetch-and-advance (see GetUpdates).
		// fetchOffset is the offset this batch was requested
		// with (fetchUpdatesLocked reads the same offset
		// immediately after, so the request's offset is >=
		// this value). It bounds the decided-set pruning
		// below: any decided id below it can never be
		// returned again, by this batch or any later one,
		// because the offset only ever moves forward.
		fetchOffset := g.Offset()
		g.fetchMu.Lock()
		updates, err := g.fetchUpdatesLocked(ctx)
		g.fetchMu.Unlock()
		// Prune decided entries that can never be re-fetched
		// (F-A8-2 stale-batch hole): a getUpdates issued with
		// an older offset can return after the watermark has
		// already advanced past some of its updates, so those
		// updates must stay recognisable as decided until a
		// batch fetched with a newer offset is processed —
		// pruning against this batch's own fetch offset is
		// exactly that point. Pruning earlier (e.g. in
		// markDecided as the watermark passed them) let a
		// stale batch re-dispatch already-finished turns.
		confirmMu.Lock()
		for id := range decided {
			if id < fetchOffset {
				delete(decided, id)
			}
		}
		for id := range logged {
			if id < fetchOffset {
				delete(logged, id)
			}
		}
		confirmMu.Unlock()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			// A revoked token, a blocked bot, or a second poller
			// holding the connection (401/403/409) can never heal
			// by retrying: fail loudly so the daemon exits (and
			// systemd / the operator notices) instead of looking
			// alive while polling a dead token every few seconds.
			var apiErr *apiError
			if errors.As(err, &apiErr) && apiErr.isFatalForPoll() {
				return fmt.Errorf("messaging: telegram poll: unrecoverable Bot API error: %w", apiErr)
			}
			// Transient failure: log it (when a logger is set) and
			// back off. Without this line a network outage is
			// completely silent in the daemon log — the process
			// looks alive while polling is down. The error is
			// safe to log: call() redacts the token.
			consecutiveFailures++
			g.logf("telegram poll: transient getUpdates error (attempt %d, retry in %s): %v", consecutiveFailures, backoff, err)
			// Deterministic recovery from a black-holed pooled
			// connection (live incident 2026-11-15): on the 3rd
			// CONSECUTIVE getUpdates TIMEOUT — timeout-class error
			// only (see isTimeoutError), never 429/5xx — rebuild
			// the poll transport with a fresh dialer between
			// attempts (under fetchMu) and reset the streak. A
			// non-timeout transient error proves the connection
			// answered, so it breaks the streak instead.
			if isTimeoutError(err) {
				if timeouts := g.notePollTimeout(); timeouts >= pollRebuildAfterTimeouts {
					g.rebuildPollTransport(fmt.Sprintf("%d consecutive getUpdates timeouts", timeouts))
				}
			} else {
				g.resetPollTimeouts()
			}
			g.setPollBackoff(true)
			select {
			case <-ctx.Done():
				g.setPollBackoff(false)
				return nil
			case <-time.After(backoff):
			}
			g.setPollBackoff(false)
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
			continue
		}
		if consecutiveFailures > 0 {
			g.logf("telegram poll: recovered after %d failed getUpdates attempt(s)", consecutiveFailures)
			consecutiveFailures = 0
		}
		backoff = time.Second

		// Every received update is logged first, in arrival order
		// (id + kind + sender / chat ids only — never message text
		// or callback data): the rc10 incident was undiagnosable
		// precisely because nothing recorded whether getUpdates
		// ever RETURNED the tap. With this line the next incident
		// reads either "received update N callback_query" (the
		// loss is downstream of the poll) or nothing at all (the
		// loss is upstream: another poller confirmed it, or
		// Telegram never delivered it). Each id logs ONCE, at
		// first receipt (F-A23-POLL, see logOnce): while a turn
		// runs, every poll re-fetches the in-flight batch, and
		// logging each re-fetch buried real signal under
		// hundreds of duplicate lines.
		for _, upd := range updates {
			if logOnce(upd.UpdateID) {
				g.logReceivedUpdate(upd)
			}
		}

		// Confirmation state (F-A8-2, see the tracker above
		// handle): markDecided runs only when a handler
		// COMPLETES, never at spawn. The old comment here
		// claimed the safe direction held generally — "a turn
		// killed mid-flight by the crash runs again" — but the
		// probe in Audit 8 falsified that: the offset was
		// confirmed at dispatch, so a turn killed mid-flight
		// was NEVER re-delivered. What is true now: a decided
		// update is one whose handler finished, or one this
		// loop dropped or skipped on purpose; an in-flight
		// update is undecided and is re-fetched on the next
		// poll (deduped, not re-dispatched — see
		// markInFlight). A DECIDED update behind the watermark
		// is re-fetched too, and is likewise skipped, not
		// re-dispatched (see isDecided): without that, a fast
		// turn behind a slow one would re-run on every poll.
		// The offset is a single watermark, so
		// it may only pass an update once every earlier update
		// is decided too: a callback later in the batch may
		// complete while an earlier message's turn is still
		// running, but the watermark stays behind that message
		// — a crash now re-delivers the suffix instead of
		// losing it. Telegram delivers update_ids
		// contiguously, so the run always reaches the batch
		// end once every update is decided.
		if len(updates) > 0 {
			noteBatchStart(updates[0].UpdateID)
		}
		// progress counts updates this batch newly decided or
		// newly dispatched. Zero progress with a non-empty batch
		// means every update in it was already in-flight: the
		// loop waits for a completion (below) instead of
		// re-fetching the same in-flight updates in a tight
		// loop.
		progress := 0

		// Pass 1 — callbacks first (F-A7-P2-3, in-batch ordering).
		// Callback queries (inline-keyboard taps) are handled on
		// their own path, outside the per-chat lock (see
		// handleCallback): a tap must be able to resolve a pending
		// approval while the turn that requested it holds the
		// chat's lock. The sender is checked the same way as for
		// messages — a tap from a non-allowlisted user can never
		// approve anything. The callback slot comes from cbSem,
		// NOT the turn semaphore (see above): a tap is dispatched
		// even when every turn slot is held.
		//
		// The passes exist because the loop used to iterate a
		// batch sequentially: a message earlier in the same batch
		// blocked the loop on sem before a callback later in that
		// batch was reached, so a tap could still wait behind a
		// message dispatch while all turn slots were held (up to
		// the 90s approval window — a same-batch tap could still
		// expire). Dispatching every callback before any message
		// closes that ordering hole; the message pass below can
		// block on sem without delaying a tap.
		dispatchCallback := func(upd Update) bool {
			q := upd.CallbackQuery
			// Already finished but still behind the watermark:
			// re-fetched, not new — skip without re-answering.
			if isDecided(upd.UpdateID) {
				return true
			}
			if !g.IsSenderAllowed(q.From) {
				if q.From != nil {
					g.logf("telegram poll: dropped callback query from non-allowlisted user %d", q.From.ID)
				} else {
					g.logf("telegram poll: dropped callback query with no sender")
				}
				markDecided(upd.UpdateID)
				progress++
				return true
			}
			if ctx.Err() != nil {
				return false
			}
			// In-flight dedup (F-A8-2): the offset stays behind
			// a running callback handler, so the next fetch
			// re-delivers it — skip it instead of answering
			// the same tap twice. Not progress: the loop must
			// still wait for this handler to finish.
			if !markInFlight(upd.UpdateID) {
				return true
			}
			select {
			case cbSem <- struct{}{}:
			case <-ctx.Done():
				unmarkInFlight(upd.UpdateID)
				return false
			}
			handlers.Add(1)
			go handleCallback(*q, upd.UpdateID)
			// No markDecided here: the update is confirmed
			// only when handleCallback completes (its defer).
			progress++
			return true
		}

		// Pass 2 — messages, in batch order. A message with no
		// text and no photo is skipped (decided: there is nothing
		// to deliver — a sticker, voice note or location, none of
		// which Orion handles); a message WITH a photo is answered
		// with the honest cannot-see reply (F-A17-9, see
		// handlePhoto), never silently dropped and never run as a
		// turn on its caption. Anything else needs a turn slot,
		// so this pass is the one that blocks when all slots are
		// held. Blocking here is safe now: every callback in the
		// batch is already dispatched, and the offset still sits
		// behind any undispatched or in-flight message (see
		// markDecided), so a crash or a shutdown mid-pass
		// re-delivers the rest.
		dispatchMessage := func(upd Update) bool {
			// Already finished but still behind the watermark:
			// re-fetched, not new — skip without re-running the
			// turn (a fast turn behind a slow one would
			// otherwise re-run on every poll cycle).
			if isDecided(upd.UpdateID) {
				return true
			}
			if upd.Message == nil {
				markDecided(upd.UpdateID)
				progress++
				return true
			}
			// A photo takes precedence over the text turn: a
			// photo message's caption lives in "caption", not
			// "text", so Text is empty for a captioned photo —
			// and a photo WITH text still shows an image the
			// model may not see, so answering the text while
			// ignoring the photo would mislead. handlePhoto
			// decides: a vision-capable model gets the photo
			// (caption as the text), anything else gets the
			// honest Bengali cannot-see reply. Non-photo media
			// (F-A18-9) is the same: a voice note, video,
			// document, sticker or location the model never
			// received must be answered honestly, never run as
			// a turn on its (empty) text and never dropped
			// silently. Both branches below run after the
			// allowlist/sender checks, so a stranger's media
			// gets no reply.
			hasPhoto := upd.Message.HasPhoto()
			mediaKind := upd.Message.MediaKind()
			if strings.TrimSpace(upd.Message.Text) == "" && !hasPhoto && mediaKind == "" {
				markDecided(upd.UpdateID)
				progress++
				return true
			}
			// Sender authorisation: drop messages from chats outside
			// the allowlist before the handler (and the agent, and
			// its tools) ever sees them. The drop is logged (when a
			// logger is set) with the chat ID, so an operator who
			// has not allowlisted themselves yet can find their
			// chat ID in the daemon log and add it to
			// telegram_allowed_chats. The text is never logged.
			if !g.IsChatAllowed(upd.Message.Chat.ID) {
				g.logf("telegram poll: dropped message from non-allowlisted chat %d (add it to telegram_allowed_chats to allow it)", upd.Message.Chat.ID)
				markDecided(upd.UpdateID)
				progress++
				return true
			}
			// Sender check (P1-O4): the chat being allowlisted is
			// not enough — the SENDER must be allowlisted too. In
			// a group chat this stops non-allowlisted members
			// driving the bot through the group's allowlisting;
			// a message with no sender is dropped outright.
			if !g.IsSenderAllowed(upd.Message.From) {
				if upd.Message.From != nil {
					g.logf("telegram poll: dropped message from non-allowlisted user %d in chat %d", upd.Message.From.ID, upd.Message.Chat.ID)
				} else {
					g.logf("telegram poll: dropped message with no sender in chat %d", upd.Message.Chat.ID)
				}
				markDecided(upd.UpdateID)
				progress++
				return true
			}
			// Photo (F-A17-9): handlePhoto decides — a vision
			// turn when the chat's model can see images, the
			// honest Bengali cannot-see reply otherwise. The
			// sender is already authorised (above); the reply
			// goes through the same in-flight/sem discipline as
			// a turn, so a crash mid-reply re-delivers the photo
			// instead of confirming and losing it.
			if hasPhoto {
				if !markInFlight(upd.UpdateID) {
					return true
				}
				select {
				case sem <- struct{}{}:
				case <-ctx.Done():
					unmarkInFlight(upd.UpdateID)
					return false
				}
				handlers.Add(1)
				go handlePhoto(upd)
				progress++
				return true
			}
			// Non-photo media (F-A18-9): answer honestly instead
			// of running a turn. The sender is already
			// authorised (above); the reply goes through the same
			// in-flight/sem discipline as a turn, so a crash
			// mid-reply re-delivers the media instead of
			// confirming and losing it. A media message WITH
			// text still takes this branch — answering the text
			// while ignoring the attachment would mislead, as
			// with a photo.
			if mediaKind != "" {
				if !markInFlight(upd.UpdateID) {
					return true
				}
				select {
				case sem <- struct{}{}:
				case <-ctx.Done():
					unmarkInFlight(upd.UpdateID)
					return false
				}
				handlers.Add(1)
				go handleMedia(upd, mediaKind)
				progress++
				return true
			}
			if ctx.Err() != nil {
				return false
			}
			// In-flight dedup (F-A8-2): the offset stays behind
			// a running turn, so the next fetch re-delivers it
			// — skip it instead of running the same turn twice
			// concurrently. Not progress: the loop must still
			// wait for this handler to finish.
			if !markInFlight(upd.UpdateID) {
				return true
			}
			// Acquire a handler slot (or stop if cancelled) before
			// dispatching, so the number of in-flight handlers is
			// bounded even under an update flood.
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				unmarkInFlight(upd.UpdateID)
				return false
			}
			// Busy-input snapshot + FIFO ticket (see
			// stampDispatch): record which run, if any, this
			// update arrived during, and the ticket that orders
			// its turn behind earlier updates from this chat.
			// The handler reads the stamp instead of looking
			// the state up live, so a late-starting handler
			// cannot ack against a newer turn's fresh state or
			// take the lock ahead of an earlier update. Taken
			// HERE, after the in-flight claim and the semaphore
			// acquisition — a re-fetched in-flight update (or a
			// dispatch aborted at the semaphore by ctx
			// cancellation) never spawns a handle, and a ticket
			// taken for it would never be released, stalling
			// the queue behind a handle that does not exist.
			g.stampDispatch(upd.UpdateID, upd.Message.Chat.ID)
			go handle(upd)
			// No markDecided here: the update is confirmed only
			// when handle completes (its defer, F-A8-2).
			progress++
			return true
		}

		for _, upd := range updates {
			if upd.CallbackQuery == nil {
				continue
			}
			if !dispatchCallback(upd) {
				return nil
			}
		}
		for _, upd := range updates {
			if upd.CallbackQuery != nil {
				continue
			}
			if !dispatchMessage(upd) {
				return nil
			}
		}

		// No-progress wait (F-A8-2): every update in this batch
		// was already in-flight, so the next fetch would return
		// the same updates immediately and the loop would spin.
		// Wait for a handler to complete (waking promptly on
		// the completion signal) or up to 1s, whichever comes
		// first, before polling again. A handler that blocks
		// forever holds the watermark behind it — that is the
		// at-least-once semantics — but it costs at most one
		// extra getUpdates per second, and later updates are
		// still fetched and dispatched on each cycle.
		if len(updates) > 0 && progress == 0 {
			select {
			case <-completed:
			case <-time.After(time.Second):
			case <-ctx.Done():
				return nil
			}
		}
	}
}

// Poll loops on GetUpdates until ctx is cancelled, delivering each
// incoming message with non-empty text to handler and sending the
// handler's reply back to the same chat. A photo message from a
// chat whose model has no vision support is answered with the
// cannot-see reply, and a non-photo media message (sticker, voice,
// video, document, location) with its cannot-process reply, inside
// PollStream; neither reaches the handler. But a photo from a
// vision-capable chat DOES reach PollStream's handler with its
// image — and Poll's handler is text-only, so Poll FAILS CLOSED on
// it: the turn is rejected with CannotSeeImagesMessage and the
// handler is never called, rather than running a caption-only turn
// (F-V1: the model would answer about an image it never saw).
// Poll is therefore a text-only API — use PollStream + TurnFunc
// for image turns. It is a thin wrapper over PollStream: the
// handler receives no progress events and no footer is appended
// (stats nil). All PollStream semantics apply — backoff on
// transient GetUpdates errors, fatal 401/403/409, allowlist,
// per-chat ordering, panic recovery, and nil on ctx cancellation.
func (g *TelegramGateway) Poll(ctx context.Context, handler func(ctx context.Context, chatID int64, text string) (string, error)) error {
	if handler == nil {
		return errors.New("messaging: telegram Poll: handler must not be nil")
	}
	return g.PollStream(ctx, func(ctx context.Context, chatID int64, text string, images []model.ImageContent, emit func(core.ProgressEvent)) (string, *core.TurnStats, error) {
		if len(images) > 0 {
			// F-V1: Poll's handler cannot receive images, so
			// never run a caption-only turn. Reaching here
			// with images means vision IS supported (a
			// non-vision photo is answered inside
			// PollStream before the handler is reached), so
			// this reply is about Poll's limitation, not the
			// model's — but it is the one honest reply for a
			// photo the caller cannot see, and the guard is
			// unreachable for callers that use PollStream.
			return CannotSeeImagesMessage, nil, nil
		}
		reply, err := handler(ctx, chatID, text)
		return reply, nil, err
	})
}
