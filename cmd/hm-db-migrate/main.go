package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const defaultDBDSN = "dbname=ble_sensors host=/var/run/postgresql"

type migration struct {
	Version  int64
	Name     string
	Path     string
	SQL      string
	Checksum string
}

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := envString("BLE_DB_DSN", defaultDBDSN)
	dir := envString("DB_MIGRATIONS_DIR", "db/migrations")
	db, err := pgx.Connect(ctx, dsn)
	if err != nil {
		log.Fatalf("connect database: %v", err)
	}
	defer db.Close(context.Background())

	if err := ensureMigrationTable(ctx, db); err != nil {
		log.Fatalf("ensure migration table: %v", err)
	}
	migrations, err := loadMigrations(dir)
	if err != nil {
		log.Fatalf("load migrations: %v", err)
	}
	for _, migration := range migrations {
		applied, err := appliedMigration(ctx, db, migration)
		if err != nil {
			log.Fatalf("check migration %d: %v", migration.Version, err)
		}
		if applied {
			log.Printf("migration %04d already applied: %s", migration.Version, migration.Name)
			continue
		}
		if err := applyMigration(ctx, db, migration); err != nil {
			log.Fatalf("apply migration %d %s: %v", migration.Version, migration.Name, err)
		}
		log.Printf("migration %04d applied: %s", migration.Version, migration.Name)
	}
}

func ensureMigrationTable(ctx context.Context, db *pgx.Conn) error {
	_, err := db.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version bigint PRIMARY KEY,
			name text NOT NULL,
			checksum text NOT NULL,
			applied_at timestamptz NOT NULL DEFAULT now()
		)
	`)
	return err
}

func loadMigrations(dir string) ([]migration, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var migrations []migration
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		version, name, err := parseMigrationName(entry.Name())
		if err != nil {
			return nil, err
		}
		path := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(data)
		migrations = append(migrations, migration{
			Version:  version,
			Name:     name,
			Path:     path,
			SQL:      string(data),
			Checksum: hex.EncodeToString(sum[:]),
		})
	}
	sort.Slice(migrations, func(i, j int) bool {
		return migrations[i].Version < migrations[j].Version
	})
	return migrations, nil
}

func parseMigrationName(name string) (int64, string, error) {
	base := strings.TrimSuffix(name, ".sql")
	parts := strings.SplitN(base, "_", 2)
	if len(parts) != 2 {
		return 0, "", fmt.Errorf("invalid migration filename %q", name)
	}
	version, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, "", fmt.Errorf("invalid migration version %q: %w", name, err)
	}
	if version <= 0 {
		return 0, "", fmt.Errorf("invalid migration version %q", name)
	}
	return version, parts[1], nil
}

func appliedMigration(ctx context.Context, db *pgx.Conn, migration migration) (bool, error) {
	var checksum string
	err := db.QueryRow(ctx, `SELECT checksum FROM schema_migrations WHERE version = $1`, migration.Version).Scan(&checksum)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if checksum != migration.Checksum {
		return false, fmt.Errorf("checksum mismatch for migration %d", migration.Version)
	}
	return true, nil
}

func applyMigration(ctx context.Context, db *pgx.Conn, migration migration) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	statements, err := splitSQLStatements(migration.SQL)
	if err != nil {
		return err
	}
	for _, statement := range statements {
		if _, err := tx.Exec(ctx, statement); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO schema_migrations (version, name, checksum)
		VALUES ($1, $2, $3)
	`, migration.Version, migration.Name, migration.Checksum); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func splitSQLStatements(sql string) ([]string, error) {
	var statements []string
	start := 0
	inSingleQuote := false
	inDoubleQuote := false
	inLineComment := false
	inBlockComment := false
	dollarQuote := ""

	for i := 0; i < len(sql); {
		if inLineComment {
			if sql[i] == '\n' {
				inLineComment = false
			}
			i++
			continue
		}
		if inBlockComment {
			if i+1 < len(sql) && sql[i] == '*' && sql[i+1] == '/' {
				inBlockComment = false
				i += 2
				continue
			}
			i++
			continue
		}
		if dollarQuote != "" {
			if strings.HasPrefix(sql[i:], dollarQuote) {
				i += len(dollarQuote)
				dollarQuote = ""
				continue
			}
			i++
			continue
		}
		if inSingleQuote {
			if sql[i] == '\'' {
				if i+1 < len(sql) && sql[i+1] == '\'' {
					i += 2
					continue
				}
				inSingleQuote = false
			}
			i++
			continue
		}
		if inDoubleQuote {
			if sql[i] == '"' {
				if i+1 < len(sql) && sql[i+1] == '"' {
					i += 2
					continue
				}
				inDoubleQuote = false
			}
			i++
			continue
		}

		switch sql[i] {
		case '-':
			if i+1 < len(sql) && sql[i+1] == '-' {
				inLineComment = true
				i += 2
				continue
			}
		case '/':
			if i+1 < len(sql) && sql[i+1] == '*' {
				inBlockComment = true
				i += 2
				continue
			}
		case '\'':
			inSingleQuote = true
		case '"':
			inDoubleQuote = true
		case '$':
			if tag, ok := readDollarQuoteTag(sql[i:]); ok {
				dollarQuote = tag
				i += len(tag)
				continue
			}
		case ';':
			statement := strings.TrimSpace(sql[start:i])
			if statement != "" {
				statements = append(statements, statement)
			}
			start = i + 1
		}
		i++
	}
	if inBlockComment {
		return nil, errors.New("unterminated block comment in migration")
	}
	if dollarQuote != "" {
		return nil, fmt.Errorf("unterminated dollar quote %s in migration", dollarQuote)
	}
	statement := strings.TrimSpace(sql[start:])
	if statement != "" {
		statements = append(statements, statement)
	}
	return statements, nil
}

func readDollarQuoteTag(sql string) (string, bool) {
	if sql == "" || sql[0] != '$' {
		return "", false
	}
	for i := 1; i < len(sql); i++ {
		if sql[i] == '$' {
			return sql[:i+1], true
		}
		if !isDollarQuoteTagChar(sql[i]) {
			return "", false
		}
	}
	return "", false
}

func isDollarQuoteTagChar(char byte) bool {
	return char == '_' ||
		(char >= 'a' && char <= 'z') ||
		(char >= 'A' && char <= 'Z') ||
		(char >= '0' && char <= '9')
}

func envString(key string, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}
