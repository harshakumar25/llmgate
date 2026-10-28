package server

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// sse is the transport shared by every streaming dialect: lazy headers, one
// "data:" line per event, a flush after each so tokens leave immediately.
type sse struct {
	w       http.ResponseWriter
	rc      *http.ResponseController
	started bool
}

func newSSE(w http.ResponseWriter) *sse {
	return &sse{w: w, rc: http.NewResponseController(w)}
}

func (s *sse) start() {
	if s.started {
		return
	}
	s.started = true
	h := s.w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no") // keep nginx-style proxies from buffering
	s.w.WriteHeader(http.StatusOK)
}

func (s *sse) event(v any) error {
	s.start()
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(s.w, "data: %s\n\n", b); err != nil {
		return err
	}
	return s.rc.Flush()
}

// finish writes the OpenAI-style end-of-stream sentinel.
func (s *sse) finish() {
	s.start()
	fmt.Fprint(s.w, "data: [DONE]\n\n")
	_ = s.rc.Flush()
}
