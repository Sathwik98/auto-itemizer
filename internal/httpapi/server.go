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

	expenseserver "auto-itemizer/internal/expense/server"
	expenseservice "auto-itemizer/internal/expense/service"
	"auto-itemizer/internal/httpapi/respond"
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
	return recoverPanics(mux)
}

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
			respond.WriteProblem(w, http.StatusInternalServerError, "INTERNAL_ERROR", "internal error")
		}()
		next.ServeHTTP(w, r)
	})
}
