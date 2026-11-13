package core

import (
	"time"

	"github.com/irfan/orion/internal/model"
)

type EventKind string

const (
	EventStepStarted      EventKind = "step_started"
	EventToolCallStarted  EventKind = "tool_call_started"
	EventToolCallFinished EventKind = "tool_call_finished"
	EventTurnFinished     EventKind = "turn_finished"
)

// ProgressEvent is one step of an agent turn, emitted synchronously on the
// goroutine running the turn. Handlers MUST be fast and non-blocking and
// MUST NOT call back into the Agent (no Run/RunStream/Shutdown from a
// handler): the loop holds runMu while emitting.
type ProgressEvent struct {
	Kind EventKind
	// Step is the 1-based provider step number within the turn.
	Step int
	// ToolName and Arguments describe the tool call (tool events only).
	// Arguments is the raw JSON arguments string, exactly as the model sent it.
	ToolName  string
	Arguments string
	// Duration is set on ToolCallFinished (wall time of that tool run) and
	// on TurnFinished (wall time of the whole turn).
	Duration time.Duration
	// Err is set when the tool run failed (ToolCallFinished) or the turn
	// ended in error (TurnFinished). Tool "errors" that are returned as
	// result text (executeTool returns "Error: ..." strings) count as Err
	// here too: set Err to an errors.New of that text when the result
	// starts with "Error" (use the existing executeTool result string).
	Err error
	// Usage is the accumulated provider usage for the turn so far; set on
	// every event after the first provider call, final on TurnFinished.
	Usage model.Usage
}

// EventHandler receives ProgressEvents from an Agent run.
type EventHandler func(ev ProgressEvent)

// TurnStats summarises one finished turn for display.
type TurnStats struct {
	Provider string
	Model    string
	Tokens   int // total tokens (Usage.TotalTokens from TurnFinished)
	Steps    int // last step number
	Duration time.Duration
}

// EventHandler returns the agent's current progress-event handler, or
// nil when none is set. It exists so callers that install a temporary
// handler for one run (app.HandleStream) can save the previous handler
// and restore it afterwards instead of clearing a handler they did
// not install.
func (a *Agent) EventHandler() EventHandler {
	a.eventHandlerMu.RLock()
	defer a.eventHandlerMu.RUnlock()
	return a.eventHandler
}

// SetEventHandler replaces the agent's progress-event handler. A nil
// handler clears it (events are then not emitted at all). It never
// blocks on a running run: the handler is read under eventHandlerMu
// at each emission, so a run in flight may or may not observe a
// handler set mid-run, but it never deadlocks and never races.
func (a *Agent) SetEventHandler(h EventHandler) {
	a.eventHandlerMu.Lock()
	defer a.eventHandlerMu.Unlock()
	a.eventHandler = h
}

// emit delivers one ProgressEvent to the current handler, if any. It
// no-ops when no handler is set, so a run without a handler behaves
// exactly as before with no added overhead beyond one RWMutex read.
// A panicking handler is recovered and logged via a.logf; it must
// never crash the loop. emit is called while runMu is held — that is
// intended, and handlers must not call back into the Agent.
func (a *Agent) emit(ev ProgressEvent) {
	a.eventHandlerMu.RLock()
	h := a.eventHandler
	a.eventHandlerMu.RUnlock()
	if h == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			a.logf("event handler panicked: %v", r)
		}
	}()
	h(ev)
}
