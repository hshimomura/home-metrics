package main

import (
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

const ciscoSpacesFirehoseLockKey int64 = 734829148912345

type collectorStatusResponse struct {
	CollectorName       string     `json:"collector_name"`
	TargetType          string     `json:"target_type"`
	TargetKey           string     `json:"target_key"`
	LastAttemptAt       *time.Time `json:"last_attempt_at,omitempty"`
	LastSuccessAt       *time.Time `json:"last_success_at,omitempty"`
	LastDataAt          *time.Time `json:"last_data_at,omitempty"`
	FirstFailureAt      *time.Time `json:"first_failure_at,omitempty"`
	LastFailureAt       *time.Time `json:"last_failure_at,omitempty"`
	LastError           string     `json:"last_error,omitempty"`
	ConsecutiveFailures int        `json:"consecutive_failures"`
	UpdatedAt           time.Time  `json:"updated_at"`
}

type healthDetailsResponse struct {
	Status           string `json:"status"`
	Database         string `json:"database"`
	CollectorTargets int    `json:"collector_targets"`
	StaleCollectors  int    `json:"stale_collectors"`
}

type schemaMigrationResponse struct {
	Version   int64     `json:"version"`
	Name      string    `json:"name"`
	Checksum  string    `json:"checksum"`
	AppliedAt time.Time `json:"applied_at"`
}

type schemaResponse struct {
	CurrentVersion int64                     `json:"current_version"`
	Migrations     []schemaMigrationResponse `json:"migrations"`
}

type ciscoSpacesFirehoseStatusResponse struct {
	LockKey                    int64      `json:"lock_key"`
	LockHeld                   bool       `json:"lock_held"`
	LockHolderPID              *int32     `json:"lock_holder_pid,omitempty"`
	Mode                       string     `json:"mode"`
	ConfiguredSecondaryAllowed bool       `json:"configured_secondary_allowed"`
	CollectorStatusPresent     bool       `json:"collector_status_present"`
	CollectorReporting         bool       `json:"collector_reporting"`
	CollectorUpdatedAt         *time.Time `json:"collector_updated_at,omitempty"`
	LastAttemptAt              *time.Time `json:"last_attempt_at,omitempty"`
	LastSuccessAt              *time.Time `json:"last_success_at,omitempty"`
	LastDataAt                 *time.Time `json:"last_data_at,omitempty"`
	FirstFailureAt             *time.Time `json:"first_failure_at,omitempty"`
	LastFailureAt              *time.Time `json:"last_failure_at,omitempty"`
	ConsecutiveFailures        int        `json:"consecutive_failures"`
	LastError                  string     `json:"last_error,omitempty"`
}

func advisoryLockParts(key int64) (int64, int64) {
	value := uint64(key)
	return int64(uint32(value >> 32)), int64(uint32(value))
}

func timePtrFromPg(value pgtype.Timestamptz) *time.Time {
	if !value.Valid {
		return nil
	}
	return &value.Time
}
