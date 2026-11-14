INSERT INTO sensor_types (code, display_name, category, vendor, model, notes)
VALUES ('shelly_blu_motion_zb', 'Shelly BLU Motion ZB', 'motion', 'Shelly', 'SBMO-103Z', 'BTHome v2 motion and battery')
ON CONFLICT (code) DO NOTHING;

CREATE TABLE sensor_motion_state (
    mac text PRIMARY KEY REFERENCES devices(mac),
    motion boolean,
    motion_observed_at timestamptz,
    last_seen_at timestamptz NOT NULL,
    battery_percent double precision CHECK (battery_percent BETWEEN 0 AND 100),
    battery_observed_at timestamptz,
    packet_id smallint CHECK (packet_id BETWEEN 0 AND 255),
    ap_mac text,
    received_at timestamptz NOT NULL
);

CREATE TABLE sensor_motion_events (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    mac text NOT NULL REFERENCES devices(mac),
    observed_at timestamptz NOT NULL,
    received_at timestamptz NOT NULL,
    motion boolean NOT NULL,
    initial boolean NOT NULL,
    packet_id smallint CHECK (packet_id BETWEEN 0 AND 255),
    ap_mac text
);
CREATE INDEX sensor_motion_events_mac_time_idx ON sensor_motion_events (mac, observed_at DESC, id DESC);

-- Serialize per device, so overlapping AP reports and collector restarts cannot
-- create duplicate transitions. Late observations never roll the state back.
CREATE FUNCTION record_sensor_motion(
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
    RETURN changed;
END;
$$;
