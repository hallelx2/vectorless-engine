package db

import (
	"context"
	"errors"
	"time"
)

// InstanceSetting is one row of instance_settings. Value is opaque to
// this package: callers seal it before writing (see pkg/models).
type InstanceSetting struct {
	Key       string
	Value     []byte
	UpdatedAt time.Time
	UpdatedBy string
}

// GetInstanceSetting reads one instance setting. A missing key returns
// ErrNotFound.
func (p *Pool) GetInstanceSetting(ctx context.Context, key string) (InstanceSetting, error) {
	s := InstanceSetting{Key: key}
	err := p.QueryRow(ctx,
		`SELECT value, updated_at, updated_by FROM instance_settings WHERE key = $1`, key,
	).Scan(&s.Value, &s.UpdatedAt, &s.UpdatedBy)
	if err != nil {
		return InstanceSetting{}, mapErr(err)
	}
	return s, nil
}

// PutInstanceSetting inserts or replaces one instance setting and stamps
// it with the current time, which is what other replicas poll for.
func (p *Pool) PutInstanceSetting(ctx context.Context, key string, value []byte, updatedBy string) (time.Time, error) {
	if key == "" {
		return time.Time{}, errors.New("db: instance setting key is required")
	}
	var at time.Time
	err := p.QueryRow(ctx, `
		INSERT INTO instance_settings (key, value, updated_at, updated_by)
		VALUES ($1, $2, clock_timestamp(), $3)
		ON CONFLICT (key) DO UPDATE
		   SET value = EXCLUDED.value, updated_at = EXCLUDED.updated_at, updated_by = EXCLUDED.updated_by
		RETURNING updated_at`, key, value, updatedBy).Scan(&at)
	return at, mapErr(err)
}

// DeleteInstanceSetting removes one instance setting. Deleting a key that
// does not exist is not an error.
func (p *Pool) DeleteInstanceSetting(ctx context.Context, key string) error {
	_, err := p.Exec(ctx, `DELETE FROM instance_settings WHERE key = $1`, key)
	return mapErr(err)
}

// InstanceSettingUpdatedAt returns when key last changed, or the zero
// time when it does not exist. It is the cheap poll behind hot reload.
func (p *Pool) InstanceSettingUpdatedAt(ctx context.Context, key string) (time.Time, error) {
	var at time.Time
	err := p.QueryRow(ctx, `SELECT updated_at FROM instance_settings WHERE key = $1`, key).Scan(&at)
	if err != nil {
		if errors.Is(mapErr(err), ErrNotFound) {
			return time.Time{}, nil
		}
		return time.Time{}, mapErr(err)
	}
	return at, nil
}
