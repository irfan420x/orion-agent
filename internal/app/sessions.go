// Per-session transcripts at the app layer (audit P0 #4).
//
// Each chat (and the CLI's global session) has one live session at
// a time, identified by a session id and backed by one append-only
// JSONL transcript under the sessions directory:
//
//	<DataDir>/chats/<chatID>/sessions/<session-id>.jsonl  (Telegram)
//	<DataDir>/sessions/<session-id>.jsonl                 (CLI)
//
// The live session id is recorded in sessions/current.json. /new
// (NewSessionForChat) rotates to a fresh session — the old
// transcript file stays on disk, listed by /sessions and resumable
// by /resume. Resuming loads the transcript's messages back into
// the working history (bounded by the store's MaxMessages cap —
// the transcript itself keeps every turn) and re-points the live
// session and the agent's transcript sink at it.
//
// Migration: the first time a transcript is opened over a store
// that already holds turns (a pre-transcript memory.json), those
// turns are imported once into the transcript (see
// session.Transcript.MigrateFrom) and never deleted from
// memory.json. Facts and typed memories are untouched — they stay
// shared, exactly as the P1-O3 boundary requires.
package app

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/irfan/orion/internal/memory"
	"github.com/irfan/orion/internal/session"
)

// openOrCreateTranscript opens the live transcript for root (a
// chat directory, or the data dir for the CLI), creating a fresh
// session id when none is recorded. When the transcript is empty
// and store already holds turns, those turns are imported once
// (migration — see the package doc); importedAt stamps them with
// the legacy history file's modification time. It returns the
// transcript and the live session id.
func openOrCreateTranscript(root string, store memory.Store, legacyFile string, log *log.Logger) (*session.Transcript, string, error) {
	id, err := session.ReadCurrent(root)
	if err != nil {
		return nil, "", fmt.Errorf("app: read current session: %w", err)
	}
	if id == "" || !session.ValidateID(id) {
		id = session.NewID()
		if err := session.WriteCurrent(root, id); err != nil {
			return nil, "", fmt.Errorf("app: record current session: %w", err)
		}
	}
	tr, err := session.Open(root, id)
	if err != nil {
		return nil, "", fmt.Errorf("app: open session transcript: %w", err)
	}
	// Migration is resumable (Audit 5 F-A5-P2-4): attempt it
	// whenever the store holds turns, not only on an empty
	// transcript — a transcript holding only Migrated records
	// imports the remainder, and a live transcript imports
	// nothing. A failed import no longer costs the whole
	// transcript: the transcript is live from here on, and the
	// next open resumes the remainder.
	if store != nil {
		if msgs, err := store.RecentMessages(0); err == nil && len(msgs) > 0 {
			importedAt := time.Now().UTC()
			if st, err := os.Stat(legacyFile); err == nil {
				importedAt = st.ModTime().UTC()
			}
			if n, err := tr.MigrateFrom(msgs, importedAt); err != nil {
				if log != nil {
					log.Printf("app: migration into session %s incomplete (%d imported): %v; remainder will resume on next open", id, n, err)
				}
			} else if n > 0 {
				// Migration is worth one log line: it is the
				// only time old history moves anywhere.
				if log != nil {
					log.Printf("app: migrated %d turn(s) from %s into session %s", n, legacyFile, id)
				}
			}
		}
	}
	return tr, id, nil
}

// rotateTranscript starts a fresh live session under root: a new
// id is recorded, its (empty) transcript opened, and the old
// transcript left on disk for listing/resume. It returns the new
// transcript and id.
func rotateTranscript(root string) (*session.Transcript, string, error) {
	id := session.NewID()
	if err := session.WriteCurrent(root, id); err != nil {
		return nil, "", fmt.Errorf("app: record new session: %w", err)
	}
	tr, err := session.Open(root, id)
	if err != nil {
		return nil, "", fmt.Errorf("app: open new session transcript: %w", err)
	}
	return tr, id, nil
}

// ListSessionsForChat lists one chat's sessions, newest first,
// with the live session id. A chat with no data dir has no
// sessions on disk (its history is ephemeral).
func (a *App) ListSessionsForChat(chatID int64) ([]session.Info, string, error) {
	cs, err := a.chatSessionFor(chatID)
	if err != nil {
		return nil, "", err
	}
	if cs.dir == "" {
		return nil, cs.sessionID, nil
	}
	infos, err := session.List(cs.dir)
	if err != nil {
		return nil, "", err
	}
	return infos, cs.sessionID, nil
}

