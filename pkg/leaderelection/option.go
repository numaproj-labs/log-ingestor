package leaderelection

import "time"

type Option func(*k8selector)

// WithLeaseDuration sets lease duration.
func WithLeaseDuration(f time.Duration) Option {
	return func(o *k8selector) {
		o.leaseDuration = f
	}
}

// WithRenewDeadline sets renew deadline.
func WithRenewDeadline(f time.Duration) Option {
	return func(o *k8selector) {
		o.renewDeadline = f
	}
}

// WithRetryPeriod sets retry period.
func WithRetryPeriod(f time.Duration) Option {
	return func(o *k8selector) {
		o.retryPeriod = f
	}
}
