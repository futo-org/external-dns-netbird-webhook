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

func TestResponseRecorderKeepsFinalStatus(t *testing.T) {
	tests := []struct {
		name    string
		handler func(w http.ResponseWriter)
		want    int
	}{
		{"write without a status", func(w http.ResponseWriter) {
			_, _ = w.Write([]byte("ok"))
		}, http.StatusOK},
		{"status after write", func(w http.ResponseWriter) {
			_, _ = w.Write([]byte("ok"))
			w.WriteHeader(http.StatusInternalServerError)
		}, http.StatusOK},
		{"second status", func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusNotFound)
			w.WriteHeader(http.StatusInternalServerError)
		}, http.StatusNotFound},
		{"informational before the final status", func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusEarlyHints)
			w.WriteHeader(http.StatusAccepted)
		}, http.StatusAccepted},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorded := make(chan int, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				rec := NewResponseRecorder(w)
				tt.handler(rec)
				recorded <- rec.Status()
			}))
			t.Cleanup(srv.Close)

			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if status := <-recorded; resp.StatusCode != tt.want || status != tt.want {
				t.Errorf("client got %d, recorder has %d, want %d", resp.StatusCode, status, tt.want)
			}
		})
	}
}
