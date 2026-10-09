package metrics

import (
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// RequestsTotal counts HTTP requests to the storage service.
	RequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "storage_requests_total",
		Help: "Total number of HTTP requests to the storage service",
	}, []string{"method", "path"})

	// ErrorsTotal counts HTTP error responses returned by the storage service.
	ErrorsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "storage_errors_total",
		Help: "Total number of HTTP error responses returned by the storage service",
	}, []string{"method", "path", "status_code"})

	// TaggerLatency records the latency of tagger client calls.
	TaggerLatency = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "storage_tagger_latency_seconds",
		Help:    "Latency of tagger client calls in seconds",
		Buckets: prometheus.DefBuckets,
	}, []string{"method"})
)

// TaggerInFlight is the number of tagger calls in progress.
var TaggerInFlight = promauto.NewGauge(prometheus.GaugeOpts{
	Name: "storage_tagger_inflight",
	Help: "Number of tagger client calls in progress",
})

// RecordTaggerLatency records the duration of a tagger client call.
func RecordTaggerLatency(method string, start time.Time) {
	TaggerLatency.WithLabelValues(method).Observe(time.Since(start).Seconds())
}

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
