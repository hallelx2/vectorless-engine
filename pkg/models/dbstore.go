package models

import (
	"context"
	"errors"
	"time"

	"github.com/hallelx2/vectorless-engine/pkg/db"
)

// DBStore keeps the sealed settings in the engine database's
// instance_settings table.
type DBStore struct{ Pool *db.Pool }

func (s DBStore) Get(ctx context.Context) (StoredValue, bool, error) {
	row, err := s.Pool.GetInstanceSetting(ctx, SettingKey)
	if errors.Is(err, db.ErrNotFound) {
		return StoredValue{}, false, nil
	}
	if err != nil {
		return StoredValue{}, false, err
	}
	return StoredValue{Value: row.Value, UpdatedAt: row.UpdatedAt, UpdatedBy: row.UpdatedBy}, true, nil
}

func (s DBStore) Put(ctx context.Context, value []byte, updatedBy string) (time.Time, error) {
	return s.Pool.PutInstanceSetting(ctx, SettingKey, value, updatedBy)
}

func (s DBStore) Delete(ctx context.Context) error {
	return s.Pool.DeleteInstanceSetting(ctx, SettingKey)
}

func (s DBStore) UpdatedAt(ctx context.Context) (time.Time, error) {
	return s.Pool.InstanceSettingUpdatedAt(ctx, SettingKey)
}
