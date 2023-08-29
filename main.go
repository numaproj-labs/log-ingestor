package main

import (
	"context"
	"flag"
	"os"
	"time"

	"go.uber.org/zap"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/numaproj-labs/log-ingestor/pkg/ingestor"
	"github.com/numaproj-labs/log-ingestor/pkg/leaderelection"
	"github.com/numaproj-labs/log-ingestor/pkg/logging"
)

func main() {
	logger := logging.NewLogger()

	var (
		ingestionURL       string
		workers            int
		lookbackDuration   time.Duration
		taskIntervalMillis int
	)

	flag.StringVar(&ingestionURL, "ingestion-url", "", "URL for the ingestion endpoint.")
	flag.IntVar(&workers, "workers", 20, "Number of workers to process logs.")
	flag.DurationVar(&lookbackDuration, "lookback-duration", time.Second*60, "The duration of time to look back for logs.")
	flag.IntVar(&taskIntervalMillis, "task-interval", 30000, "Each element in the work queue will be picked up in an interval of this period of milliseconds.")
	flag.Parse()

	if ingestionURL == "" {
		logger.Fatal("Required flag \"--ingestion-url\" is missing")
	}

	namespace, existing := os.LookupEnv("NAMESPACE")
	if !existing {
		logger.Fatal("Required environment variable \"NAMESPACE\" is missing")
	}

	hostname, existing := os.LookupEnv("POD_NAME")
	if !existing {
		logger.Fatal("Required environment variable \"POD_NAME\" is missing")
	}

	config, err := getClientConfig()
	if err != nil {
		logger.Fatalw("Failed to retrieve kubernetes config", zap.Error(err))
	}

	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		logger.Fatalw("Failed to create kubernetes client", zap.Error(err))
	}

	dynamicClient := dynamic.NewForConfigOrDie(config)

	opts := []ingestor.Option{
		ingestor.WithLookbackDuration(lookbackDuration),
		ingestor.WithTaskIntervalMillis(taskIntervalMillis),
		ingestor.WithWorkers(workers),
	}

	i, err := ingestor.NewIngestor(client, dynamicClient, ingestionURL, opts...)
	if err != nil {
		logger.Fatalw("Failed to create ingestor", zap.Error(err))
	}
	elector := leaderelection.NewK8sLeaderElector(client, namespace, "log-ingestor-lock", hostname)
	ctx := ctrl.SetupSignalHandler()
	elector.RunOrDie(ctx, leaderelection.LeaderCallbacks{
		OnStartedLeading: func(_ context.Context) {
			i.Run(ctx)
		},
		OnStoppedLeading: func() {
			logger.Fatalf("Leader lost: %s", hostname)
		},
	})
}

func getClientConfig() (*rest.Config, error) {
	kubeconfig, _ := os.LookupEnv("KUBECONFIG")
	if kubeconfig != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	return rest.InClusterConfig()
}
