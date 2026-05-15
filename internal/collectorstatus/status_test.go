package collectorstatus

import "testing"

func TestTargetNormalizedDefaultsTargetKey(t *testing.T) {
	target := Target{
		CollectorName: "collector",
		TargetType:    "target",
	}.normalized()

	if target.TargetKey != "default" {
		t.Fatalf("TargetKey = %q, want default", target.TargetKey)
	}
}

func TestTargetNormalizedDefaultsBlankTargetKey(t *testing.T) {
	target := Target{
		CollectorName: "collector",
		TargetType:    "target",
		TargetKey:     "   ",
	}.normalized()

	if target.TargetKey != "default" {
		t.Fatalf("TargetKey = %q, want default", target.TargetKey)
	}
}

func TestTruncatePreservesUTF8(t *testing.T) {
	value := truncate("abcあいう", 5)
	if value != "abcあい" {
		t.Fatalf("truncate() = %q, want abcあい", value)
	}
}
