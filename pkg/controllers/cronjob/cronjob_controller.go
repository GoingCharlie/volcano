package cronjob

import (
	"context"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/kubernetes"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/cri-api/pkg/errors"
	"k8s.io/klog/v2"
	batchv1alpha1 "volcano.sh/apis/pkg/apis/batch/v1alpha1"
	vcclientset "volcano.sh/apis/pkg/client/clientset/versioned"
	vcscheme "volcano.sh/apis/pkg/client/clientset/versioned/scheme"
	vcinformer "volcano.sh/apis/pkg/client/informers/externalversions"
	batchinformer "volcano.sh/apis/pkg/client/informers/externalversions/batch/v1alpha1"
	batchlister "volcano.sh/apis/pkg/client/listers/batch/v1alpha1"
	"volcano.sh/volcano/pkg/controllers/framework"
	"volcano.sh/volcano/pkg/features"
)

var (
	// controllerKind contains the schema.GroupVersionKind for this controller type.
	controllerKind = batchv1alpha1.SchemeGroupVersion.WithKind("CronJob")

	nextScheduleDelta = 100 * time.Millisecond
)

func init() {
	framework.RegisterController(&cronjobcontroller{})
}

type cronjobcontroller struct {
	kubeClient kubernetes.Interface
	vcClient   vcclientset.Interface

	jobInformer     batchinformer.JobInformer
	cronJobInformer batchinformer.CronJobInformer

	vcInformerFactory vcinformer.SharedInformerFactory

	cronJobList batchlister.CronJobLister
	cronJobSync func() bool
	// A store of jobs
	jobLister batchlister.JobLister
	jobSynced func() bool

	queue         workqueue.TypedRateLimitingInterface[string]
	recorder      record.EventRecorder
	workers       uint32
	maxRequeueNum int
	now           func() time.Time
}

func (cc *cronjobcontroller) Name() string { return "cronjob-controller" }

func (cc *cronjobcontroller) Initialize(opt *framework.ControllerOption) error {
	cc.kubeClient = opt.KubeClient
	cc.vcClient = opt.VolcanoClient

	workers := opt.WorkerNum
	// Initialize event client
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(klog.Infof)
	eventBroadcaster.StartRecordingToSink(&corev1.EventSinkImpl{Interface: cc.kubeClient.CoreV1().Events("")})
	recorder := eventBroadcaster.NewRecorder(vcscheme.Scheme, v1.EventSource{Component: "vc-controller-manager"})
	cc.recorder = recorder
	cc.workers = workers
	cc.maxRequeueNum = opt.MaxRequeueNum
	if cc.maxRequeueNum < 0 {
		cc.maxRequeueNum = -1
	}
	cc.queue = workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]())

	factory := opt.VCSharedInformerFactory
	cc.vcInformerFactory = factory
	if utilfeature.DefaultFeatureGate.Enabled(features.VolcanoCronJobSupport) {
		cc.jobInformer = factory.Batch().V1alpha1().Jobs()
		cc.jobInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc:    cc.addJob,
			UpdateFunc: cc.updateJob,
			DeleteFunc: cc.deleteJob,
		})
		cc.jobLister = cc.jobInformer.Lister()
		cc.jobSynced = cc.jobInformer.Informer().HasSynced

		cc.cronJobInformer = factory.Batch().V1alpha1().CronJobs()
		cc.cronJobList = cc.cronJobInformer.Lister()
		cc.cronJobSync = cc.cronJobInformer.Informer().HasSynced
		cc.cronJobInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				cc.enqueueController(obj)
			},
			UpdateFunc: func(oldObj interface{}, newObj interface{}) {
				cc.updateCronJob(oldObj, newObj)
			},
			DeleteFunc: func(obj interface{}) {
				cc.enqueueController(obj)
			},
		})
	}
	cc.now = time.Now
	return nil
}
func (cc *cronjobcontroller) Run(stopCh <-chan struct{}) {
	cc.vcInformerFactory.Start(stopCh)
	for informerType, ok := range cc.vcInformerFactory.WaitForCacheSync(stopCh) {
		if !ok {
			klog.Errorf("caches failed to sync: %v", informerType)
			return
		}
	}
	for i := 0; i < int(cc.workers); i++ {
		go wait.Until(cc.worker, 0, stopCh)
	}
	klog.Infof("CronJobController is running ...... ")

}
func (cc *cronjobcontroller) worker() {
	for cc.processNextReq() {
	}
}
func (cc *cronjobcontroller) processNextReq() bool {
	key, shutdown := cc.queue.Get()
	if shutdown {
		klog.Errorf("Fail to pop item from queue")
		return false
	}
	defer cc.queue.Done(key)
	requeueAfter, err := cc.sync(key)
	switch {
	case err != nil:
		cc.handleJobError(cc.queue, key, err)
		return true
	case requeueAfter != nil:
		cc.queue.AddAfter(key, *requeueAfter)
	}
	cc.queue.Forget(key)
	return true
}
func (cc *cronjobcontroller) sync(cronJobKey string) (*time.Duration, error) {
	ns, name, err := cache.SplitMetaNamespaceKey(cronJobKey)
	if err != nil {
		return nil, err
	}
	cronJob, err := cc.cronJobList.CronJobs(ns).Get(name)
	switch {
	case errors.IsNotFound(err):
		// may be cronjob is deleted, don't need to requeue this key
		klog.V(4).Info("CronJob not found, maybe it was deleted",
			"cronjob", klog.KObj(cronJob),
			"err", err)
		return nil, nil
	case err != nil:
		// for other transient apiserver error requeue with exponential backoff
		return nil, err
	}

	jobsToBeReconciled, err := cc.getJobsToBeReconciled(cronJob)
	if err != nil {
		return nil, err
	}

	// cronJobCopy is used to combine all the updates to a
	// CronJob object and perform an actual update only once.
	cronJobCopy := cronJob.DeepCopy()

	updateStatusAfterCleanup := cc.cleanupFinishedJobs(cronJobCopy, jobsToBeReconciled)

	requeueAfter, updateStatusAfterSync, syncErr := cc.syncCronJob(cronJobCopy, jobsToBeReconciled)
	if syncErr != nil {
		klog.V(2).Info("Error reconciling cronjob", "cronjob", klog.KObj(cronJob), "err", syncErr)
	}

	// Update the CronJob if needed
	if updateStatusAfterCleanup || updateStatusAfterSync {
		if _, err := cc.vcClient.BatchV1alpha1().CronJobs(ns).UpdateStatus(context.TODO(), cronJobCopy, metav1.UpdateOptions{}); err != nil {
			klog.V(2).Info("Unable to update status for cronjob", "cronjob", klog.KObj(cronJob), "resourceVersion", cronJob.ResourceVersion, "err", err)
			return nil, err
		}
	}

	if requeueAfter != nil {
		klog.V(4).Info("Re-queuing cronjob", "cronjob", klog.KObj(cronJob), "requeueAfter", requeueAfter)
		return requeueAfter, nil
	}
	// this marks the key done, currently only happens when the cronjob is suspended or spec has invalid schedule format
	return nil, syncErr
}
func (cc *cronjobcontroller) handleJobError(queue workqueue.TypedRateLimitingInterface[string], key string, err error) {
	if cc.maxRequeueNum == -1 || queue.NumRequeues(key) < cc.maxRequeueNum {
		klog.V(2).Infof("Failed to handle CronJob <%s>: %v",
			key, err)
		queue.AddRateLimited(key)
		return
	}

}
