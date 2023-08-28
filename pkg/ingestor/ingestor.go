package ingestor

import (
	"bufio"
	"container/list"
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/google/uuid"
	lru "github.com/hashicorp/golang-lru"
	"github.com/spf13/viper"
	"github.com/tidwall/sjson"
	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/numaproj-labs/log-ingestor/pkg/logging"
)

var defaultSettings struct {
	taskInterval     int
	workers          int
	lookbackDuration time.Duration
}

func init() {
	defaultSettings.taskInterval = 30000
	defaultSettings.lookbackDuration = time.Minute
	defaultSettings.workers = 20
}

type ingestor struct {
	k8sclient kubernetes.Interface
	dynclient dynamic.Interface
	// Time in milliseconds, each element in the work queue will be picked up in an interval of this period of time.
	taskInterval int
	// The duration of time to look back for logs.
	lookbackDuration time.Duration
	workers          int
	logger           *zap.SugaredLogger

	// The URL to send logs to.
	ingestionURL string
	httpClient   *http.Client

	config *Config

	podInfoMap map[string]*list.Element
	// List of the pod namespaced name, format is "namespace/appType/appName/podName/containerName".
	podInfoList *list.List
	// Cache to store the last processing time of each pod/container
	// Format of the key is "namespace/appType/appName/podName/containerName"
	podInfoCache *lru.Cache
	lock         *sync.RWMutex
}

// NewIngestor returns an ingestor instance
func NewIngestor(k8sclient kubernetes.Interface, dynclient dynamic.Interface, ingestionURL string, opts ...Option) (*ingestor, error) {
	i := &ingestor{
		k8sclient:        k8sclient,
		dynclient:        dynclient,
		ingestionURL:     ingestionURL,
		taskInterval:     defaultSettings.taskInterval,
		lookbackDuration: defaultSettings.lookbackDuration,
		podInfoMap:       make(map[string]*list.Element),
		podInfoList:      list.New(),
		lock:             new(sync.RWMutex),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(i)
		}
	}
	if i.logger == nil {
		i.logger = logging.NewLogger()
	}
	c, err := i.loadConfig(func(err error) {
		i.logger.Errorw("failed to reload configuration file", zap.Error(err))
	})
	if err != nil {
		return nil, err
	}
	i.config = c
	podInfoCache, _ := lru.New(10000)
	i.podInfoCache = podInfoCache
	i.httpClient = &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
		Timeout: time.Second * 2,
	}
	return i, nil
}

// Contains returns if the ingestor contains the key.
func (i *ingestor) Contains(key string) bool {
	i.lock.RLock()
	defer i.lock.RUnlock()
	_, ok := i.podInfoMap[key]
	return ok
}

// StartWatching put a key (namespace/appType/appName/podName/containerName) into the ingestor
func (i *ingestor) StartWatching(key string) {
	i.lock.Lock()
	defer i.lock.Unlock()
	if _, ok := i.podInfoMap[key]; !ok {
		i.podInfoMap[key] = i.podInfoList.PushBack(key)
	}
}

// StopWatching stops watch the key (namespace/appType/appName/podName/containerName)
func (i *ingestor) StopWatching(key string) {
	i.lock.Lock()
	defer i.lock.Unlock()
	if e, ok := i.podInfoMap[key]; ok {
		_ = i.podInfoList.Remove(e)
		delete(i.podInfoMap, key)
	}
}

// Length returns how many pods are being watched for ingestion.
func (i *ingestor) Length() int {
	i.lock.RLock()
	defer i.lock.RUnlock()
	return i.podInfoList.Len()
}

// loadConfig loads configuration file and watch for changes
func (i *ingestor) loadConfig(onErrorReloading func(error)) (*Config, error) {
	v := viper.New()
	v.SetConfigName("config")
	v.SetConfigType("yaml")
	v.AddConfigPath("/etc/ingestor")
	err := v.ReadInConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to load configuration file. %w", err)
	}
	r := &Config{}
	err = v.Unmarshal(r)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal configuration file. %w", err)
	}
	v.WatchConfig()
	v.OnConfigChange(func(e fsnotify.Event) {
		c := &Config{}
		err = v.Unmarshal(c)
		if err != nil {
			onErrorReloading(err)
		}
		i.lock.Lock()
		defer i.lock.Unlock()
		i.config = c
	})
	return r, nil
}

