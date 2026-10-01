package metrics

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// RequestsTotal counts HTTP requests to the api service.
	RequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "api_requests_total",
		Help: "Total number of HTTP requests to the api service",
	}, []string{"method", "path"})

	// ErrorsTotal counts HTTP error responses returned by the api service.
	ErrorsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "api_errors_total",
		Help: "Total number of HTTP error responses returned by the api service",
	}, []string{"method", "path", "status_code"})
)

// statusRecorder wraps an http.ResponseWriter to capture the status code.
type statusRecorder struct {
	ResponseWriter http.ResponseWriter
	Status         int
}

func (sr *statusRecorder) WriteHeader(status int) {
	sr.Status = status
	sr.ResponseWriter.WriteHeader(status)
}

func (sr *statusRecorder) Write(b []byte) (int, error) {
	return sr.ResponseWriter.Write(b)
}

func (sr *statusRecorder) Header() http.Header {
	return sr.ResponseWriter.Header()
}

// Middleware returns an HTTP middleware that records request and error metrics.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, Status: http.StatusOK}
		next.ServeHTTP(rec, r)

		path := chi.RouteContext(r.Context()).RoutePattern()
		if path == "" {
			path = r.URL.Path
		}

		RequestsTotal.WithLabelValues(r.Method, path).Inc()
		if rec.Status >= 400 {
			ErrorsTotal.WithLabelValues(r.Method, path, strconv.Itoa(rec.Status)).Inc()
		}
	})
}
