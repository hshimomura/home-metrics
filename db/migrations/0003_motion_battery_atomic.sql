-- Atomic persistence of the two supported Shelly measurements.
CREATE OR REPLACE FUNCTION record_sensor_motion(
    p_mac text, p_observed timestamptz, p_received timestamptz,
    p_motion boolean, p_battery double precision, p_packet integer, p_ap text
) RETURNS boolean LANGUAGE plpgsql AS $$
DECLARE
    previous sensor_motion_state%ROWTYPE;
    changed boolean := false;
BEGIN
    INSERT INTO sensor_motion_state (mac, last_seen_at, received_at)
    VALUES (p_mac, p_observed, p_received) ON CONFLICT (mac) DO NOTHING;
    SELECT * INTO previous FROM sensor_motion_state WHERE mac = p_mac FOR UPDATE;
    IF p_motion IS NOT NULL AND
       (previous.motion_observed_at IS NULL OR p_observed > previous.motion_observed_at) THEN
        changed := previous.motion IS DISTINCT FROM p_motion;
        IF changed THEN
            INSERT INTO sensor_motion_events (mac, observed_at, received_at, motion, initial, packet_id, ap_mac)
            VALUES (p_mac, p_observed, p_received, p_motion, previous.motion IS NULL, p_packet, p_ap);
        END IF;
        UPDATE sensor_motion_state SET motion = p_motion, motion_observed_at = p_observed
        WHERE mac = p_mac;
    END IF;
    IF p_battery IS NOT NULL AND
       (previous.battery_observed_at IS NULL OR p_observed > previous.battery_observed_at) THEN
        UPDATE sensor_motion_state SET battery_percent = p_battery, battery_observed_at = p_observed
        WHERE mac = p_mac;
    END IF;
    UPDATE sensor_motion_state SET last_seen_at = GREATEST(last_seen_at, p_observed),
        received_at = GREATEST(received_at, p_received),
        packet_id = CASE WHEN p_observed >= previous.last_seen_at THEN p_packet ELSE packet_id END,
        ap_mac = CASE WHEN p_observed >= previous.last_seen_at THEN p_ap ELSE ap_mac END
    WHERE mac = p_mac;
    -- Store battery history in the same transaction as the transition. Replays
    -- after a lost response are safe, including a crash before queue removal.
    IF p_battery IS NOT NULL THEN
        INSERT INTO sensor_minute (ts, mac, battery_percent)
        VALUES (date_trunc('minute', p_observed), p_mac, p_battery)
        ON CONFLICT (ts, mac) DO UPDATE SET battery_percent = EXCLUDED.battery_percent
        WHERE previous.battery_observed_at IS NULL OR p_observed >= previous.battery_observed_at;
    END IF;
    RETURN changed;
END;
$$;