// Run starts an infinite for loop for ingestion.
// it accepts a cancellable context as a parameter.
func (i *ingestor) Run(ctx context.Context) {
	i.logger.Info("Start ingestion service...")
	var wg sync.WaitGroup
	keyCh := make(chan string)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	startFrom := time.Now().Add(-i.lookbackDuration).Truncate(time.Minute)

	// Pod tracker
	wg.Add(1)
	go func() {
		defer wg.Done()
		i.trackPods(ctx)
	}()

	// Worker group
	for j := 1; j <= i.workers; j++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			go i.run(ctx, startFrom, workerID, keyCh)
		}(j)
	}

	// Function assign() moves an element in the list from the front to the back,
	// and send to the channel so that it can be picked up by a worker.
	assign := func() {
		i.lock.Lock()
		defer i.lock.Unlock()
		if i.podInfoList.Len() == 0 {
			return
		}
		e := i.podInfoList.Front()
		if key, ok := e.Value.(string); ok {
			i.podInfoList.MoveToBack(e)
			keyCh <- key
		}
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		// Following for loop keeps calling assign() function to assign tasks to the workers.
		// It makes sure each element in the list will be assigned every N milliseconds.
		for {
			select {
			case <-ctx.Done():
				i.logger.Info("Shutting down the job assigner")
				return
			default:
				assign()
			}
			// Make sure each of the key will be assigned at most every N milliseconds.
			time.Sleep(time.Millisecond * time.Duration(func() int {
				l := i.Length()
				if l == 0 {
					return i.taskInterval
				}
				result := i.taskInterval / l
				if result > 0 {
					return result
				}
				return 1
			}()))
		}
	}()

	wg.Wait()
}

func (i *ingestor) trackPods(ctx context.Context) {
	ticker := time.NewTicker(time.Second * 60)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			i.logger.Info("Shutting down the pod tracker")
			return
		case <-ticker.C:
			var apps []Application
			{
				i.lock.RLock()
				defer i.lock.RUnlock()
				apps = make([]Application, len(i.config.Applications))
				copy(apps, i.config.Applications)
			}
			for _, app := range apps {
				var selector *metav1.LabelSelector
				switch app.Type {
				case "deployment":
					deploy, err := i.k8sclient.AppsV1().Deployments(app.Namespace).Get(ctx, app.Name, metav1.GetOptions{})
					if err != nil {
						i.logger.Errorw("failed to get deployment", zap.String("namespace", app.Namespace), zap.String("name", app.Name), zap.Error(err))
						continue
					}
					selector = deploy.Spec.Selector
				case "statefulset":
					sts, err := i.k8sclient.AppsV1().StatefulSets(app.Namespace).Get(ctx, app.Name, metav1.GetOptions{})
					if err != nil {
						i.logger.Errorw("failed to get statefulset", zap.String("namespace", app.Namespace), zap.String("name", app.Name), zap.Error(err))
						continue
					}
					selector = sts.Spec.Selector
				case "daemonset":
					ds, err := i.k8sclient.AppsV1().DaemonSets(app.Namespace).Get(ctx, app.Name, metav1.GetOptions{})
					if err != nil {
						i.logger.Errorw("failed to get daemonset", zap.String("namespace", app.Namespace), zap.String("name", app.Name), zap.Error(err))
						continue
					}
					selector = ds.Spec.Selector
				case "rollout":
					ro, err := i.dynclient.Resource(schema.GroupVersionResource{
						Group:    "argoproj.io",
						Version:  "v1alpha1",
						Resource: "rollouts",
					}).Namespace(app.Namespace).Get(ctx, app.Name, metav1.GetOptions{})
					if err != nil {
						i.logger.Errorw("failed to get rollout", zap.String("namespace", app.Namespace), zap.String("name", app.Name), zap.Error(err))
						continue
					}
					selector = ro.Object["spec"].(map[string]interface{})["selector"].(*metav1.LabelSelector)
				default:
					i.logger.Errorw("unknown application type", zap.String("type", app.Type))
				}
				if selector == nil {
					continue
				}
				pods, err := i.k8sclient.CoreV1().Pods(app.Namespace).List(ctx, metav1.ListOptions{
					LabelSelector: metav1.FormatLabelSelector(selector),
				})
				if err != nil {
					i.logger.Errorw("failed to list pods", zap.String("namespace", app.Namespace), zap.String("name", app.Name), zap.Error(err))
					continue
				}
				for _, pod := range pods.Items {
					for _, c := range pod.Spec.Containers {
						key := fmt.Sprintf("%s/%s/%s/%s/%s", pod.Namespace, app.Type, app.Name, pod.Name, c.Name)
						if !i.Contains(key) {
							// Only add new key here, removing invalid keys will be handled by the worker.
							i.StartWatching(key)
						}
					}
					i.logger.Infow("Start watching pod", zap.String("namespace", pod.Namespace), zap.String("name", pod.Name))
				}
			}
		}
	}
}

