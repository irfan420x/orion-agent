// /sessions and /resume (audit P0 #4): the Telegram commands that
// list a chat's per-session transcripts and resume one by id or
// unique prefix.
package messaging

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type stubSessionEnv struct {
	env *CommandEnv

	listInfos   []SessionInfo
	listCurrent string
	listErr     error
	listCalled  bool

	resumeArg    string
	resumeID     string
	resumeCount  int
	resumeErr    error
	resumeCalled bool
}

func newStubSessionEnv() *stubSessionEnv {
	s := &stubSessionEnv{
		listInfos: []SessionInfo{
			{ID: "20261113-150405-aaaaaaaa", Messages: 12, Summaries: 1, UpdatedAt: time.Now().Add(-time.Hour), Title: "first chat"},
			{ID: "20261113-160405-bbbbbbbb", Messages: 4, Summaries: 0, UpdatedAt: time.Now(), Title: "second chat"},
		},
		listCurrent: "20261113-160405-bbbbbbbb",
		resumeID:    "20261113-150405-aaaaaaaa",
		resumeCount: 12,
	}
	s.env = &CommandEnv{
		ChatID: 7,
		ListSessions: func(chatID int64) ([]SessionInfo, string, error) {
			s.listCalled = true
			return s.listInfos, s.listCurrent, s.listErr
		},
		ResumeSession: func(chatID int64, idOrPrefix string) (string, int, error) {
			s.resumeCalled = true
			s.resumeArg = idOrPrefix
			return s.resumeID, s.resumeCount, s.resumeErr
		},
	}
	return s
}

func dispatchSession(t *testing.T, s *stubSessionEnv, text string) string {
	t.Helper()
	r := DefaultRegistry(s.env)
	reply, handled, err := r.Dispatch(context.Background(), s.env, text)
	if err != nil {
		t.Fatalf("Dispatch(%q) error: %v", text, err)
	}
	if !handled {
		t.Fatalf("Dispatch(%q) not handled", text)
	}
	return reply
}

func TestSessionsCommandLists(t *testing.T) {
	s := newStubSessionEnv()
	reply := dispatchSession(t, s, "/sessions")
	if !s.listCalled {
		t.Fatalf("ListSessions not called")
	}
	for _, want := range []string{
		"20261113-150405-aaaaaaaa", "20261113-160405-bbbbbbbb",
		"12 message(s)", "4 message(s)", "1 summaries",
		"★ live", `"first chat"`,
	} {
		if !strings.Contains(reply, want) {
			t.Errorf("reply missing %q:\n%s", want, reply)
		}
	}
	if !strings.Contains(reply, "/resume") {
		t.Errorf("reply should point at /resume:\n%s", reply)
	}
}

func TestSessionsCommandEmpty(t *testing.T) {
	s := newStubSessionEnv()
	s.listInfos = nil
	reply := dispatchSession(t, s, "/sessions")
	if !strings.Contains(reply, "No saved sessions yet") {
		t.Errorf("empty reply: %q", reply)
	}
}

func TestSessionsCommandError(t *testing.T) {
	s := newStubSessionEnv()
	s.listErr = errors.New("disk exploded")
	reply := dispatchSession(t, s, "/sessions")
	if !strings.Contains(reply, "Could not list sessions") {
		t.Errorf("error reply: %q", reply)
	}
}

func TestSessionsCommandUnwired(t *testing.T) {
	s := newStubSessionEnv()
	s.env.ListSessions = nil
	reply := dispatchSession(t, s, "/sessions")
	if !strings.Contains(reply, "unavailable") {
		t.Errorf("unwired reply should say unavailable: %q", reply)
	}
}

func TestResumeCommand(t *testing.T) {
	s := newStubSessionEnv()
	reply := dispatchSession(t, s, "/resume 20261113-150405-aa")
	if !s.resumeCalled {
		t.Fatalf("ResumeSession not called")
	}
	if s.resumeArg != "20261113-150405-aa" {
		t.Errorf("resume arg=%q, want the prefix as typed", s.resumeArg)
	}
	for _, want := range []string{"20261113-150405-aaaaaaaa", "12 message(s)"} {
		if !strings.Contains(reply, want) {
			t.Errorf("reply missing %q:\n%s", want, reply)
		}
	}
}

func TestResumeCommandUsageAndError(t *testing.T) {
	s := newStubSessionEnv()
	if reply := dispatchSession(t, s, "/resume"); !strings.Contains(reply, "Usage: /resume") {
		t.Errorf("bare /resume should print usage: %q", reply)
	}
	s.resumeErr = errors.New("no session matches \"zzz\"")
	if reply := dispatchSession(t, s, "/resume zzz"); !strings.Contains(reply, "Could not resume session") {
		t.Errorf("failing resume reply: %q", reply)
	}
}

func TestResumeCommandUnwired(t *testing.T) {
	s := newStubSessionEnv()
	s.env.ResumeSession = nil
	reply := dispatchSession(t, s, "/resume 20261113-150405-aa")
	if !strings.Contains(reply, "unavailable") {
		t.Errorf("unwired reply should say unavailable: %q", reply)
	}
}

// TestResumeCommandReportsCorruptAndCap (Audit 5 F-A5-P2-5): when
// the last-resume report says the transcript was capped, had
// corrupt lines, and merged summaries, the /resume reply must say
// so instead of silently dropping it.
func TestResumeCommandReportsCorruptAndCap(t *testing.T) {
	s := newStubSessionEnv()
	s.env.LastResumeReport = func(chatID int64) (ResumeReport, bool) {
		return ResumeReport{
			SessionID:          s.resumeID,
			TranscriptMessages: 250,
			LoadedMessages:     200,
			CorruptLines:       1,
			SummariesLoaded:    2,
			Capped:             true,
		}, true
	}
	reply := dispatchSession(t, s, "/resume 20261113-150405-aa")
	for _, want := range []string{
		"the transcript held 250 messages; working history is capped at 200",
		"1 corrupt line(s) were skipped",
		"2 summaries were merged into memory",
	} {
		if !strings.Contains(reply, want) {
			t.Errorf("reply missing %q:\n%s", want, reply)
		}
	}
}

// TestResumeCommandWithoutReportStaysShort: with no report wired,
// the reply keeps its short form (no dangling "Note:").
func TestResumeCommandWithoutReportStaysShort(t *testing.T) {
	s := newStubSessionEnv()
	reply := dispatchSession(t, s, "/resume 20261113-150405-aa")
	if strings.Contains(reply, "Note:") {
		t.Errorf("reply without a report should not carry a note:\n%s", reply)
	}
}
