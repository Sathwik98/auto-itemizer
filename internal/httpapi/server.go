// Package httpapi is the router of the HTTP layer (ARCHITECTURE.md §1). The
// controllers are in receipt/server and expense/server; this package sends
// each route to them, and gives unknown paths, wrong methods and panics the
// JSON error body of §3.0. The request and response bodies are in
// httpapi/models, and httpapi/respond writes them.
package httpapi

import (
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	expenseserver "auto-itemizer/internal/expense/server"
	expenseservice "auto-itemizer/internal/expense/service"
	"auto-itemizer/internal/httpapi/respond"
	"auto-itemizer/internal/metrics"
	receiptserver "auto-itemizer/internal/receipt/server"
	receiptservice "auto-itemizer/internal/receipt/service"
)

// NewHandler returns the API: the routes of ARCHITECTURE.md §3. Every 4xx and
// 5xx response has the JSON error body of §3.0, including unknown paths,
// wrong methods and panics.
func NewHandler(receipts *receiptservice.Service, expenses *expenseservice.Service) http.Handler {
	rc := receiptserver.New(receipts)
	ec := expenseserver.New(expenses)

	routes := []struct {
		method  string
		path    string
		handler http.HandlerFunc
	}{
		{http.MethodPost, "/receipts", rc.Upload},
		{http.MethodPost, "/receipts/{id}/process", rc.Process},
		{http.MethodGet, "/receipts/{id}", rc.Get},
		{http.MethodGet, "/transactions/{id}", ec.Get},
		{http.MethodPost, "/transactions/{id}/itemize", ec.Itemize},
		{http.MethodPatch, "/transactions/{id}/items", ec.PatchItems},
		{http.MethodGet, "/health", health},
		{http.MethodGet, "/metrics", metrics.Handler().ServeHTTP}, // ARCHITECTURE.md §3.8
	}

	mux := http.NewServeMux()
	for _, route := range routes {
		mux.HandleFunc(route.method+" "+route.path, route.handler)
		// The same path with any other method: a JSON 405 instead of the
		// mux's plain-text one.
		mux.HandleFunc(route.path, methodNotAllowed(route.method))
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		respond.WriteProblem(w, http.StatusNotFound, "NOT_FOUND", "no such endpoint")
	})
	// logRequests is outside recoverPanics, so a panic's 500 is logged and
	// counted like any other answer.
	return logRequests(recoverPanics(mux))
}

// statusClientClosed is recorded for a request whose client went away before
// an answer was written. It is nginx's "client closed request"; no response
// with this code is ever sent.
const statusClientClosed = 499

// logRequests writes one log line per request and records it in the metrics
// (ARCHITECTURE.md §3.8). The level follows the status: INFO below 400, WARN
// for 4xx, ERROR for 5xx. GET /metrics is counted but not logged, because
// Prometheus polls it.
func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		duration := time.Since(start)

		// The router sets r.Pattern to the route that matched, such as
		// "POST /receipts/{id}/process". The catch-all "/" is an unknown path.
		route := r.Pattern
		if route == "" || route == "/" {
			route = "unmatched"
		}
		status := rec.status
		switch {
		case status == 0 && r.Context().Err() != nil:
			status = statusClientClosed // the client left, so there was no one to answer
		case status == 0:
			status = http.StatusOK // the handler wrote nothing
		}
		metrics.RecordRequest(route, status, duration)
		if route == "GET /metrics" {
			return
		}

		level := slog.LevelInfo
		switch {
		case status >= 500:
			level = slog.LevelError
		case status >= 400:
			level = slog.LevelWarn
		}
		slog.Log(r.Context(), level, "request", "method", r.Method, "path", r.URL.Path,
			"route", route, "status", status, "duration_ms", duration.Milliseconds())
	})
}

// statusRecorder remembers the status code a handler sends, for the request
// log and metrics.
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
		r.status = http.StatusOK // a body without WriteHeader means 200
	}
	return r.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the real writer.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// health handles GET /health.
func health(w http.ResponseWriter, r *http.Request) {
	respond.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// methodNotAllowed answers a request whose path exists with another method.
func methodNotAllowed(allowed string) http.HandlerFunc {
	if allowed == http.MethodGet {
		allowed += ", " + http.MethodHead // the mux serves HEAD for GET routes
	}
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Allow", allowed)
		respond.WriteProblem(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "use "+allowed+" for this path")
	}
}

// recoverPanics turns a panic in a handler into a logged 500 with the JSON
// error body, instead of a dropped connection.
func recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			p := recover()
			if p == nil {
				return
			}
			if p == http.ErrAbortHandler { // net/http's way to abort a response on purpose
				panic(p)
			}
			slog.Error("panic in handler", "method", r.Method, "path", r.URL.Path,
				"panic", p, "stack", string(debug.Stack()))
			metrics.RecordPanic()
			respond.WriteProblem(w, http.StatusInternalServerError, "INTERNAL_ERROR", "internal error")
		}()
		next.ServeHTTP(w, r)
	})
}
