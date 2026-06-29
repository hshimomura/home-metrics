package main

import (
	"math"
	"testing"
)

func TestValidateSensorAlertRule(t *testing.T) {
	trigger := 30.0
	clear := 29.0
	forSeconds := int64(300)
	maxAge := int64(600)
	enabled := true
	rule, err := validateSensorAlertRule(sensorAlertRuleRequest{
		Name: "Env high temperature", MAC: "D3:8D:7F:32:1E:65", Metric: "temperature_c",
		Direction: "above", TriggerThreshold: &trigger, ClearThreshold: &clear,
		ForDurationSeconds: &forSeconds, MaxDataAgeSeconds: &maxAge,
		Severity: "warning", Enabled: &enabled,
	})
	if err != nil {
		t.Fatalf("validate rule: %v", err)
	}
	if rule.MAC != "d3:8d:7f:32:1e:65" || rule.ForDurationSeconds != 300 {
		t.Fatalf("validated rule = %+v", rule)
	}
}

func TestValidateSensorAlertRuleRejectsInvalidHysteresis(t *testing.T) {
	trigger := 30.0
	clear := 30.0
	zero := int64(0)
	maxAge := int64(600)
	enabled := true
	_, err := validateSensorAlertRule(sensorAlertRuleRequest{
		Name: "bad", MAC: "aa:bb:cc:dd:ee:ff", Metric: "temperature_c",
		Direction: "above", TriggerThreshold: &trigger, ClearThreshold: &clear,
		ForDurationSeconds: &zero, MaxDataAgeSeconds: &maxAge,
		Severity: "warning", Enabled: &enabled,
	})
	if err == nil {
		t.Fatal("expected hysteresis validation error")
	}
}

func TestValidateSensorAlertRuleRejectsUnknownMetricAndNonFiniteThreshold(t *testing.T) {
	trigger := math.NaN()
	clear := 1.0
	zero := int64(0)
	maxAge := int64(600)
	enabled := true
	_, err := validateSensorAlertRule(sensorAlertRuleRequest{
		Name: "bad", MAC: "aa:bb:cc:dd:ee:ff", Metric: "unknown",
		Direction: "above", TriggerThreshold: &trigger, ClearThreshold: &clear,
		ForDurationSeconds: &zero, MaxDataAgeSeconds: &maxAge,
		Severity: "warning", Enabled: &enabled,
	})
	if err == nil {
		t.Fatal("expected validation error")
	}
}
