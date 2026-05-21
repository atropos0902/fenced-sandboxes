// Package diagnose turns a failed command into a cause, an owner and the
// evidence for it. Measured facts are looked at before log text, because a log
// line is whatever the program felt like printing.
package diagnose

import (
	"regexp"
	"strings"
)

// Owners say who has to act.
const (
	Platform = "platform" // us; the attempt is retried
	Setup    = "setup"    // the repository's onboarding settings
	Agent    = "agent"    // the change itself
)

type Facts struct {
	Phase    string // clone, setup, agent, check, preview
	Exit     int
	Out, Err string
	TimedOut bool
	OOM      bool
	Denied   []string // hosts the proxy refused during this phase
	Lost     bool     // the provider no longer has the sandbox
	Changed  bool     // the checkout holds the agent's commit, not the base branch

	// Preview only
	Listen   string // "", "loopback" or "any"
	Alive    bool
	HTTPCode int
}

type Cause struct {
	Code     string   `json:"code"`
	Owner    string   `json:"owner"`
	Evidence []string `json:"evidence"`
	Next     string   `json:"next"`
}

type pattern struct {
	code, owner, next string
	re                *regexp.Regexp
}

var patterns = []pattern{
	{"lockfile_out_of_date", Agent, "Commit the updated lockfile.", regexp.MustCompile(`(?i)npm error code EUSAGE|can only install packages when your package\.json and package-lock\.json|ERR_PNPM_OUTDATED_LOCKFILE|lockfile needs to be updated`)},
	{"dependency_conflict", Agent, "Resolve the version conflict named in the log.", regexp.MustCompile(`npm error code ERESOLVE|ResolutionImpossible|failed to select a version for`)},
	{"toolchain_version", Setup, "Change the base image or the engines field.", regexp.MustCompile(`EBADENGINE|requires go >= |Unsupported engine|requires a different Python`)},
	{"registry_auth", Setup, "Add a credential for this registry.", regexp.MustCompile(`npm error code E40[13]|401 Unauthorized|403 Forbidden.*registry`)},
	{"missing_env", Setup, "Add the variable to the repository's settings.", regexp.MustCompile(`(?i)(missing|undefined|required) (required )?env(ironment)?( variable)?|environment variable \S+ (is )?(not set|missing|required)|KeyError: '[A-Z_]+'`)},
	{"disk_full", Platform, "Raise the disk limit.", regexp.MustCompile(`ENOSPC|No space left on device`)},
	{"process_limit", Setup, "Raise the process limit or lower build parallelism.", regexp.MustCompile(`fork: (retry: )?Resource temporarily unavailable|can't fork|EAGAIN.*(spawn|fork)|pthread_create failed`)},
}

func Explain(f Facts) Cause {
	if f.Lost {
		return Cause{"sandbox_lost", Platform, []string{"provider no longer lists the sandbox"}, "Retried on a new sandbox."}
	}
	if f.OOM {
		return Cause{f.Phase + "_oom", Setup, []string{"cgroup oom_kill count rose during the command"}, "Raise memory_mb or lower build parallelism."}
	}
	if f.TimedOut {
		owner := Setup
		if f.Phase == "agent" {
			owner = Agent
		}
		ev := []string{"time limit reached"}
		if len(f.Denied) > 0 {
			// Installers retry a refused host until the clock runs out.
			ev = append(ev, "refused hosts: "+strings.Join(f.Denied, ", "))
			return Cause{f.Phase + "_egress_denied", Setup, ev, "Allow the host or remove the dependency on it."}
		}
		return Cause{f.Phase + "_timeout", owner, append(ev, last(f)), "Check what the last log line was waiting for."}
	}
	if f.Phase == "preview" {
		return preview(f)
	}
	if len(f.Denied) > 0 {
		return Cause{f.Phase + "_egress_denied", Setup, []string{"refused hosts: " + strings.Join(f.Denied, ", ")}, "Allow the host or remove the dependency on it."}
	}
	log := f.Err + "\n" + f.Out
	for _, p := range patterns {
		if m := p.re.FindString(log); m != "" {
			owner := p.owner
			if f.Phase == "setup" && owner == Agent && !f.Changed {
				owner = Setup // nothing the agent wrote exists yet
			}
			return Cause{p.code, owner, []string{clip(line(log, m))}, p.next}
		}
	}
	switch {
	case f.Exit == 127:
		return Cause{"command_not_found", Setup, []string{"exit 127", last(f)}, "Install the tool in the base image."}
	case f.Phase == "clone":
		return Cause{"clone_failed", Platform, []string{last(f)}, "Retried. Check the GitHub App installation if it repeats."}
	case f.Phase == "check":
		return Cause{"checks_failed", Agent, []string{telling(log)}, "Read the check output on the pull request."}
	case f.Phase == "agent":
		return Cause{"agent_failed", Agent, []string{last(f)}, "Read the agent log."}
	}
	owner := Setup
	if f.Changed { // the same command worked on the base branch
		owner = Agent
	}
	return Cause{f.Phase + "_failed", owner, []string{last(f)}, "Run the setup command in a fresh checkout."}
}

var failing = regexp.MustCompile(`(?m)^\s*(✖|not ok|FAIL|--- FAIL|.*error TS\d+|.*AssertionError|.*Error:).*$`)

// telling picks the line that names the failure. Test runners end with a
// summary or a stack trace, so the last line is rarely it.
func telling(log string) string {
	if m := strings.TrimSpace(failing.FindString(log)); m != "" {
		return clip(m)
	}
	return last(Facts{Out: log})
}

func preview(f Facts) Cause {
	switch {
	case !f.Alive:
		return Cause{"preview_crashed", Agent, []string{telling(f.Err)}, "Read the app's last lines."}
	case f.Listen == "loopback":
		return Cause{"preview_loopback_only", Setup, []string{"port is open on 127.0.0.1 and nowhere else"}, "Bind the app to 0.0.0.0."}
	case f.Listen == "":
		return Cause{"preview_not_listening", Setup, []string{"process is alive, port is closed", telling(f.Err)}, "Check preview_port."}
	case f.HTTPCode >= 500:
		return Cause{"preview_http_error", Agent, []string{"HTTP " + itoa(f.HTTPCode), telling(f.Err)}, "Read the app's last lines."}
	}
	return Cause{"preview_not_ready", Setup, []string{telling(f.Err)}, "Raise preview_seconds if the app is still compiling."}
}

func last(f Facts) string {
	for _, s := range []string{f.Err, f.Out} {
		ls := strings.Split(strings.TrimSpace(s), "\n")
		if l := strings.TrimSpace(ls[len(ls)-1]); l != "" {
			return clip(l)
		}
	}
	return "no output"
}

func line(log, m string) string {
	i := strings.Index(log, m)
	s := strings.LastIndexByte(log[:i], '\n') + 1
	e := strings.IndexByte(log[i:], '\n')
	if e < 0 {
		return strings.TrimSpace(log[s:])
	}
	return strings.TrimSpace(log[s : i+e])
}

func clip(s string) string {
	if len(s) > 300 {
		return s[:300]
	}
	return s
}

func itoa(n int) string {
	return string([]byte{byte('0' + n/100%10), byte('0' + n/10%10), byte('0' + n%10)})
}
