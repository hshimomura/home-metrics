ALTER TABLE devices
    ADD COLUMN IF NOT EXISTS maintenance_mode boolean NOT NULL DEFAULT false,
    ADD COLUMN IF NOT EXISTS maintenance_reason text,
    ADD COLUMN IF NOT EXISTS maintenance_since timestamptz;
