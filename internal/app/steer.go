// Busy-input bridge: steering a message into a chat's running
// turn, and collecting the steered messages a run did not consume.
//
// Lock ordering: SteerTurn takes runMu, then chatsMu — never the
// reverse. No other path nests chatsMu inside runMu (RegisterRun /
// UnregisterRun / StopRun take runMu only and never touch chats;
// chatSessionFor / AgentForChat take chatsMu only and never touch
// runs), so this order cannot deadlock. The agent is read under
// chatsMu and Steer is called after both locks are released:
// Steer probes the agent's own runMu, which is a different mutex
// from the app's, and never blocks on a run.
package app

import (
	"github.com/irfan/orion/internal/core"
)

// SteerTurn offers text to the live turn for chatID, for
// busy-input mode "steer". It reports false when no turn is
// registered for the chat (nothing is running to steer into), when
// the chat has no agent yet, or when the agent's Steer declines
// (no run active — the run ended between the registration check
// and the steer). The caller then runs the message as a normal
// turn instead. It never builds an agent just to steer: a chat
// with no agent has no run, so there is nothing to steer into.
func (a *App) SteerTurn(chatID int64, text string, updateID int64) bool {
	if a == nil {
		return false
	}
	a.runMu.Lock()
	_, running := a.runs[chatID]
	if !running {
		a.runMu.Unlock()
		return false
	}
	a.chatsMu.Lock()
	var agent *core.Agent
	if cs := a.chats[chatID]; cs != nil {
		agent = cs.agent
	}
	a.chatsMu.Unlock()
	a.runMu.Unlock()
	if agent == nil {
		return false
	}
	return agent.Steer(text, updateID)
}

// DrainSteeredLeftovers returns the steered messages the chat's
// last run did not consume (they arrived during the final provider
// call, or after the step budget ran out) and clears them, so the
// gateway can run each as a fresh turn. A chat with no agent — or
// no app — has no leftovers. The agent's DrainPendingSteer is
// itself the return-and-clear: a message it returns here is
// either run by the caller or lost by the caller, never by this
// method.
func (a *App) DrainSteeredLeftovers(chatID int64) []core.SteeredMessage {
	if a == nil {
		return nil
	}
	a.chatsMu.Lock()
	var agent *core.Agent
	if cs := a.chats[chatID]; cs != nil {
		agent = cs.agent
	}
	a.chatsMu.Unlock()
	if agent == nil {
		return nil
	}
	return agent.DrainPendingSteer()
}
