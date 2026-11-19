// /model flag tests: --once, --global, --confirm, reset, and the
// cost-confirm gate. The handler is a thin parser over the App via
// CommandEnv; the App behaviour itself is covered in internal/app.
package messaging

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type stubModelEnv struct {
	env *CommandEnv

	provider, modelName string
	providers           []string

	switchCalls       []string
	switchOnceCalls   []string
	switchGlobalCalls []string
	resetCalls        int
	switchErr         error

	warning     string
	warningErr  error
	warningSeen []string

	detail string
}

func newStubModelEnv() *stubModelEnv {
	s := &stubModelEnv{
		provider:  "deepseek",
		modelName: "deepseek-flash",
		providers: []string{"openai", "deepseek", "anthropic", "ollama"},
		detail:    "Aliases: fast -> openai/gpt-4o-mini\nFallback chain: deepseek -> openai",
	}
	s.env = &CommandEnv{
		ChatID:  7,
		OwnerID: 7,
		ModelInfo: func() (string, string, []string) {
			return s.provider, s.modelName, s.providers
		},
		ModelDetail: func() string { return s.detail },
		SwitchProvider: func(name string) error {
			s.switchCalls = append(s.switchCalls, name)
			if s.switchErr != nil {
				return s.switchErr
			}
			s.provider = name
			return nil
		},
		SwitchProviderOnce: func(route string) error {
			s.switchOnceCalls = append(s.switchOnceCalls, route)
			return nil
		},
		SwitchProviderGlobal: func(route string) error {
			s.switchGlobalCalls = append(s.switchGlobalCalls, route)
			return nil
		},
		ResetProvider: func() error {
			s.resetCalls++
			s.provider, s.modelName = "deepseek", "deepseek-flash"
			return nil
		},
		SwitchCostWarning: func(route string) (string, error) {
			s.warningSeen = append(s.warningSeen, route)
			return s.warning, s.warningErr
		},
	}
	return s
}