func (i *ingestor) run(ctx context.Context, defaultStartTime time.Time, workerID int, keyCh chan string) {
	i.logger.Infof("Started autoscaling worker %v", workerID)
	for {
		select {
		case <-ctx.Done():
			i.logger.Infof("Stopped worker %v", workerID)
			return
		case key := <-keyCh:
			if err := i.runOnce(ctx, key, defaultStartTime, workerID); err != nil {
				i.logger.Errorw("Failed to work on a pod", zap.String("podKey", key), zap.Error(err))
			}
		}
	}
}

func (i *ingestor) runOnce(ctx context.Context, key string, defaultStartTime time.Time, workerID int) error {
	log := i.logger.With("worker", fmt.Sprint(workerID)).With("podKey", key)
	log.Debugf("Working on key: %s", key)
	strs := strings.Split(key, "/")
	if len(strs) != 5 {
		return fmt.Errorf("invalid key %q", key)
	}
	namespace, appName, appType, podName := strs[0], strs[1], strs[2], strs[3]
	logs, endTime, err := i.readPodLogs(ctx, i.k8sclient, key, defaultStartTime)
	if err != nil {
		return fmt.Errorf("failed to read pod logs. %w", err)
	}
	if err := i.sendLogs(ctx, namespace, appName, appType, podName, logs, endTime); err != nil {
		return fmt.Errorf("failed to send logs. %w", err)
	}
	i.podInfoCache.Add(key, endTime)
	return nil
}

func (i *ingestor) readPodLogs(ctx context.Context, k8sclient kubernetes.Interface, key string, defaultStartTime time.Time) ([]string, time.Time, error) {
	strs := strings.Split(key, "/")
	namespace, podName, containerName := strs[0], strs[3], strs[4]
	lastTime := defaultStartTime
	lastTimeObj, ok := i.podInfoCache.Get(key)
	if ok {
		lastTime = lastTimeObj.(time.Time)
	}

	stream, err := k8sclient.CoreV1().Pods(namespace).GetLogs(podName, &corev1.PodLogOptions{
		Follow:     false,
		Container:  containerName,
		Timestamps: true,
		SinceTime:  &metav1.Time{Time: lastTime},
	}).Stream(ctx)
	if err != nil {
		return nil, time.Time{}, err
	}
	defer func() { _ = stream.Close() }()

	var result []string
	endTime := lastTime.Add(i.lookbackDuration)
	s := bufio.NewScanner(stream)
	for {
		select {
		case <-ctx.Done():
			return nil, time.Time{}, ctx.Err()
		default:
			if !s.Scan() {
				return result, endTime, nil
			}
			// TODO: check timestamp
			if false {
				return result, endTime, nil
			}
			result = append(result, s.Text())
		}
	}
}

func (i *ingestor) sendLogs(ctx context.Context, namespace, appName, appType, podName string, logs []string, endTime time.Time) error {
	s := "{}"
	s, _ = sjson.Set(s, "ts", endTime.UnixMilli())
	s, _ = sjson.Set(s, "tid", uuid.New().String())
	s, _ = sjson.Set(s, "start_time", endTime.Add(-i.lookbackDuration).UnixMilli())
	s, _ = sjson.Set(s, "end_time", endTime.UnixMilli())
	s, _ = sjson.Set(s, "namespace", namespace)
	s, _ = sjson.Set(s, "app_name", appName)
	s, _ = sjson.Set(s, "app_type", appType)
	s, _ = sjson.Set(s, "summarization_type", "pod")
	s, _ = sjson.Set(s, "summarization_name", podName)
	s, _ = sjson.Set(s, "logs", logs)
	// TODO:
	if false {
		resp, err := i.httpClient.Post(i.ingestionURL, "application/json", strings.NewReader(s))
		if err != nil {
			return err
		}
		_ = resp.Body.Close()
	} else {
		fmt.Println(s)
	}
	return nil
}
