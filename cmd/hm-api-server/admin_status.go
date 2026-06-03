package main

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func (api *apiServer) handleHealthDetails(w http.ResponseWriter, r *http.Request) {
	if err := api.db.Ping(r.Context()); err != nil {
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	var res healthDetailsResponse
	res.Status = "ok"
	res.Database = "ok"
	includeCiscoSpaces := ciscoSpacesCollectorEnabled()
	err := api.db.QueryRow(r.Context(), `
		SELECT
			(SELECT count(*) FROM collector_status
			 WHERE $2
			    OR NOT (
			      collector_name = 'hm-cisco-spaces-collector'
			      AND target_type = 'cisco_spaces_firehose'
			      AND target_key = 'default'
			    )),
			(SELECT count(*) FROM collector_status
			 WHERE ($2
			        OR NOT (
			          collector_name = 'hm-cisco-spaces-collector'
			          AND target_type = 'cisco_spaces_firehose'
			          AND target_key = 'default'
			        ))
			   AND (last_success_at IS NULL
			        OR consecutive_failures > 0
			        OR updated_at < now() - $1::interval))
	`, intervalSeconds(envDuration("COLLECTOR_STATUS_STALE_AFTER", 5*time.Minute)), includeCiscoSpaces).Scan(&res.CollectorTargets, &res.StaleCollectors)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "query health details")
		return
	}
	if res.StaleCollectors > 0 {
		res.Status = "degraded"
	}
	writeJSON(w, http.StatusOK, res)
}

func (api *apiServer) handleSchema(w http.ResponseWriter, r *http.Request) {
	rows, err := api.db.Query(r.Context(), `
		SELECT version, name, checksum, applied_at
		FROM schema_migrations
		ORDER BY version
	`)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "query schema migrations")
		return
	}
	defer rows.Close()

	res := schemaResponse{Migrations: []schemaMigrationResponse{}}
	for rows.Next() {
		var item schemaMigrationResponse
		if err := rows.Scan(&item.Version, &item.Name, &item.Checksum, &item.AppliedAt); err != nil {
			writeError(w, http.StatusInternalServerError, "scan schema migration")
			return
		}
		if item.Version > res.CurrentVersion {
			res.CurrentVersion = item.Version
		}
		res.Migrations = append(res.Migrations, item)
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, "read schema migrations")
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (api *apiServer) handleCiscoSpacesFirehoseStatus(w http.ResponseWriter, r *http.Request) {
	res := ciscoSpacesFirehoseStatusResponse{
		CollectorEnabled:           ciscoSpacesCollectorEnabled(),
		LockKey:                    ciscoSpacesFirehoseLockKey,
		ConfiguredSecondaryAllowed: envBool("CISCO_SPACES_ALLOW_SECONDARY", false),
	}
	classID, objID := advisoryLockParts(ciscoSpacesFirehoseLockKey)
	var pid pgtype.Int4
	err := api.db.QueryRow(r.Context(), `
		SELECT pid
		FROM pg_locks
		WHERE locktype = 'advisory'
		  AND mode = 'ExclusiveLock'
		  AND granted
		  AND classid::bigint = $1
		  AND objid::bigint = $2
		  AND objsubid = 1
		LIMIT 1
	`, classID, objID).Scan(&pid)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusInternalServerError, "query Cisco Spaces advisory lock")
		return
	}
	if pid.Valid {
		res.LockHeld = true
		res.LockHolderPID = &pid.Int32
	}

	var lastAttemptAt, lastSuccessAt, lastDataAt, firstFailureAt, lastFailureAt pgtype.Timestamptz
	var updatedAt pgtype.Timestamptz
	err = api.db.QueryRow(r.Context(), `
		SELECT
			last_attempt_at,
			last_success_at,
			last_data_at,
			first_failure_at,
			last_failure_at,
			COALESCE(last_error, ''),
			consecutive_failures,
			updated_at
		FROM collector_status
		WHERE collector_name = 'hm-cisco-spaces-collector'
		  AND target_type = 'cisco_spaces_firehose'
		  AND target_key = 'default'
	`).Scan(
		&lastAttemptAt,
		&lastSuccessAt,
		&lastDataAt,
		&firstFailureAt,
		&lastFailureAt,
		&res.LastError,
		&res.ConsecutiveFailures,
		&updatedAt,
	)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusInternalServerError, "query Cisco Spaces collector status")
		return
	}
	if err == nil {
		res.CollectorStatusPresent = true
		res.LastAttemptAt = timePtrFromPg(lastAttemptAt)
		res.LastSuccessAt = timePtrFromPg(lastSuccessAt)
		res.LastDataAt = timePtrFromPg(lastDataAt)
		res.FirstFailureAt = timePtrFromPg(firstFailureAt)
		res.LastFailureAt = timePtrFromPg(lastFailureAt)
		res.CollectorUpdatedAt = timePtrFromPg(updatedAt)
		if res.CollectorUpdatedAt != nil {
			window := envDuration("COLLECTOR_STATUS_STALE_AFTER", 5*time.Minute)
			res.CollectorReporting = time.Since(*res.CollectorUpdatedAt) < window
		}
	}
	res.Mode = "disabled"
	if !res.CollectorEnabled {
		writeJSON(w, http.StatusOK, res)
		return
	}
	if res.LockHeld {
		res.Mode = "primary-lock-held"
	} else if res.CollectorReporting {
		res.Mode = "secondary-or-unlocked-reporting"
	} else if res.ConfiguredSecondaryAllowed {
		res.Mode = "secondary-allowed-idle"
	}
	writeJSON(w, http.StatusOK, res)
}