// CurrentSessionIDForChat returns one chat's live session id
// ("" when the chat has no data dir and no session yet).
func (a *App) CurrentSessionIDForChat(chatID int64) (string, error) {
	cs, err := a.chatSessionFor(chatID)
	if err != nil {
		return "", err
	}
	return cs.sessionID, nil
}

// ResumeReport describes what a /resume actually restored, so
// the caller can report it instead of silently dropping it
// (Audit 5 F-A5-P2-5): corrupt lines in the transcript, a
// transcript longer than the store's cap, and summaries loaded
// into the shared fact. The last report per chat (or global) is
// kept for LastResumeReportForChat / LastResumeReport — the
// /resume reply text is rendered by the messaging layer, which
// reads the report from there.
type ResumeReport struct {
	SessionID          string
	TranscriptMessages int  // messages in the transcript (pre-cap)
	LoadedMessages     int  // messages now in working history (capped)
	CorruptLines       int  // unparseable transcript lines skipped
	SummariesLoaded    int  // transcript summaries merged into the fact
	Capped             bool // transcript held more than MaxMessages
}

var resumeReportsMu sync.Mutex
var resumeReports = map[string]ResumeReport{}

func recordResumeReport(key string, r ResumeReport) {
	resumeReportsMu.Lock()
	defer resumeReportsMu.Unlock()
	resumeReports[key] = r
}

func lastResumeReport(key string) (ResumeReport, bool) {
	resumeReportsMu.Lock()
	defer resumeReportsMu.Unlock()
	r, ok := resumeReports[key]
	return r, ok
}

// LastResumeReportForChat returns the last resume outcome for one
// chat, if any resume has run for it.
func (a *App) LastResumeReportForChat(chatID int64) (ResumeReport, bool) {
	return lastResumeReport(fmt.Sprintf("chat:%d", chatID))
}

// LastResumeReport returns the last resume outcome for the
// CLI/global session, if any resume has run for it.
func (a *App) LastResumeReport() (ResumeReport, bool) {
	return lastResumeReport("global")
}

// maxResumeSummaryFactRunes caps the shared conversation-summary
// fact after merging resumed summaries, mirroring core's cap.
const maxResumeSummaryFactRunes = 8000

// LoadResumedSession loads a transcript's messages into store and
// its summaries into the shared conversation-summary fact, and
// reports the outcome. The transcript keeps every turn; the
// working history is bounded by the store's MaxMessages cap, and
// the report says so instead of silently dropping the excess.
// Summaries that exist only in the transcript are merged into the
// fact (deduplicated — a summary already there is not added
// twice), so they reach the model through fact recall instead of
// being invisible after resume. Corrupt lines are counted, not
// fatal.
func LoadResumedSession(store memory.Store, tr *session.Transcript, sessionID string, logger *log.Logger) (ResumeReport, error) {
	rep := ResumeReport{SessionID: sessionID}
	msgs, err := tr.Messages()
	if err != nil {
		return rep, fmt.Errorf("app: resume: read transcript: %w", err)
	}
	rep.TranscriptMessages = len(msgs)
	rep.CorruptLines = tr.CorruptLines()
	rep.Capped = len(msgs) > memory.MaxMessages
	if rep.Capped && logger != nil {
		logger.Printf("app: resume: transcript %s holds %d messages; working history capped at %d, oldest %d turn(s) stay in the transcript only", sessionID, len(msgs), memory.MaxMessages, len(msgs)-memory.MaxMessages)
	}
	if rep.CorruptLines > 0 && logger != nil {
		logger.Printf("app: resume: transcript %s has %d corrupt line(s) skipped", sessionID, rep.CorruptLines)
	}
	if err := store.ReplaceMessages(msgs); err != nil {
		return rep, fmt.Errorf("app: resume: load history: %w", err)
	}
	if loaded, err := store.RecentMessages(0); err == nil {
		rep.LoadedMessages = len(loaded)
	} else {
		rep.LoadedMessages = len(msgs)
		if rep.LoadedMessages > memory.MaxMessages {
			rep.LoadedMessages = memory.MaxMessages
		}
	}
	if sums, err := tr.Summaries(); err == nil && len(sums) > 0 {
		n, err := mergeResumedSummaries(store, sessionID, sums)
		if err != nil {
			if logger != nil {
				logger.Printf("app: resume: transcript summaries for %s could not be merged into the fact: %v", sessionID, err)
			}
		} else {
			rep.SummariesLoaded = n
		}
	}
	return rep, nil
}

