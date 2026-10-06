// Package metrics holds keystorage's Prometheus instrumentation.
package metrics

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// RequestsTotal counts HTTP requests to the keystorage service.
	RequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "keystorage_requests_total",
		Help: "Total number of HTTP requests to the keystorage service",
	}, []string{"method", "path"})

	// ErrorsTotal counts HTTP error responses returned by the keystorage service.
	ErrorsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "keystorage_errors_total",
		Help: "Total number of HTTP error responses returned by the keystorage service",
	}, []string{"method", "path", "status_code"})
)

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (sr *statusRecorder) WriteHeader(status int) {
	sr.status = status
	sr.ResponseWriter.WriteHeader(status)
}

// Middleware records request and error metrics, labelled by route pattern.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		path := chi.RouteContext(r.Context()).RoutePattern()
		if path == "" {
			path = "unmatched" // never label by raw path: unknown URLs would grow the series without bound
		}
		RequestsTotal.WithLabelValues(r.Method, path).Inc()
		if rec.status >= 400 {
			ErrorsTotal.WithLabelValues(r.Method, path, strconv.Itoa(rec.status)).Inc()
		}
	})
}
