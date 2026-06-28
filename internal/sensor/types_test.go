package sensor

import "testing"

func TestNormalizeMAC(t *testing.T) {
	for _, input := range []string{"5C:85:7E:14:73:7D", "5c-85-7e-14-73-7d", "5c857e14737d"} {
		if got, want := NormalizeMAC(input), "5c:85:7e:14:73:7d"; got != want {
			t.Fatalf("NormalizeMAC(%q) = %q, want %q", input, got, want)
		}
	}
	if got := NormalizeMAC("not-a-mac"); got != "" {
		t.Fatalf("invalid MAC normalized to %q", got)
	}
}

func TestMetricKeysAndColumnsAreUnique(t *testing.T) {
	keys := map[string]bool{}
	columns := map[string]bool{}
	for _, metric := range Metrics {
		if keys[metric.Key] {
			t.Fatalf("duplicate metric key %q", metric.Key)
		}
		if columns[metric.Column] {
			t.Fatalf("duplicate metric column %q", metric.Column)
		}
		keys[metric.Key] = true
		columns[metric.Column] = true
	}
}
