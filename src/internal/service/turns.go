package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/takezoh/syntoria-reach/src/internal/core"
)

type turnPersistenceError struct{ err error }

func (e *turnPersistenceError) Error() string { return e.err.Error() }

// WatchTurns resumes observations immediately and on each scheduled tick.
// The caller owns the schedule and cancels the context on shutdown.
func (s *Service) WatchTurns(ctx context.Context, ticks <-chan time.Time) {
	for {
		if err := s.ObserveTurns(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("observe turns", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case _, ok := <-ticks:
			if !ok {
				return
			}
		}
	}
}

// ObserveTurns reads each armed turn once. The caller schedules subsequent reads.
func (s *Service) ObserveTurns(ctx context.Context) error {
	s.turnMu.Lock()
	defer s.turnMu.Unlock()
	turns, err := s.store.ArmedTurns()
	if err != nil {
		return err
	}
	var failures []error
	for _, t := range turns {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.observeTurn(ctx, t); err != nil {
			failures = append(failures, fmt.Errorf("session %s: %w", t.SessionID, err))
		}
	}
	return errors.Join(failures...)
}
func (s *Service) observeSessionTurns(ctx context.Context, id string) error {
	turns, err := s.store.ArmedTurns()
	if err != nil {
		return err
	}
	for _, t := range turns {
		if t.SessionID == id {
			if err := s.observeTurn(ctx, t); err != nil {
				return err
			}
		}
	}
	turns, err = s.store.ArmedTurns()
	if err != nil {
		return err
	}
	for _, t := range turns {
		if t.SessionID == id {
			return core.Errorf(core.CodeInvalidState, "previous turn has not stopped")
		}
	}
	return nil
}
func (s *Service) observeTurn(ctx context.Context, t core.Turn) error {
	st, err := s.workspaces.Session(ctx, t.SessionID)
	if errors.Is(err, core.ErrNotFound) {
		if err := s.store.DisarmTurn(t); err != nil {
			return err
		}
		slog.Warn("armed turn session lost", "session", t.SessionID)
		return nil
	}
	if err != nil {
		return err
	}
	switch st.State {
	case core.AgentIdle, core.AgentWaitingOnHuman, core.AgentExited:
		return s.closeTurn(t, st.State)
	}
	return nil
}

// TurnStopChanges signals that the durable outbox may have new records.
func (s *Service) TurnStopChanges() <-chan struct{} { return s.turnStops }

// PendingTurnStops returns committed occurrences in their durable order.
func (s *Service) PendingTurnStops() ([]core.TurnStop, error) { return s.store.PendingTurnStops() }

// AckTurnStop removes only the occurrence acknowledged by the gateway.
func (s *Service) AckTurnStop(id string) error { return s.store.DeleteTurnStop(id) }

// closeTurn signals the relay only after the occurrence is committed.
func (s *Service) closeTurn(t core.Turn, state string) error {
	err := s.store.CloseTurn(t, core.TurnStop{EventID: s.newID("event_"), SessionID: t.SessionID, State: state, ObservedAt: s.now()})
	if err == nil {
		select {
		case s.turnStops <- struct{}{}:
		default:
		}
	}
	return err
}

// ReconcilePendingTurns records the observation gap before admitting prompts.
// When turns are observed by reading the session, a session still working
// is armed again, so the stop of what it runs is observed. A session with an
// armed turn already observes the turn it runs.
func (s *Service) ReconcilePendingTurns(ctx context.Context) error {
	turns, err := s.store.PendingTurns()
	if err != nil {
		return err
	}
	armed, err := s.store.ArmedTurns()
	if err != nil {
		return err
	}
	observed := map[string]bool{}
	for _, t := range armed {
		observed[t.SessionID] = true
	}
	for _, t := range turns {
		stop := core.TurnStop{EventID: s.newID("event_"), SessionID: t.SessionID, State: core.TurnUnknown, ObservedAt: s.now()}
		if s.observeTurns && !observed[t.SessionID] && s.working(ctx, t.SessionID) {
			err = s.store.ArmPendingTurn(t, stop)
			observed[t.SessionID] = true
		} else {
			err = s.store.ClosePendingTurn(t, stop)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// working reports whether the session is seen working. A failed read is
// not evidence of a running turn.
func (s *Service) working(ctx context.Context, id string) bool {
	st, err := s.workspaces.Session(ctx, id)
	if err != nil {
		if !errors.Is(err, core.ErrNotFound) {
			slog.Warn("cannot read the session of a turn left pending", "session", id, "err", err)
		}
		return false
	}
	return st.State == core.AgentWorking
}