// mergeResumedSummaries merges a resumed session's transcript
// summaries into the shared conversation-summary fact, one dated
// section per summary, skipping summaries already present. The
// fact is shared across a chat's sessions, so a resumed section
// is headed with its session id; the fact is trimmed to the cap
// by dropping the oldest sections first.
func mergeResumedSummaries(store memory.Store, sessionID string, summaries []string) (int, error) {
	if len(summaries) == 0 {
		return 0, nil
	}
	existing, found, err := store.GetFact("conversation-summary")
	if err != nil {
		return 0, err
	}
	combined := ""
	if found {
		combined = strings.TrimSpace(existing.Value)
	}
	added := 0
	stamp := time.Now().UTC().Format("2006-01-02 15:04:05 UTC")
	for _, s := range summaries {
		s = strings.TrimSpace(s)
		if s == "" || strings.Contains(combined, s) {
			continue
		}
		section := "## Resumed " + sessionID + " " + stamp + "\n" + s
		if combined == "" {
			combined = section
		} else {
			combined += "\n\n" + section
		}
		added++
	}
	if added == 0 {
		return 0, nil
	}
	combined = trimResumeSummaryFact(combined, maxResumeSummaryFactRunes)
	return added, store.AddFact("conversation-summary", combined)
}

// trimResumeSummaryFact drops the oldest "## <...>" sections until
// the fact fits within maxRunes, newest kept. It mirrors
// core.trimSummaryFact: text before the first header is its own
// oldest section.
func trimResumeSummaryFact(combined string, maxRunes int) string {
	if len([]rune(combined)) <= maxRunes {
		return combined
	}
	var sections []string
	var cur strings.Builder
	for _, line := range strings.SplitAfter(combined, "\n") {
		if strings.HasPrefix(line, "## ") && cur.Len() > 0 {
			sections = append(sections, strings.TrimRight(cur.String(), "\n"))
			cur.Reset()
		}
		cur.WriteString(line)
	}
	if cur.Len() > 0 {
		sections = append(sections, strings.TrimRight(cur.String(), "\n"))
	}
	if len(sections) == 0 {
		return combined
	}
	for len(sections) > 1 {
		total := 0
		for _, s := range sections {
			total += len([]rune(s)) + 2
		}
		if total <= maxRunes {
			break
		}
		sections = sections[1:]
	}
	if len(sections) == 1 {
		if r := []rune(sections[0]); len(r) > maxRunes {
			return string(r[len(r)-maxRunes:])
		}
	}
	return strings.Join(sections, "\n\n")
}

// ResumeSessionForChat resumes one chat's named session: the id
// may be a full id or a unique leading prefix. The transcript's
// messages replace the working history (bounded by the store's
// cap — the transcript keeps every turn), the live session
// pointer moves to it, and the chat's agent records further turns
// into it. It returns the resolved id and the message count now
// in working history. Resuming the live session is a no-op that
// reports its message count. Facts and typed memories are shared
// and untouched by a resume. The resume outcome (corrupt lines,
// capping, summaries loaded) is recorded for
// LastResumeReportForChat.
func (a *App) ResumeSessionForChat(chatID int64, idOrPrefix string) (string, int, error) {
	cs, err := a.chatSessionFor(chatID)
	if err != nil {
		return "", 0, err
	}
	if cs.dir == "" {
		return "", 0, errors.New("app: resume needs a data dir (this chat's history is ephemeral)")
	}
	full, err := session.ResolvePrefix(cs.dir, idOrPrefix)
	if err != nil {
		return "", 0, err
	}
	if full == cs.sessionID {
		msgs, err := cs.store.RecentMessages(0)
		if err != nil {
			return "", 0, fmt.Errorf("app: resume: read history: %w", err)
		}
		return full, len(msgs), nil
	}
	tr, err := session.Open(cs.dir, full)
	if err != nil {
		return "", 0, fmt.Errorf("app: resume: open transcript: %w", err)
	}
	rep, err := LoadResumedSession(cs.store, tr, full, a.Logger)
	if err != nil {
		return "", 0, err
	}
	if err := session.WriteCurrent(cs.dir, full); err != nil {
		return "", 0, fmt.Errorf("app: resume: record current session: %w", err)
	}
	a.chatsMu.Lock()
	oldTr := cs.transcript
	cs.transcript = tr
	cs.sessionID = full
	agent := cs.agent
	a.chatsMu.Unlock()
	if agent != nil {
		// A resumed session starts with a clean compaction
		// slate: the breaker state belonged to the previous
		// session's history.
		agent.SetTranscript(tr)
		agent.ResetCompactionBreaker()
	}
	// The old transcript is no longer the live one: release its
	// lock/fd (a later resume back to it reopens it).
	if oldTr != nil && oldTr != tr {
		_ = oldTr.Close()
	}
	recordResumeReport(fmt.Sprintf("chat:%d", chatID), rep)
	return full, rep.LoadedMessages, nil
}

