package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/hallelx2/llmgate/judge/typesafe"
)

// Slow local preparation is what hid HAL-1708 for a week; it must be
// loud, and a healthy request must not be.
func TestJudgeRequestLoggerWarnsOnlyOnSlowPreparation(t *testing.T) {
	var buf bytes.Buffer
	log := judgeRequestLogger(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))

	log(typesafe.RequestTrace{Prepare: 3 * time.Millisecond, FirstByte: 900 * time.Millisecond})
	if buf.Len() != 0 {
		t.Fatalf("a healthy request logged at info or above: %s", buf.String())
	}
	log(typesafe.RequestTrace{Prepare: 4 * time.Second, Questions: 120})
	if !strings.Contains(buf.String(), "slow request preparation") || !strings.Contains(buf.String(), "prepare_ms=4000") {
		t.Errorf("slow preparation not reported: %s", buf.String())
	}
}
