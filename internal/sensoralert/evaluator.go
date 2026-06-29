package sensoralert

import "time"

type Direction string

const (
	DirectionAbove Direction = "above"
	DirectionBelow Direction = "below"
)

type Status string

const (
	StatusNormal  Status = "normal"
	StatusPending Status = "pending"
	StatusFiring  Status = "firing"
)

type EvaluationStatus string

const (
	EvaluationOK       EvaluationStatus = "ok"
	EvaluationStale    EvaluationStatus = "stale"
	EvaluationNoData   EvaluationStatus = "no_data"
	EvaluationDisabled EvaluationStatus = "disabled"
)

type Rule struct {
	Direction        Direction
	TriggerThreshold float64
	ClearThreshold   float64
	ForDuration      time.Duration
	MaxDataAge       time.Duration
	Enabled          bool
}

type State struct {
	Status           Status
	PendingSince     *time.Time
	FiredAt          *time.Time
	ResolvedAt       *time.Time
	LastValue        *float64
	LastValueAt      *time.Time
	LastEvaluatedAt  *time.Time
	EvaluationStatus EvaluationStatus
}

type Observation struct {
	Time  time.Time
	Value float64
}

type Event struct {
	Type    string
	Reason  string
	Value   *float64
	ValueAt *time.Time
}

type Result struct {
	State State
	Event *Event
}

func Evaluate(now time.Time, rule Rule, deviceEnabled bool, current State, observation *Observation) Result {
	state := current
	if state.Status == "" {
		state.Status = StatusNormal
	}
	state.LastEvaluatedAt = timePointer(now)

	if !rule.Enabled || !deviceEnabled {
		result := Result{State: state}
		if state.Status == StatusFiring {
			result.Event = &Event{Type: "resolved", Reason: disabledReason(rule.Enabled), Value: state.LastValue, ValueAt: state.LastValueAt}
			result.State.ResolvedAt = timePointer(now)
		}
		result.State.Status = StatusNormal
		result.State.PendingSince = nil
		result.State.EvaluationStatus = EvaluationDisabled
		return result
	}

	if observation == nil {
		state.EvaluationStatus = EvaluationNoData
		return Result{State: state}
	}

	state.LastValue = floatPointer(observation.Value)
	state.LastValueAt = timePointer(observation.Time)
	if rule.MaxDataAge > 0 && now.Sub(observation.Time) > rule.MaxDataAge {
		state.EvaluationStatus = EvaluationStale
		return Result{State: state}
	}
	state.EvaluationStatus = EvaluationOK

	if current.LastValueAt != nil && !observation.Time.After(*current.LastValueAt) {
		return Result{State: state}
	}

	switch state.Status {
	case StatusFiring:
		if clears(rule, observation.Value) {
			state.Status = StatusNormal
			state.PendingSince = nil
			state.ResolvedAt = timePointer(now)
			return Result{
				State: state,
				Event: &Event{Type: "resolved", Reason: "threshold_cleared", Value: floatPointer(observation.Value), ValueAt: timePointer(observation.Time)},
			}
		}
	case StatusPending:
		if !triggers(rule, observation.Value) {
			state.Status = StatusNormal
			state.PendingSince = nil
			return Result{State: state}
		}
		if state.PendingSince == nil {
			state.PendingSince = timePointer(observation.Time)
		}
		if current.LastValueAt != nil && rule.MaxDataAge > 0 && observation.Time.Sub(*current.LastValueAt) > rule.MaxDataAge {
			state.PendingSince = timePointer(observation.Time)
			return Result{State: state}
		}
		if observation.Time.Sub(*state.PendingSince) >= rule.ForDuration {
			state.Status = StatusFiring
			state.FiredAt = timePointer(now)
			state.ResolvedAt = nil
			return Result{
				State: state,
				Event: &Event{Type: "firing", Reason: "threshold_crossed", Value: floatPointer(observation.Value), ValueAt: timePointer(observation.Time)},
			}
		}
	default:
		state.Status = StatusNormal
		if triggers(rule, observation.Value) {
			if rule.ForDuration <= 0 {
				state.Status = StatusFiring
				state.FiredAt = timePointer(now)
				state.ResolvedAt = nil
				return Result{
					State: state,
					Event: &Event{Type: "firing", Reason: "threshold_crossed", Value: floatPointer(observation.Value), ValueAt: timePointer(observation.Time)},
				}
			}
			state.Status = StatusPending
			state.PendingSince = timePointer(observation.Time)
		}
	}
	return Result{State: state}
}

func triggers(rule Rule, value float64) bool {
	if rule.Direction == DirectionBelow {
		return value <= rule.TriggerThreshold
	}
	return value >= rule.TriggerThreshold
}

func clears(rule Rule, value float64) bool {
	if rule.Direction == DirectionBelow {
		return value >= rule.ClearThreshold
	}
	return value <= rule.ClearThreshold
}

func disabledReason(ruleEnabled bool) string {
	if !ruleEnabled {
		return "rule_disabled"
	}
	return "device_disabled"
}

func timePointer(value time.Time) *time.Time {
	return &value
}

func floatPointer(value float64) *float64 {
	return &value
}
