package leaderelection

import (
	"context"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

// A k8s based leader election implementation
type k8selector struct {
	k8sclient  kubernetes.Interface
	namespace  string
	leaseName  string
	identifier string

	leaseDuration time.Duration
	renewDeadline time.Duration
	retryPeriod   time.Duration
}

func NewK8sLeaderElector(k8sclient kubernetes.Interface, namespace string, leaseName string, identifier string, opts ...Option) Elector {
	e := &k8selector{
		k8sclient:     k8sclient,
		namespace:     namespace,
		leaseName:     leaseName,
		identifier:    identifier,
		leaseDuration: 15 * time.Second,
		renewDeadline: 10 * time.Second,
		retryPeriod:   2 * time.Second,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(e)
		}
	}
	return e
}

func (ke *k8selector) RunOrDie(ctx context.Context, callbacks LeaderCallbacks) {
	lock := &resourcelock.LeaseLock{
		LeaseMeta: metav1.ObjectMeta{
			Name:      ke.leaseName,
			Namespace: ke.namespace,
		},
		Client: ke.k8sclient.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{
			Identity: ke.identifier,
		},
	}

	leaderelection.RunOrDie(ctx, leaderelection.LeaderElectionConfig{
		Lock:            lock,
		ReleaseOnCancel: true,
		LeaseDuration:   ke.leaseDuration,
		RenewDeadline:   ke.renewDeadline,
		RetryPeriod:     ke.retryPeriod,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: callbacks.OnStartedLeading,
			OnStoppedLeading: callbacks.OnStoppedLeading,
		},
	})
}