func dispatchModel(t *testing.T, s *stubModelEnv, text string) string {
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

func TestModelShowIncludesAliasesAndChain(t *testing.T) {
	s := newStubModelEnv()
	reply := dispatchModel(t, s, "/model")
	for _, want := range []string{"deepseek / deepseek-flash", "Aliases: fast", "Fallback chain: deepseek -> openai"} {
		if !strings.Contains(reply, want) {
			t.Errorf("reply missing %q:\n%s", want, reply)
		}
	}
	if len(s.switchCalls) != 0 {
		t.Errorf("/model with no args switched providers: %v", s.switchCalls)
	}
}

func TestModelSwitchScopes(t *testing.T) {
	t.Run("chat scope is the default", func(t *testing.T) {
		s := newStubModelEnv()
		reply := dispatchModel(t, s, "/model openai")
		if !strings.Contains(reply, "openai") {
			t.Errorf("reply = %q, want it to name openai", reply)
		}
		if len(s.switchCalls) != 1 || s.switchCalls[0] != "openai" {
			t.Errorf("SwitchProvider calls = %v, want [openai]", s.switchCalls)
		}
		if len(s.switchOnceCalls) != 0 || len(s.switchGlobalCalls) != 0 {
			t.Errorf("once/global calls fired on a plain switch: %v %v", s.switchOnceCalls, s.switchGlobalCalls)
		}
	})
	t.Run("once", func(t *testing.T) {
		s := newStubModelEnv()
		reply := dispatchModel(t, s, "/model openai --once")
		if !strings.Contains(reply, "once") && !strings.Contains(reply, "next turn") {
			t.Errorf("reply = %q, want it to say the switch lasts one turn", reply)
		}
		if len(s.switchOnceCalls) != 1 || s.switchOnceCalls[0] != "openai" {
			t.Errorf("SwitchProviderOnce calls = %v, want [openai]", s.switchOnceCalls)
		}
		if len(s.switchCalls) != 0 {
			t.Errorf("SwitchProvider calls = %v, want none (--once must not persist)", s.switchCalls)
		}
	})
	t.Run("global", func(t *testing.T) {
		s := newStubModelEnv()
		reply := dispatchModel(t, s, "/model openai --global")
		if !strings.Contains(reply, "global") && !strings.Contains(reply, "daemon") {
			t.Errorf("reply = %q, want it to say the switch is global", reply)
		}
		if len(s.switchGlobalCalls) != 1 || s.switchGlobalCalls[0] != "openai" {
			t.Errorf("SwitchProviderGlobal calls = %v, want [openai]", s.switchGlobalCalls)
		}
		if len(s.switchCalls) != 0 {
			t.Errorf("SwitchProvider calls = %v, want none (--global is the daemon default, not the chat)", s.switchCalls)
		}
	})
	t.Run("reset", func(t *testing.T) {
		s := newStubModelEnv()
		s.provider = "openai"
		reply := dispatchModel(t, s, "/model reset")
		if s.resetCalls != 1 {
			t.Errorf("ResetProvider calls = %d, want 1", s.resetCalls)
		}
		if !strings.Contains(reply, "deepseek") {
			t.Errorf("reply = %q, want it to name the restored default", reply)
		}
	})
	t.Run("provider/model route", func(t *testing.T) {
		s := newStubModelEnv()
		dispatchModel(t, s, "/model deepseek/deepseek-v4-pro")
		if len(s.switchCalls) != 1 || s.switchCalls[0] != "deepseek/deepseek-v4-pro" {
			t.Errorf("SwitchProvider calls = %v, want [deepseek/deepseek-v4-pro] (the route passes through)", s.switchCalls)
		}
	})
}

func TestModelFlagConflicts(t *testing.T) {
	for _, text := range []string{
		"/model openai --once --global",
		"/model openai --global --once",
		"/model reset --once",
		"/model reset --global",
		"/model reset --confirm",
		"/model --once",
		"/model openai --bogus",
	} {
		s := newStubModelEnv()
		reply := dispatchModel(t, s, text)
		if !strings.Contains(reply, "❌") {
			t.Errorf("Dispatch(%q) = %q, want an error reply", text, reply)
		}
		if len(s.switchCalls) != 0 || len(s.switchOnceCalls) != 0 || len(s.switchGlobalCalls) != 0 || s.resetCalls != 0 {
			t.Errorf("Dispatch(%q) switched something: chat %v once %v global %v reset %d", text, s.switchCalls, s.switchOnceCalls, s.switchGlobalCalls, s.resetCalls)
		}
	}
}

func TestModelCostConfirmGate(t *testing.T) {
	s := newStubModelEnv()
	s.warning = "Switching to anthropic/claude-opus-4 costs about $15.00/$75.00 per 1M tokens (est.), vs $0.15/$0.60 now."
	reply := dispatchModel(t, s, "/model anthropic/claude-opus-4")
	if !strings.Contains(reply, s.warning) {
		t.Errorf("reply = %q, want it to carry the cost warning", reply)
	}
	if !strings.Contains(reply, "--confirm") {
		t.Errorf("reply = %q, want it to name --confirm", reply)
	}
	if len(s.switchCalls) != 0 {
		t.Errorf("SwitchProvider calls = %v, want none (no switch before confirmation)", s.switchCalls)
	}
	// Re-issuing with --confirm proceeds.
	reply = dispatchModel(t, s, "/model anthropic/claude-opus-4 --confirm")
	if len(s.switchCalls) != 1 {
		t.Errorf("SwitchProvider calls after --confirm = %v, want 1", s.switchCalls)
	}
	if strings.Contains(reply, "❌") {
		t.Errorf("reply after --confirm = %q, want success", reply)
	}
}

func TestModelCostWarningError(t *testing.T) {
	s := newStubModelEnv()
	s.warningErr = errors.New("unknown route")
	reply := dispatchModel(t, s, "/model nope")
	if !strings.Contains(reply, "❌") {
		t.Errorf("reply = %q, want an error reply when route resolution fails", reply)
	}
	if len(s.switchCalls) != 0 {
		t.Errorf("SwitchProvider calls = %v, want none", s.switchCalls)
	}
}

func TestModelSwitchErrorListsProviders(t *testing.T) {
	s := newStubModelEnv()
	s.switchErr = errors.New(`app: unknown provider "claude"`)
	reply := dispatchModel(t, s, "/model claude")
	if !strings.Contains(reply, "❌") || !strings.Contains(reply, "unknown provider") {
		t.Errorf("reply = %q, want the error", reply)
	}
	if !strings.Contains(reply, "openai") {
		t.Errorf("reply = %q, want the available providers listed", reply)
	}
}
