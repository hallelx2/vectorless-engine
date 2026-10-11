package models

import (
	"bytes"
	"context"
	"os"
	"testing"

	"github.com/hallelx2/vectorless-engine/pkg/db"
)

// TestDBStoreRoundTrip runs the registry against a real Postgres: the
// migration creates instance_settings, a save lands sealed, a second
// registry (another replica) reads it back, and clearing deletes it.
// Skipped without VLE_TEST_DATABASE_URL.
func TestDBStoreRoundTrip(t *testing.T) {
	url := os.Getenv("VLE_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("VLE_TEST_DATABASE_URL not set; skipping live-DB settings test")
	}
	ctx := context.Background()
	pool, err := db.Open(ctx, url, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := pool.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	_ = pool.DeleteInstanceSetting(ctx, SettingKey)
	defer pool.DeleteInstanceSetting(ctx, SettingKey)

	c, _ := NewCipher(testKey)
	a := New(Options{Store: DBStore{Pool: pool}, Cipher: c, BuildClient: fakeBuilder(nil), BuildJudge: fakeJudgeBuilder})
	if _, err := a.Save(ctx, Update{Generative: &Generative{Provider: "anthropic", APIKey: "db-secret-key-000000001", IngestModel: "glm-4.5-air"}}, "it@example.com"); err != nil {
		t.Fatal(err)
	}
	row, err := pool.GetInstanceSetting(ctx, SettingKey)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(row.Value, []byte("db-secret-key")) {
		t.Fatal("key stored in the clear")
	}
	if row.UpdatedBy != "it@example.com" {
		t.Fatalf("updated_by = %q", row.UpdatedBy)
	}
	at, err := pool.InstanceSettingUpdatedAt(ctx, SettingKey)
	if err != nil || !at.Equal(row.UpdatedAt) {
		t.Fatalf("updated_at poll = %v %v", at, err)
	}

	b := New(Options{Store: DBStore{Pool: pool}, Cipher: c, BuildClient: fakeBuilder(nil), BuildJudge: fakeJudgeBuilder})
	if err := b.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	if b.IngestModel() != "glm-4.5-air" || b.Status().Generative.Source != SourceUI {
		t.Fatalf("replica status = %+v", b.Status().Generative)
	}

	if _, err := a.Save(ctx, Update{ClearGenerative: true}, "it@example.com"); err != nil {
		t.Fatal(err)
	}
	if at, _ := pool.InstanceSettingUpdatedAt(ctx, SettingKey); !at.IsZero() {
		t.Fatal("clear left the row")
	}
}
