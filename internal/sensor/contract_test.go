package sensor

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestMetricContractCoversSchemaAPIWebAndRollups(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve contract test path")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	schema := readContractFile(t, filepath.Join(root, "db", "schema.sql"))
	openAPI := readContractFile(t, filepath.Join(root, "docs", "openapi.yaml"))
	web := readContractFile(t, filepath.Join(root, "web", "index.html"))
	migration := readContractFile(t, filepath.Join(root, "db", "migrations", "0017_add_weighted_rollup_counts.sql"))

	for _, metric := range Metrics {
		if count := strings.Count(schema, metric.Column+" double precision"); count != 4 {
			t.Errorf("schema column %s appears %d times, want sensor_minute plus three rollups", metric.Column, count)
		}
		if count := strings.Count(schema, metric.Column+"_count bigint"); count != 3 {
			t.Errorf("schema count column %s_count appears %d times, want three rollups", metric.Column, count)
		}
		if !strings.Contains(openAPI, "- "+metric.Key) {
			t.Errorf("OpenAPI SensorMetric is missing %s", metric.Key)
		}
		if !strings.Contains(web, "key: '"+metric.Key+"'") {
			t.Errorf("web metric registry is missing %s", metric.Key)
		}
		if strings.Count(migration, metric.Column+"_count bigint") != 3 {
			t.Errorf("weighted rollup migration is missing %s_count", metric.Column)
		}
	}
}

func readContractFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}
