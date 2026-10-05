package server

import (
	"log/slog"
	"net/http"
	"time"
)

// defaultSlowRequestThreshold is how long a request may take before ServeHTTP
// logs it. Every endpoint normally answers in well under 100 ms; slack-check
// gives up after 5 s. A second is far enough above normal to stay quiet and
// far enough below the checker's limit to catch a stall that nearly tripped
// it.
const defaultSlowRequestThreshold = time.Second

// statusRecorder remembers the status a handler wrote so a slow request can
// be logged with it.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (r *statusRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

// logIfSlow writes a Warn record for a request that took at least the
// threshold, splitting the time between the host/auth gate and the handler.
// handlerStart is zero when the gate turned the request away. Warn rather
// than Info so the record reaches the dashboard's feedback panel as well as
// trayapp.log: a stall otherwise leaves no trace on the trayapp's side.
//
// The timing starts when ServeHTTP is entered. A stall before that — the
// connection waiting to be accepted, or the whole process paused — does not
// appear here; a client-side timeout with no matching record points there.
func (s *Server) logIfSlow(r *http.Request, status int, start, handlerStart time.Time) {
	end := time.Now()
	took := end.Sub(start)
	if took < s.slowRequestThreshold {
		return
	}
	gate, handler := took, time.Duration(0)
	if !handlerStart.IsZero() {
		gate, handler = handlerStart.Sub(start), end.Sub(handlerStart)
	}
	slog.Warn("slow request",
		"method", r.Method,
		"path", r.URL.Path,
		"status", status,
		"remote", r.RemoteAddr,
		"took_ms", millis(took),
		"gate_ms", millis(gate),
		"handler_ms", millis(handler))
}

// millis renders a duration as milliseconds to a tenth.
func millis(d time.Duration) float64 {
	return float64(d.Microseconds()/100) / 10
}
