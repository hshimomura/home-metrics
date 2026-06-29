package sensoralert

import (
	"testing"
	"time"
)

func TestEvaluateRequiresFreshDistinctObservationsForDuration(t *testing.T) {
	base := time.Date(2026, 6, 29, 0, 0, 0, 0, time.UTC)
	rule := Rule{Direction: DirectionAbove, TriggerThreshold: 30, ClearThreshold: 29, ForDuration: 5 * time.Minute, MaxDataAge: 10 * time.Minute, Enabled: true}

	first := Evaluate(base, rule, true, State{}, &Observation{Time: base, Value: 31})
	if first.State.Status != StatusPending || first.Event != nil {
		t.Fatalf("first result = %+v, want pending without event", first)
	}

	repeated := Evaluate(base.Add(6*time.Minute), rule, true, first.State, &Observation{Time: base, Value: 31})
	if repeated.State.Status != StatusPending || repeated.Event != nil {
		t.Fatalf("repeated result = %+v, want pending without event", repeated)
	}

	firing := Evaluate(base.Add(6*time.Minute), rule, true, repeated.State, &Observation{Time: base.Add(6 * time.Minute), Value: 31.5})
	if firing.State.Status != StatusFiring || firing.Event == nil || firing.Event.Type != "firing" {
		t.Fatalf("firing result = %+v, want firing event", firing)
	}
}

func TestEvaluateResetsPendingAfterObservationGap(t *testing.T) {
	base := time.Date(2026, 6, 29, 0, 0, 0, 0, time.UTC)
	rule := Rule{Direction: DirectionAbove, TriggerThreshold: 30, ClearThreshold: 29, ForDuration: 5 * time.Minute, MaxDataAge: 10 * time.Minute, Enabled: true}
	first := Evaluate(base, rule, true, State{}, &Observation{Time: base, Value: 31})

	now := base.Add(21 * time.Minute)
	result := Evaluate(now, rule, true, first.State, &Observation{Time: now, Value: 31.5})
	if result.State.Status != StatusPending || result.Event != nil {
		t.Fatalf("gap result = %+v, want restarted pending state", result)
	}
	if result.State.PendingSince == nil || !result.State.PendingSince.Equal(now) {
		t.Fatalf("pending since = %v, want %s", result.State.PendingSince, now)
	}
}

func TestEvaluateUsesHysteresisAndOnlyEmitsTransitions(t *testing.T) {
	base := time.Date(2026, 6, 29, 0, 0, 0, 0, time.UTC)
	rule := Rule{Direction: DirectionAbove, TriggerThreshold: 30, ClearThreshold: 29, MaxDataAge: 10 * time.Minute, Enabled: true}

	firing := Evaluate(base, rule, true, State{}, &Observation{Time: base, Value: 30})
	stillFiring := Evaluate(base.Add(time.Minute), rule, true, firing.State, &Observation{Time: base.Add(time.Minute), Value: 29.5})
	if stillFiring.State.Status != StatusFiring || stillFiring.Event != nil {
		t.Fatalf("middle-band result = %+v, want firing without event", stillFiring)
	}

	resolved := Evaluate(base.Add(2*time.Minute), rule, true, stillFiring.State, &Observation{Time: base.Add(2 * time.Minute), Value: 29})
	if resolved.State.Status != StatusNormal || resolved.Event == nil || resolved.Event.Type != "resolved" {
		t.Fatalf("resolved result = %+v, want resolved event", resolved)
	}
}

func TestEvaluateBelowThreshold(t *testing.T) {
	base := time.Date(2026, 6, 29, 0, 0, 0, 0, time.UTC)
	rule := Rule{Direction: DirectionBelow, TriggerThreshold: 15, ClearThreshold: 18, MaxDataAge: 10 * time.Minute, Enabled: true}

	firing := Evaluate(base, rule, true, State{}, &Observation{Time: base, Value: 15})
	if firing.State.Status != StatusFiring {
		t.Fatalf("status = %s, want firing", firing.State.Status)
	}
	resolved := Evaluate(base.Add(time.Minute), rule, true, firing.State, &Observation{Time: base.Add(time.Minute), Value: 18})
	if resolved.State.Status != StatusNormal || resolved.Event == nil {
		t.Fatalf("resolved result = %+v", resolved)
	}
}

func TestEvaluateStaleDataDoesNotChangeFiringState(t *testing.T) {
	base := time.Date(2026, 6, 29, 0, 0, 0, 0, time.UTC)
	rule := Rule{Direction: DirectionAbove, TriggerThreshold: 30, ClearThreshold: 29, MaxDataAge: 10 * time.Minute, Enabled: true}
	state := State{Status: StatusFiring}

	result := Evaluate(base, rule, true, state, &Observation{Time: base.Add(-11 * time.Minute), Value: 20})
	if result.State.Status != StatusFiring || result.State.EvaluationStatus != EvaluationStale || result.Event != nil {
		t.Fatalf("stale result = %+v, want firing/stale without event", result)
	}
}

func TestEvaluateDisabledRuleResolvesFiringAlert(t *testing.T) {
	base := time.Date(2026, 6, 29, 0, 0, 0, 0, time.UTC)
	rule := Rule{Direction: DirectionAbove, TriggerThreshold: 30, ClearThreshold: 29, Enabled: false}
	state := State{Status: StatusFiring}

	result := Evaluate(base, rule, true, state, nil)
	if result.State.Status != StatusNormal || result.State.EvaluationStatus != EvaluationDisabled {
		t.Fatalf("disabled result = %+v", result)
	}
	if result.Event == nil || result.Event.Reason != "rule_disabled" {
		t.Fatalf("event = %+v, want rule_disabled resolution", result.Event)
	}
}

func TestEvaluateNoDataPreservesState(t *testing.T) {
	base := time.Date(2026, 6, 29, 0, 0, 0, 0, time.UTC)
	rule := Rule{Direction: DirectionAbove, Enabled: true}
	state := State{Status: StatusPending}

	result := Evaluate(base, rule, true, state, nil)
	if result.State.Status != StatusPending || result.State.EvaluationStatus != EvaluationNoData || result.Event != nil {
		t.Fatalf("no-data result = %+v", result)
	}
}
