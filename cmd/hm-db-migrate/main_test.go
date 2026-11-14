package main

import (
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
)

func TestSchemaMigrationsParse(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test path")
	}
	dir := filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", "db", "migrations"))
	migrations, err := loadMigrations(dir)
	if err != nil {
		t.Fatalf("load migrations: %v", err)
	}
	if len(migrations) < 3 || migrations[0].Version != 1 || migrations[0].Name != "initial_schema" {
		t.Fatalf("migrations = %+v, want baseline and additive motion migration", migrations)
	}
	if migrations[1].Version != 2 || migrations[1].Name != "shelly_motion" {
		t.Fatal("missing motion migration")
	}
	for _, m := range migrations {
		if _, err := splitSQLStatements(m.SQL); err != nil {
			t.Fatalf("parse %s: %v", m.Name, err)
		}
	}
	statements, err := splitSQLStatements(migrations[0].SQL)
	if err != nil {
		t.Fatalf("parse initial schema: %v", err)
	}
	if len(statements) < 20 {
		t.Fatalf("initial schema statements = %d, want complete schema", len(statements))
	}
}

func TestSplitSQLStatementsKeepsQuotedSemicolons(t *testing.T) {
	input := `
		CREATE TABLE example (value text DEFAULT 'a;b');
		INSERT INTO example VALUES ($$c;d$$);
		-- comment with ;
		SELECT "semi;colon" FROM example;
	`
	got, err := splitSQLStatements(input)
	if err != nil {
		t.Fatalf("splitSQLStatements returned error: %v", err)
	}
	want := []string{
		"CREATE TABLE example (value text DEFAULT 'a;b')",
		"INSERT INTO example VALUES ($$c;d$$)",
		`-- comment with ;
		SELECT "semi;colon" FROM example`,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("statements mismatch\nwant: %#v\n got: %#v", want, got)
	}
}

func TestSplitSQLStatementsReportsUnterminatedDollarQuote(t *testing.T) {
	if _, err := splitSQLStatements("SELECT $body$unterminated;"); err == nil {
		t.Fatal("expected unterminated dollar quote error")
	}
}
