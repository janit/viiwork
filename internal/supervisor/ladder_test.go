package supervisor

import (
	"testing"
	"time"
)

var t0 = time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)

var cfg3 = LadderConfig{MaxFailures: 3, RespawnGrace: 60 * time.Second, MaxRespawns: 2, StartupTimeout: 10 * time.Minute}

func at(d time.Duration) time.Time { return t0.Add(d) }

// step observes and checks the action and, when set, the state and cause.
type step struct {
	at       time.Duration
	ready    bool
	alive    bool
	inFlight int
	action   Action
	state    *State
	cause    string
}

func stateOf(s State) *State { return &s }

func run(t *testing.T, l *Ladder, steps ...step) {
	t.Helper()
	for i, s := range steps {
		got := l.Observe(at(s.at), s.ready, s.alive, s.inFlight)
		if got != s.action {
			t.Fatalf("step %d (+%v ready=%v alive=%v inFlight=%d): action %v, want %v", i, s.at, s.ready, s.alive, s.inFlight, got, s.action)
		}
		if s.state != nil && l.State() != *s.state {
			t.Fatalf("step %d: state %v, want %v", i, l.State(), *s.state)
		}
		if s.cause != "" && l.Cause() != s.cause {
			t.Fatalf("step %d: cause %q, want %q", i, l.Cause(), s.cause)
		}
	}
}

// healthyLadder is NewLadder observed ready and alive at +1 s.
func healthyLadder(t *testing.T, cfg LadderConfig) *Ladder {
	t.Helper()
	l := NewLadder(cfg, t0)
	run(t, l, step{at: time.Second, ready: true, alive: true, action: ActionNone, state: stateOf(StateHealthy)})
	return l
}