func ciscoSpacesCollectorEnabled() bool {
	return envBool("CISCO_SPACES_COLLECTOR_ENABLED", false)
}

func (api *apiServer) handleCollectorStatus(w http.ResponseWriter, r *http.Request) {
	rows, err := api.db.Query(r.Context(), `
		SELECT
			collector_name,
			target_type,
			target_key,
			last_attempt_at,
			last_success_at,
			last_data_at,
			first_failure_at,
			last_failure_at,
			COALESCE(last_error, ''),
			consecutive_failures,
			updated_at
		FROM collector_status
		ORDER BY collector_name, target_type, target_key
	`)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "query collector status")
		return
	}
	defer rows.Close()

	items := []collectorStatusResponse{}
	for rows.Next() {
		var item collectorStatusResponse
		var lastAttemptAt, lastSuccessAt, lastDataAt, firstFailureAt, lastFailureAt pgtype.Timestamptz
		if err := rows.Scan(
			&item.CollectorName,
			&item.TargetType,
			&item.TargetKey,
			&lastAttemptAt,
			&lastSuccessAt,
			&lastDataAt,
			&firstFailureAt,
			&lastFailureAt,
			&item.LastError,
			&item.ConsecutiveFailures,
			&item.UpdatedAt,
		); err != nil {
			writeError(w, http.StatusInternalServerError, "scan collector status")
			return
		}
		item.LastAttemptAt = timePtrFromPg(lastAttemptAt)
		item.LastSuccessAt = timePtrFromPg(lastSuccessAt)
		item.LastDataAt = timePtrFromPg(lastDataAt)
		item.FirstFailureAt = timePtrFromPg(firstFailureAt)
		item.LastFailureAt = timePtrFromPg(lastFailureAt)
		item.Expected = collectorExpected(item.CollectorName, item.TargetType, item.TargetKey)
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, "read collector status")
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func intervalSeconds(duration time.Duration) string {
	return fmt.Sprintf("%f seconds", duration.Seconds())
}

func collectorExpected(name string, targetType string, targetKey string) bool {
	if name == "hm-cisco-spaces-collector" && targetType == "cisco_spaces_firehose" && targetKey == "default" {
		return ciscoSpacesCollectorEnabled()
	}
	return true
}