// CompactionStatusForChat returns one chat's agent's
// auto-compaction status, for /status. A chat with no agent yet
// has never compacted (zero status, nil error).
func (a *App) CompactionStatusForChat(chatID int64) (compactionStatus, error) {
	cs, err := a.chatSessionFor(chatID)
	if err != nil {
		return compactionStatus{}, err
	}
	a.chatsMu.Lock()
	agent := cs.agent
	a.chatsMu.Unlock()
	if agent == nil {
		return compactionStatus{}, nil
	}
	st := agent.CompactionStatus()
	return compactionStatus{
		Pressure:              st.Pressure,
		Warning:               st.Warning,
		Tripped:               st.Tripped,
		ConsecutiveFailures:   st.ConsecutiveFailures,
		LastError:             st.LastError,
		Compactions:           st.Compactions,
		DroppedWithoutSummary: st.DroppedWithoutSummary,
		LastDroppedAt:         st.LastDroppedAt,
	}, nil
}

// compactionStatus is the app-level view of core.CompactionStatus
// (messaging renders it; it carries no core import in its shape).
type compactionStatus struct {
	Pressure              float64
	Warning               bool
	Tripped               bool
	ConsecutiveFailures   int
	LastError             string
	Compactions           int
	DroppedWithoutSummary int
	LastDroppedAt         time.Time
}

// wireGlobalTranscript opens the CLI/global session's transcript
// (under the data dir) and wires it into the global agent, with a
// one-time migration of any pre-transcript global history. It is
// best-effort: a transcript failure is logged and the agent runs
// without one, exactly as before — the daemon must never fail to
// start over a transcript. Called once from Build.
func (a *App) wireGlobalTranscript() {
	if a == nil || a.Config == nil || a.Config.DataDir == "" || a.Agent == nil || a.Memory == nil {
		return
	}
	tr, id, err := openOrCreateTranscript(a.Config.DataDir, a.Memory, filepath.Join(a.Config.DataDir, "memory.json"), a.Logger)
	if err != nil {
		if a.Logger != nil {
			a.Logger.Printf("app: global transcript unavailable (%v); turns will not be recorded to a transcript", err)
		}
		return
	}
	a.globalTranscript = tr
	a.globalSessionID = id
	a.Agent.SetTranscript(tr)
}

// ListSessions lists the CLI/global sessions, newest first, with
// the live session id.
func (a *App) ListSessions() ([]session.Info, string, error) {
	if a == nil || a.Config == nil || a.Config.DataDir == "" {
		return nil, "", errors.New("app: sessions need a data dir")
	}
	infos, err := session.List(a.Config.DataDir)
	if err != nil {
		return nil, "", err
	}
	current := a.globalSessionID
	if current == "" {
		current, _ = session.ReadCurrent(a.Config.DataDir)
	}
	return infos, current, nil
}

// ResumeSession resumes a CLI/global session by full id or unique
// prefix, mirroring ResumeSessionForChat. It returns the resolved
// id and the message count now in working history.
func (a *App) ResumeSession(idOrPrefix string) (string, int, error) {
	if a == nil || a.Config == nil || a.Config.DataDir == "" || a.Memory == nil {
		return "", 0, errors.New("app: resume needs a configured App with a data dir")
	}
	full, err := session.ResolvePrefix(a.Config.DataDir, idOrPrefix)
	if err != nil {
		return "", 0, err
	}
	if full == a.globalSessionID {
		msgs, err := a.Memory.RecentMessages(0)
		if err != nil {
			return "", 0, fmt.Errorf("app: resume: read history: %w", err)
		}
		return full, len(msgs), nil
	}
	tr, err := session.Open(a.Config.DataDir, full)
	if err != nil {
		return "", 0, fmt.Errorf("app: resume: open transcript: %w", err)
	}
	rep, err := LoadResumedSession(a.Memory, tr, full, a.Logger)
	if err != nil {
		return "", 0, err
	}
	if err := session.WriteCurrent(a.Config.DataDir, full); err != nil {
		return "", 0, fmt.Errorf("app: resume: record current session: %w", err)
	}
	oldTr := a.globalTranscript
	a.globalTranscript = tr
	a.globalSessionID = full
	if a.Agent != nil {
		a.Agent.SetTranscript(tr)
		a.Agent.ResetCompactionBreaker()
	}
	if oldTr != nil && oldTr != tr {
		_ = oldTr.Close()
	}
	recordResumeReport("global", rep)
	return full, rep.LoadedMessages, nil
}

// CurrentSessionID returns the CLI/global live session id.
func (a *App) CurrentSessionID() string {
	if a == nil {
		return ""
	}
	return a.globalSessionID
}
