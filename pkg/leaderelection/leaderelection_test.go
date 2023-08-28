package leaderelection

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

func Test_RunOrDie(t *testing.T) {
	k8sCli := k8sfake.NewSimpleClientset()
	elector1 := NewK8sLeaderElector(k8sCli, "test-ns", "my-lock", "e1", WithLeaseDuration(10*time.Second), WithRenewDeadline(5*time.Second))
	elector2 := NewK8sLeaderElector(k8sCli, "test-ns", "my-lock", "e2", WithLeaseDuration(10*time.Second), WithRenewDeadline(5*time.Second))

	ctx1, cancel1 := context.WithCancel(context.Background())
	ctx2, cancel2 := context.WithCancel(context.Background())

	defer cancel1()
	defer cancel2()
	flags := map[string]bool{"run1": false, "run2": false, "stop1": false, "stop2": false, "end1": false, "end2": false}
	var lock sync.RWMutex
	setFlag := func(flag string, f bool) {
		lock.Lock()
		defer lock.Unlock()
		flags[flag] = f
	}

	readFlag := func(flag string) bool {
		lock.RLock()
		defer lock.RUnlock()
		return flags[flag]
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		elector1.RunOrDie(ctx1, LeaderCallbacks{
			OnStartedLeading: func(_ context.Context) {
				fmt.Println("start 101")
				setFlag("run1", true)
			},
			OnStoppedLeading: func() {
				fmt.Println("stop 101")
				setFlag("stop1", true)
			},
		})
		setFlag("end1", true)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		elector2.RunOrDie(ctx2, LeaderCallbacks{
			OnStartedLeading: func(_ context.Context) {
				fmt.Println("start 201")
				setFlag("run2", true)
			},
			OnStoppedLeading: func() {
				fmt.Println("stop 201")
				setFlag("stop2", true)
			},
		})
		setFlag("end2", true)
	}()

	time.Sleep(3 * time.Second)
	assert.True(t, readFlag("run1") || readFlag("run2"))
	assert.False(t, readFlag("run1") && readFlag("run2"))
	if readFlag("run1") {
		cancel1()
	}
	if readFlag("run2") {
		cancel2()
	}
	time.Sleep(3 * time.Second)
	assert.True(t, readFlag("stop1") || readFlag("stop2"))
	assert.False(t, readFlag("stop1") && readFlag("stop2"))
	assert.True(t, readFlag("end1") || readFlag("end2"))
	assert.False(t, readFlag("end1") && readFlag("end2"))
	cancel1()
	cancel2()
	wg.Wait()
	assert.True(t, readFlag("end1") && readFlag("end2"))
}
