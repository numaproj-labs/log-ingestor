package ingestor

import (
	"time"

	"go.uber.org/zap"
)

type Option func(*ingestor)

// WithTaskInterval sets a time in milliseconds, each element in the work queue will be picked up in an interval of this period of time.
func WithTaskInterval(f int) Option {
	return func(o *ingestor) {
		o.taskInterval = f
	}
}

// WithWorkers sets the number of workers to be used.
func WithWorkers(n int) Option {
	return func(o *ingestor) {
		o.workers = n
	}
}

// WithLookbackDuration sets the lookback duration to be used.
func WithLookbackDuration(f time.Duration) Option {
	return func(o *ingestor) {
		o.lookbackDuration = f
	}
}

// WithLogger sets the logger to be used.
func WithLogger(l *zap.SugaredLogger) Option {
	return func(o *ingestor) {
		o.logger = l
	}
}
