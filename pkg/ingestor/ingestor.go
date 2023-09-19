package ingestor

import (
	"bufio"
	"container/list"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/araddon/dateparse"
	"github.com/fsnotify/fsnotify"
	"github.com/google/uuid"
	lru "github.com/hashicorp/golang-lru"
	"github.com/spf13/viper"
	"github.com/tidwall/sjson"
	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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

var (
	logLineRegex = regexp.MustCompile(`(\s|\t)+`)
)

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
	v.AddConfigPath("/etc/config/log-ingestor")
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
			go i.run(ctx, workerID, keyCh)
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

	cloneApps := func() []Application {
		i.lock.RLock()
		defer i.lock.RUnlock()
		apps := make([]Application, len(i.config.Applications))
		copy(apps, i.config.Applications)
		return apps
	}

	runOnce := func() {
		apps := cloneApps()
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
				jsonData, _ := json.Marshal(ro.Object["spec"].(map[string]interface{})["selector"])
				selector = &metav1.LabelSelector{}
				if err := json.Unmarshal(jsonData, selector); err != nil {
					i.logger.Errorw("failed to unmarshal rollout selector", zap.String("namespace", app.Namespace), zap.String("name", app.Name), zap.Error(err))
					continue
				}
			default:
				i.logger.Errorw("unknown application type", zap.String("type", app.Type))
				continue
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
					if len(app.Containers) > 0 && !StringSliceContains(app.Containers, c.Name) {
						// Clean up if it already exists
						if i.Contains(key) {
							i.StopWatching(key)
							_ = i.podInfoCache.Remove(key)
							_ = i.podInfoCache.Remove(key + "/add-time")
						}
						continue
					}
					if !i.Contains(key) {
						// Only add new key here, removing invalid keys will be handled by the worker.
						i.StartWatching(key)
						i.podInfoCache.Add(key+"/add-time", time.Now())
						i.logger.Infow("Start watching", zap.String("namespace", pod.Namespace), zap.String("podName", pod.Name), zap.String("containerName", c.Name))
					}
				}
			}
		}
	}

	runOnce()

	ticker := time.NewTicker(time.Second * 60)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			i.logger.Info("Shutting down the pod tracker")
			return
		case <-ticker.C:
			runOnce()
		}
	}
}

// run is the main function of the worker.
func (i *ingestor) run(ctx context.Context, workerID int, keyCh chan string) {
	i.logger.Infof("Started worker %v", workerID)
	for {
		select {
		case <-ctx.Done():
			i.logger.Infof("Stopped worker %v", workerID)
			return
		case key := <-keyCh:
			if err := i.runOnce(ctx, key, workerID); err != nil {
				i.logger.Errorw("Failed to work on a pod", zap.String("podKey", key), zap.Error(err))
			}
		}
	}
}

// runOnce defines the work of processing a key for a worker
func (i *ingestor) runOnce(ctx context.Context, key string, workerID int) error {
	log := i.logger.With("worker", fmt.Sprint(workerID)).With("key", key)
	log.Debugf("Working on key: %s", key)

	var startTime time.Time
	lastTimeObj, ok := i.podInfoCache.Get(key)
	if ok {
		startTime = lastTimeObj.(time.Time)
	} else {
		addTimeObj, ok := i.podInfoCache.Get(key + "/add-time")
		if ok {
			startTime = addTimeObj.(time.Time).Truncate(time.Minute)
		} else {
			return fmt.Errorf("failed to get start time for key %q", key)
		}
	}

	endTime := startTime.Add(i.lookbackDuration)
	if endTime.After(time.Now()) {
		log.Debug("Skip because the lookback duration is not reached yet")
		return nil
	}

	strs := strings.Split(key, "/")
	if len(strs) != 5 {
		return fmt.Errorf("invalid key %q", key)
	}
	namespace, appName, appType, podName := strs[0], strs[1], strs[2], strs[3]
	// events
	events, err := i.readK8sEvents(ctx, i.k8sclient, key, startTime, endTime)
	if err != nil {
		return fmt.Errorf("failed to read K8s events. %w", err)
	}
	// logs
	logs, err := i.readPodLogs(ctx, i.k8sclient, key, startTime, endTime)
	if err != nil {
		return fmt.Errorf("failed to read pod logs. %w", err)
	}
	if err := i.sendData(ctx, namespace, appName, appType, podName, logs, events, startTime, endTime); err != nil {
		return fmt.Errorf("failed to send logs. %w", err)
	}
	i.podInfoCache.Add(key, endTime)
	return nil
}

