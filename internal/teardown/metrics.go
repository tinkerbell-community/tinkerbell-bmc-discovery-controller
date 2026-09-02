package teardown

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	machinesCompleted = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "teardown_machines_total",
		Help: "Machines whose Talos teardown completed and released the pre-terminate hook.",
	})
	etcdOutcomes = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "teardown_etcd_total",
		Help: "etcd phase conclusions by outcome.",
	}, []string{"outcome"})
	resetOutcomes = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "teardown_reset_total",
		Help: "Reset phase conclusions by outcome (fail-open degradations included).",
	}, []string{"outcome"})
	resetDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "teardown_reset_duration_seconds",
		Help:    "Duration of acknowledged Talos reset calls.",
		Buckets: prometheus.DefBuckets,
	})
)

func init() {
	metrics.Registry.MustRegister(machinesCompleted, etcdOutcomes, resetOutcomes, resetDuration)
}
