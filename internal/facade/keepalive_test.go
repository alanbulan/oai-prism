package facade

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/oai-prism/oaiprism/internal/sse"
)

func TestKeepAlive(t *testing.T) {
	rec := httptest.NewRecorder()
	sw, err := sse.New(rec)
	if err != nil {
		t.Fatal(err)
	}
	stop := keepAlive(sw, 10*time.Millisecond, anthropicPingFrame)
	time.Sleep(55 * time.Millisecond)
	stop()
	n := strings.Count(rec.Body.String(), `"type":"ping"`)
	if n < 2 {
		t.Fatalf("应定时写保活帧，实际 %d 帧: %q", n, rec.Body.String())
	}
	time.Sleep(30 * time.Millisecond)
	if strings.Count(rec.Body.String(), `"type":"ping"`) != n {
		t.Fatal("stop 之后不应再写")
	}
}