// parseLogLine parses a log line and returns the timestamp and the log message.
func (i *ingestor) parseLogLine(log string) (bool, time.Time, string) {
	strs := logLineRegex.Split(log, 2)
	if len(strs) != 2 {
		return false, time.Time{}, ""
	}
	t, err := dateparse.ParseStrict(strs[0])
	if err != nil {
		i.logger.Errorw("Failed to parse timestamp", zap.String("timestamp", strs[0]), zap.Error(err))
		return false, time.Time{}, ""
	}
	return true, t, strs[1]
}

func (i *ingestor) readK8sEvents(ctx context.Context, k8sclient kubernetes.Interface, key string, sinceTime, endTime time.Time) ([]string, error) {
	i.logger.Debugw("Reading K8s events", zap.String("key", key), zap.Time("sinceTime", sinceTime), zap.Time("endTime", endTime))
	strs := strings.Split(key, "/")
	namespace, podName := strs[0], strs[3]
	eventsList, err := k8sclient.CoreV1().Events(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	var result []string
	for _, e := range eventsList.Items {
		if e.Type != "Warning" {
			continue
		}
		if e.LastTimestamp.Time.Before(sinceTime) || e.LastTimestamp.Time.After(endTime) {
			continue
		}
		if e.InvolvedObject.Kind != "Pod" || e.InvolvedObject.Name != podName {
			continue
		}
		msg := fmt.Sprintf(`time=%s type=%s reason="%s" kind=%s object=%s msg="%s"`, e.LastTimestamp.Time.Format(time.RFC3339), e.Type, e.Reason, e.InvolvedObject.Kind, e.InvolvedObject.Name, e.Message)
		result = append(result, msg)
	}
	return result, nil
}

// readPodLogs reads logs from a pod/container.
func (i *ingestor) readPodLogs(ctx context.Context, k8sclient kubernetes.Interface, key string, sinceTime, endTime time.Time) ([]string, error) {
	i.logger.Debugw("Reading logs", zap.String("key", key), zap.Time("sinceTime", sinceTime), zap.Time("endTime", endTime))
	strs := strings.Split(key, "/")
	namespace, podName, containerName := strs[0], strs[3], strs[4]

	stream, err := k8sclient.CoreV1().Pods(namespace).GetLogs(podName, &corev1.PodLogOptions{
		Follow:     false,
		Container:  containerName,
		Timestamps: true,
		SinceTime:  &metav1.Time{Time: sinceTime},
	}).Stream(ctx)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// Stop watching
			i.StopWatching(key)
			_ = i.podInfoCache.Remove(key)
			_ = i.podInfoCache.Remove(key + "/add-time")
			i.logger.Infow("Stop watching", zap.String("namespace", namespace), zap.String("podName", podName), zap.String("containerName", containerName))
		}
		return nil, err
	}
	defer func() { _ = stream.Close() }()

	var result []string
	s := bufio.NewScanner(stream)
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
			if !s.Scan() {
				return result, nil
			}
			// No way to set end time for log query, check timestamp here.
			ok, ts, log := i.parseLogLine(s.Text())
			if !ok {
				continue
			}
			if ts.Before(sinceTime) {
				continue
			}
			if !ts.Before(endTime) {
				return result, nil
			} else {
				result = append(result, log)
			}
		}
	}
}

func (i *ingestor) sendData(ctx context.Context, namespace, appName, appType, podName string, logs []string, events []string, startTime, endTime time.Time) error {
	s := "{}"
	s, _ = sjson.Set(s, "ts", endTime.UnixMilli())
	s, _ = sjson.Set(s, "tid", uuid.New().String())
	s, _ = sjson.Set(s, "start_time", startTime.UnixMilli())
	s, _ = sjson.Set(s, "end_time", endTime.UnixMilli())
	s, _ = sjson.Set(s, "namespace", namespace)
	s, _ = sjson.Set(s, "app_name", appName)
	s, _ = sjson.Set(s, "app_type", appType)
	s, _ = sjson.Set(s, "summarization_type", "pod")
	s, _ = sjson.Set(s, "summarization_name", podName)
	s, _ = sjson.Set(s, "logs", logs)
	s, _ = sjson.Set(s, "events", events)
	// TODO:
	if true {
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

func StringSliceContains(list []string, str string) bool {
	if len(list) == 0 {
		return false
	}
	for _, s := range list {
		if s == str {
			return true
		}
	}
	return false
}
