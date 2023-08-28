package leaderelection

import "context"

type Elector interface {
	RunOrDie(context.Context, LeaderCallbacks)
}

type LeaderCallbacks struct {
	OnStartedLeading func(context.Context)
	OnStoppedLeading func()
}
