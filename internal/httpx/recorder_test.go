package httpx

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestResponseRecorderSupportsResponseController(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		rec := NewResponseRecorder(w)
		if err := http.NewResponseController(rec).SetWriteDeadline(time.Now().Add(time.Minute)); err != nil {
			t.Errorf("SetWriteDeadline through the recorder: %v", err)
		}
		rec.WriteHeader(http.StatusAccepted)
		if _, err := rec.Write([]byte("ok")); err != nil {
			t.Errorf("Write: %v", err)
		}
		if rec.Status() != http.StatusAccepted || rec.Written() != 2 {
			t.Errorf("recorded %d/%d bytes, want 202/2", rec.Status(), rec.Written())
		}
	}))
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
}
