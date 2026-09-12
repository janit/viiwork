package supervisor

import (
	"time"

	"github.com/janit/viiwork/v2/meshapi"
)

// State is a backend's place on the health ladder.
type State int

const (
	StateStarting State = iota
	StateHealthy
	StateUnhealthy
	StateDead
)

// String is the meshapi status word published for the state.
func (s State) String() string {
	switch s {
	case StateHealthy:
		return meshapi.StatusHealthy
	case StateUnhealthy:
		return meshapi.StatusUnhealthy
	case StateDead:
		return meshapi.StatusDead
	default:
		return meshapi.StatusStarting
	}
}

// Action is what the supervision loop must do after an observation.
type Action int

const (
	ActionNone Action = iota
	ActionRespawn
	ActionMarkDead
)

// Causes of a give-up, as logged and emitted.
const (
	CauseStartupTimeout = "startup timeout"
	CauseExited         = "process exited"
	CauseProbes         = "health probes failed"
	CauseHardFailure    = "hard failure on the inference path"
)

// respawnRefill is how long a backend must stay healthy before its respawn
// budget refills (Decision 1).
const respawnRefill = 10 * time.Minute

type LadderConfig struct {
	MaxFailures    int
	RespawnGrace   time.Duration
	MaxRespawns    int
	StartupTimeout time.Duration
}

// Ladder is the health state machine of one backend, ported from v1's
// handleHealthResult: starting -> healthy -> unhealthy after MaxFailures
// failed probes -> respawn, with a hard failure or an exited process skipping
// the strikes, and dead once the respawn budget is spent.
//
// Decision 1: MaxRespawns is the number of respawns allowed, and the budget
// refills only after 10 minutes of continuous health. v1 counted the respawn
// that tipped into dead as an attempt and reset the counter on every recovery,
// so a backend that crashed a few minutes after each load respawned forever.
//
// The ladder is pure: no clock, no I/O, no goroutines, and not safe for
// concurrent use (the Backend guards it).
type Ladder struct {
	cfg          LadderConfig
	state        State
	start        time.Time
	healthySince time.Time
	strikes      int
	respawns     int
	hardFailure  bool
	condemned    string // cause set by Condemn, "" when not condemned
	deferredAt   time.Time
	deferCause   string
	cause        string
}

func NewLadder(cfg LadderConfig, now time.Time) *Ladder {
	return &Ladder{cfg: cfg, state: StateStarting, start: now}
}

// Observe records one probe result and returns the action it calls for.
func (l *Ladder) Observe(now time.Time, ready, alive bool, inFlight int) Action {
	if l.state == StateDead {
		return ActionNone
	}
	if l.condemned != "" {
		l.state = StateUnhealthy
		return l.giveUp(now, l.condemned, alive, inFlight)
	}

	if l.state == StateStarting {
		if ready {
			l.state = StateHealthy
			l.healthySince = now
			l.clearTransient()
			return ActionNone
		}
		if alive && now.Sub(l.start) < l.cfg.StartupTimeout {
			return ActionNone // only the timeout ends patience while loading
		}
		cause := CauseStartupTimeout
		if !alive {
			cause = CauseExited
		}
		return l.giveUp(now, cause, alive, 0)
	}

	if ready {
		if l.state == StateUnhealthy {
			l.healthySince = now
		}
		l.state = StateHealthy
		l.clearTransient()
		if l.respawns > 0 && now.Sub(l.healthySince) >= respawnRefill {
			l.respawns = 0
		}
		return ActionNone
	}

	l.state = StateUnhealthy
	var cause string
	switch {
	case l.hardFailure:
		l.strikes = l.cfg.MaxFailures
		l.hardFailure = false
		cause = CauseHardFailure
	case !alive:
		l.strikes = l.cfg.MaxFailures
		cause = CauseExited
	default:
		l.strikes++
		cause = CauseProbes
	}
	if l.strikes < l.cfg.MaxFailures {
		return ActionNone
	}
	return l.giveUp(now, cause, alive, inFlight)
}

// giveUp respawns or marks dead, first waiting up to RespawnGrace while a
// live process still has requests in flight.
func (l *Ladder) giveUp(now time.Time, cause string, alive bool, inFlight int) Action {
	if !l.deferredAt.IsZero() && cause == CauseProbes {
		// Probes keep failing while a harder cause drains: report the cause
		// that started the drain.
		cause = l.deferCause
	}
	if alive && inFlight > 0 && l.cfg.RespawnGrace > 0 {
		if l.deferredAt.IsZero() {
			l.deferredAt, l.deferCause = now, cause
		}
		if now.Sub(l.deferredAt) < l.cfg.RespawnGrace {
			return ActionNone
		}
	}
	l.deferredAt, l.deferCause = time.Time{}, ""
	l.strikes = 0
	l.cause = cause
	if l.respawns >= l.cfg.MaxRespawns {
		l.state = StateDead
		return ActionMarkDead
	}
	l.respawns++
	return ActionRespawn
}

func (l *Ladder) clearTransient() {
	l.strikes = 0
	l.hardFailure = false
	l.deferredAt, l.deferCause = time.Time{}, ""
}

// NoteHardFailure latches an inference-path transport failure (EOF, refused);
// the next failed probe gives up at once. A later ready probe clears it: the
// failure was stale or the backend recovered.
func (l *Ladder) NoteHardFailure() { l.hardFailure = true }

// Condemn makes the next observation give up with cause, whatever the probe
// says. It is how the on-GPU check respawns a backend that answers probes.
func (l *Ladder) Condemn(cause string) {
	if l.state != StateDead {
		l.condemned = cause
	}
}

// MarkDead ends the ladder at once. It is for a configuration bug no respawn
// fixes, such as an engine that cannot build a command.
func (l *Ladder) MarkDead(cause string) {
	l.state = StateDead
	l.cause = cause
}

// Restarted returns a relaunched backend to starting. Respawns are kept.
func (l *Ladder) Restarted(now time.Time) {
	if l.state == StateDead {
		return
	}
	l.state = StateStarting
	l.start = now
	l.clearTransient()
	l.condemned = ""
}

func (l *Ladder) State() State  { return l.state }
func (l *Ladder) Respawns() int { return l.respawns }
func (l *Ladder) Cause() string { return l.cause }
