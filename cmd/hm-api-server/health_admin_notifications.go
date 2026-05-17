package main

import (
	"context"
	"net/http"

	"github.com/jackc/pgx/v5/pgtype"
)

func (api *apiServer) handleHealthNotificationEvents(w http.ResponseWriter, r *http.Request) {
	rows, err := api.db.Query(r.Context(), `
		SELECT
			id,
			event_id,
			alert_key,
			channel_id,
			channel_type,
			status,
			http_status,
			COALESCE(response_body, ''),
			COALESCE(error, ''),
			created_at
		FROM health_notification_events
		ORDER BY created_at DESC
		LIMIT 200
	`)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "query health notification events")
		return
	}
	defer rows.Close()

	items := []healthNotificationEventResponse{}
	for rows.Next() {
		var item healthNotificationEventResponse
		var channelID pgtype.Int8
		var httpStatus pgtype.Int4
		if err := rows.Scan(
			&item.ID,
			&item.EventID,
			&item.AlertKey,
			&channelID,
			&item.ChannelType,
			&item.Status,
			&httpStatus,
			&item.ResponseBody,
			&item.Error,
			&item.CreatedAt,
		); err != nil {
			writeError(w, http.StatusInternalServerError, "scan health notification event")
			return
		}
		if channelID.Valid {
			item.ChannelID = &channelID.Int64
		}
		if httpStatus.Valid {
			item.HTTPStatus = &httpStatus.Int32
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, "read health notification events")
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (api *apiServer) handleDeleteHealthNotificationEvents(w http.ResponseWriter, r *http.Request) {
	handleDeleteHealthNotificationEvents(w, r, func(ctx context.Context) error {
		_, err := api.db.Exec(ctx, `
			DELETE FROM health_notification_events
		`)
		return err
	})
}

func handleDeleteHealthNotificationEvents(w http.ResponseWriter, r *http.Request, deleteEvents func(context.Context) error) {
	if err := deleteEvents(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, "delete health notification events")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
