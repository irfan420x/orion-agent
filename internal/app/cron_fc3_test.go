package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/irfan/orion/internal/permissions"
)

// --- App-level cron warning wiring (F-C3) ---------------------------------
//
// The registry-level wiring test (tools.TestSetCronScheduleRuleStoreWiring)
// pins that SetCronScheduleRuleStore installs the store on the
// cron_schedule tool, and NewPermissionGate is the one shared
// constructor every entrypoint calls — but the two were never
// exercised together. This test closes that seam: build a real App,
// call NewPermissionGate, then schedule through a.Tools and assert
// the warning fires. If this fails, scheduled jobs silently lose
// their warning in production even though each half passes alone.

func TestAppNewPermissionGateWiresCronWarning(t *testing.T) {
	a := testApp(t)
	defer a.Close()
	gate, err := a.NewPermissionGate(nil)
	if err != nil {
		t.Fatalf("NewPermissionGate: %v", err)
	}
	if err := gate.Rules().SetForChat(111, "shell_exec", permissions.ActionAllow); err != nil {
		t.Fatal(err)
	}
	ctx := permissions.WithApprovalContext(context.Background(), 111, 111)
	out, err := a.Tools.Execute(ctx, "cron_schedule", json.RawMessage(`{"expression":"0 9 * * *","prompt":"morning brief"}`))
	if err != nil {
		t.Fatalf("a.Tools cron_schedule: %v", err)
	}
	var parsed struct {
		Job struct {
			ID string `json:"id"`
		} `json:"job"`
		Note string `json:"note"`
	}
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("cron_schedule result is not JSON (%q): %v", out, err)
	}
	if parsed.Job.ID == "" {
		t.Fatalf("cron_schedule created no job: %s", out)
	}
	if !strings.Contains(parsed.Note, "Warning") || !strings.Contains(parsed.Note, "shell_exec") {
		t.Errorf("schedule note via App = %q, want a Warning naming shell_exec (NewPermissionGate must wire the live rule store into the registry's cron_schedule tool)", parsed.Note)
	}
}