func TestLadder(t *testing.T) {
	s := time.Second

	t.Run("1 starting then ready", func(t *testing.T) {
		run(t, NewLadder(cfg3, t0),
			step{at: 1 * s, alive: true, action: ActionNone, state: stateOf(StateStarting)},
			step{at: 2 * s, ready: true, alive: true, action: ActionNone, state: stateOf(StateHealthy)})
	})

	t.Run("2 startup timeout", func(t *testing.T) {
		run(t, NewLadder(cfg3, t0),
			step{at: 9 * time.Minute, alive: true, action: ActionNone},
			step{at: 10 * time.Minute, alive: true, action: ActionRespawn, cause: CauseStartupTimeout})
	})

	t.Run("3 exits while starting", func(t *testing.T) {
		run(t, NewLadder(cfg3, t0), step{at: 1 * s, action: ActionRespawn, cause: CauseExited})
	})

	t.Run("4 probe strikes", func(t *testing.T) {
		run(t, healthyLadder(t, cfg3),
			step{at: 5 * s, alive: true, action: ActionNone, state: stateOf(StateUnhealthy)},
			step{at: 10 * s, alive: true, action: ActionNone, state: stateOf(StateUnhealthy)},
			step{at: 15 * s, alive: true, action: ActionRespawn, cause: CauseProbes})
	})

	t.Run("5 a ready probe restarts the strikes", func(t *testing.T) {
		run(t, healthyLadder(t, cfg3),
			step{at: 5 * s, alive: true, action: ActionNone},
			step{at: 10 * s, alive: true, action: ActionNone},
			step{at: 15 * s, ready: true, alive: true, action: ActionNone, state: stateOf(StateHealthy)},
			step{at: 20 * s, alive: true, action: ActionNone})
	})

	t.Run("6 hard failure skips the strikes", func(t *testing.T) {
		l := healthyLadder(t, cfg3)
		l.NoteHardFailure()
		run(t, l, step{at: 5 * s, alive: true, action: ActionRespawn, cause: CauseHardFailure})
	})

	t.Run("7 a ready probe clears the hard failure", func(t *testing.T) {
		l := healthyLadder(t, cfg3)
		l.NoteHardFailure()
		run(t, l,
			step{at: 5 * s, ready: true, alive: true, action: ActionNone},
			step{at: 10 * s, alive: true, action: ActionNone})
	})

	t.Run("8 exits while healthy", func(t *testing.T) {
		run(t, healthyLadder(t, cfg3), step{at: 5 * s, action: ActionRespawn, cause: CauseExited})
	})

	t.Run("9 respawn grace while requests are in flight", func(t *testing.T) {
		l := healthyLadder(t, cfg3)
		l.NoteHardFailure()
		run(t, l,
			step{at: 5 * s, alive: true, inFlight: 2, action: ActionNone},
			step{at: 30 * s, alive: true, inFlight: 2, action: ActionNone},
			step{at: 66 * s, alive: true, inFlight: 1, action: ActionRespawn, cause: CauseHardFailure})
	})

	t.Run("10 drained before the grace ends", func(t *testing.T) {
		l := healthyLadder(t, cfg3)
		l.NoteHardFailure()
		run(t, l,
			step{at: 5 * s, alive: true, inFlight: 2, action: ActionNone},
			step{at: 10 * s, alive: true, inFlight: 0, action: ActionRespawn})
	})

	t.Run("11 no respawn grace", func(t *testing.T) {
		cfg := cfg3
		cfg.RespawnGrace = 0
		l := healthyLadder(t, cfg)
		l.NoteHardFailure()
		run(t, l, step{at: 5 * s, alive: true, inFlight: 2, action: ActionRespawn})
	})

	t.Run("12 respawn budget spent", func(t *testing.T) {
		cfg := LadderConfig{MaxFailures: 1, MaxRespawns: 2, StartupTimeout: time.Minute}
		l := NewLadder(cfg, t0)
		run(t, l, step{at: 1 * s, action: ActionRespawn})
		l.Restarted(at(1 * s))
		run(t, l, step{at: 2 * s, action: ActionRespawn})
		l.Restarted(at(2 * s))
		run(t, l,
			step{at: 3 * s, action: ActionMarkDead, state: stateOf(StateDead)},
			step{at: 4 * s, ready: true, alive: true, action: ActionNone, state: stateOf(StateDead)})
	})

	t.Run("13 budget refills after 10 minutes of health", func(t *testing.T) {
		cfg := LadderConfig{MaxFailures: 1, MaxRespawns: 1, StartupTimeout: time.Minute}
		l := NewLadder(cfg, t0)
		run(t, l, step{at: 1 * s, action: ActionRespawn})
		l.Restarted(at(1 * s))
		run(t, l,
			step{at: 2 * s, ready: true, alive: true, action: ActionNone},
			step{at: 11 * time.Minute, ready: true, alive: true, action: ActionNone})
		if l.Respawns() != 0 {
			t.Fatalf("Respawns() = %d after 10 min of health, want 0", l.Respawns())
		}
		run(t, l, step{at: 12 * time.Minute, action: ActionRespawn})
	})

	t.Run("14 budget not refilled before 10 minutes", func(t *testing.T) {
		cfg := LadderConfig{MaxFailures: 1, MaxRespawns: 1, StartupTimeout: time.Minute}
		l := NewLadder(cfg, t0)
		run(t, l, step{at: 1 * s, action: ActionRespawn})
		l.Restarted(at(1 * s))
		run(t, l,
			step{at: 2 * s, ready: true, alive: true, action: ActionNone},
			step{at: 9 * time.Minute, ready: true, alive: true, action: ActionNone},
			step{at: 9*time.Minute + s, action: ActionMarkDead})
	})

	t.Run("15 condemned backend respawns though it answers probes", func(t *testing.T) {
		l := healthyLadder(t, cfg3)
		l.Condemn("not on assigned GPU")
		run(t, l, step{at: 5 * s, ready: true, alive: true, action: ActionRespawn, cause: "not on assigned GPU"})
		l.Restarted(at(6 * s))
		run(t, l, step{at: 7 * s, ready: true, alive: true, action: ActionNone, state: stateOf(StateHealthy)})
	})

	t.Run("16 condemnation honours the respawn grace", func(t *testing.T) {
		l := healthyLadder(t, cfg3)
		l.Condemn("not on assigned GPU")
		run(t, l,
			step{at: 5 * s, ready: true, alive: true, inFlight: 1, action: ActionNone},
			step{at: 30 * s, ready: true, alive: true, inFlight: 1, action: ActionNone},
			step{at: 66 * s, ready: true, alive: true, inFlight: 1, action: ActionRespawn})
	})

	t.Run("17 marked dead", func(t *testing.T) {
		l := NewLadder(cfg3, t0)
		l.MarkDead("cannot build command")
		run(t, l, step{at: 1 * s, ready: true, alive: true, action: ActionNone, state: stateOf(StateDead), cause: "cannot build command"})
	})
}

func TestStateString(t *testing.T) {
	for s, want := range map[State]string{StateStarting: "starting", StateHealthy: "healthy", StateUnhealthy: "unhealthy", StateDead: "dead"} {
		if got := s.String(); got != want {
			t.Errorf("State(%d).String() = %q, want %q", int(s), got, want)
		}
	}
}
