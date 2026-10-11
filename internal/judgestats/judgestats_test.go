package judgestats

import (
	"bytes"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hallelx2/llmgate/judge/typesafe"
)

func TestSummarySeparatesLocalFromProviderTime(t *testing.T) {
	r := &Recorder{}
	var wg sync.WaitGroup
	for i := 1; i <= 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.Observe(typesafe.RequestTrace{
				Prepare:      time.Duration(i) * time.Millisecond,
				FirstByte:    time.Duration(i) * 100 * time.Millisecond,
				Total:        time.Duration(i)*100*time.Millisecond + time.Duration(i)*time.Millisecond,
				GuardCounted: i%2 == 0,
				ConnReused:   i > 1,
			})
		}()
	}
	r.Observe(typesafe.RequestTrace{Err: errors.New("529")})
	wg.Wait()
	var buf bytes.Buffer
	r.Summary(&buf)
	out := buf.String()
	for _, want := range []string{"judge requests 21 (failed 1, tokenised by the guard 10, on a reused connection 19)", "prepare (local)", "first byte (provider)", "max    2000ms"} {
		if !strings.Contains(out, want) {
			t.Errorf("summary missing %q:\n%s", want, out)
		}
	}
}

func TestEmptySummaryPrintsNothing(t *testing.T) {
	var buf bytes.Buffer
	(&Recorder{}).Summary(&buf)
	if buf.Len() != 0 {
		t.Errorf("got %q", buf.String())
	}
}
