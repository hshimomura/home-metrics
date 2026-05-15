package main

import (
	"reflect"
	"testing"
)

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
