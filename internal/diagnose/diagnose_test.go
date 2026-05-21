package diagnose

import "testing"

func TestExplain(t *testing.T) {
	for _, c := range []struct {
		name        string
		f           Facts
		code, owner string
	}{
		{"kernel kill beats log text", Facts{Phase: "setup", Exit: 137, OOM: true, Err: "npm error code ERESOLVE"}, "setup_oom", Setup},
		{"137 without a kill count is not OOM", Facts{Phase: "setup", Exit: 137}, "setup_failed", Setup},
		{"refused host explains a timeout", Facts{Phase: "setup", Exit: 124, TimedOut: true, Denied: []string{"x.example"}}, "setup_egress_denied", Setup},
		{"plain timeout", Facts{Phase: "agent", Exit: 124, TimedOut: true}, "agent_timeout", Agent},
		{"refused host beats log text", Facts{Phase: "setup", Exit: 1, Denied: []string{"x"}, Err: "403 Forbidden"}, "setup_egress_denied", Setup},
		{"lockfile on the base branch", Facts{Phase: "setup", Exit: 1, Err: "npm error code EUSAGE"}, "lockfile_out_of_date", Setup},
		{"lockfile in the agent's change", Facts{Phase: "check", Exit: 1, Err: "npm error code EUSAGE"}, "lockfile_out_of_date", Agent},
		{"setup on the agent's commit", Facts{Phase: "setup", Exit: 1, Err: "npm error code EUSAGE", Changed: true}, "lockfile_out_of_date", Agent},
		{"setup breaks only after the change", Facts{Phase: "setup", Exit: 1, Changed: true}, "setup_failed", Agent},
		{"engine", Facts{Phase: "setup", Exit: 1, Err: "npm error code EBADENGINE"}, "toolchain_version", Setup},
		{"variable", Facts{Phase: "check", Exit: 1, Err: "Error: Missing required environment variable STRIPE_KEY"}, "missing_env", Setup},
		{"tool", Facts{Phase: "setup", Exit: 127, Err: "sh: pnpm: not found"}, "command_not_found", Setup},
		{"disk", Facts{Phase: "setup", Exit: 1, Err: "ENOSPC: no space left"}, "disk_full", Platform},
		{"clone", Facts{Phase: "clone", Exit: 128, Err: "fatal: x"}, "clone_failed", Platform},
		{"test failure", Facts{Phase: "check", Exit: 1, Out: "not ok 1"}, "checks_failed", Agent},
		{"lost", Facts{Phase: "agent", Lost: true, Exit: 137}, "sandbox_lost", Platform},
		{"preview on loopback", Facts{Phase: "preview", Alive: true, Listen: "loopback"}, "preview_loopback_only", Setup},
		{"preview died", Facts{Phase: "preview", Err: "SyntaxError"}, "preview_crashed", Agent},
		{"preview wrong port", Facts{Phase: "preview", Alive: true}, "preview_not_listening", Setup},
		{"preview 500", Facts{Phase: "preview", Alive: true, Listen: "any", HTTPCode: 500}, "preview_http_error", Agent},
	} {
		if got := Explain(c.f); got.Code != c.code || got.Owner != c.owner {
			t.Errorf("%s: %s/%s, want %s/%s", c.name, got.Code, got.Owner, c.code, c.owner)
		}
	}
}
