-- 0009_instance_settings.up.sql — instance-level settings (HAL-2663).
--
-- One row per setting, keyed by name. The first (and today only) key is
-- 'models': the model providers an instance administrator entered in the
-- dashboard — generative provider, API key, model names, and the Judge's
-- key. The value is sealed with AES-256-GCM under the instance encryption
-- key (VLE_ENCRYPTION_KEY) before it reaches this table, so a database
-- dump never carries a provider key in the clear. updated_at is what
-- every engine replica polls to hot-reload a change without a restart.
CREATE TABLE IF NOT EXISTS instance_settings (
    key        TEXT PRIMARY KEY,
    value      BYTEA NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_by TEXT NOT NULL DEFAULT ''
);
