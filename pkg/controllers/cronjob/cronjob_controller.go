package cronjob

import (
	"context"
	"fmt"
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
	batchv1 "volcano.sh/apis/pkg/apis/batch/v1alpha1"
	vcclientset "volcano.sh/apis/pkg/client/clientset/versioned"
	vcscheme "volcano.sh/apis/pkg/client/clientset/versioned/scheme"
	vcinformer "volcano.sh/apis/pkg/client/informers/externalversions"
	batchinformer "volcano.sh/apis/pkg/client/informers/externalversions/batch/v1alpha1"
	batchlister "volcano.sh/apis/pkg/client/listers/batch/v1alpha1"
	"volcano.sh/volcano/pkg/controllers/framework"
	"volcano.sh/volcano/pkg/features"
)

var (
	controllerKind = batchv1.SchemeGroupVersion.WithKind("CronJob")

	nextScheduleDelta = 100 * time.Millisecond
)

func init() {
	framework.RegisterController(&cronjobcontroller{})
}

type cronjobcontroller struct {
	kubeClient kubernetes.Interface
	vcClient   vcclientset.Interface

	cronjobClient   cronjobClientInterface
	jobClient       jobClientInterface
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
	// vcscheme.AddToScheme(runtime.NewScheme())
	cc.kubeClient = opt.KubeClient
	cc.vcClient = opt.VolcanoClient
	cc.cronjobClient = &realCronjobClient{}
	cc.jobClient = &realJobClient{}
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
	if utilfeature.DefaultFeatureGate.Enabled(features.VolcanoJobSupport) {
		cc.jobInformer = factory.Batch().V1alpha1().Jobs()
		cc.jobInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc:    cc.addJob,
			UpdateFunc: cc.updateJob,
			DeleteFunc: cc.deleteJob,
		})
		cc.jobLister = cc.jobInformer.Lister()
		cc.jobSynced = cc.jobInformer.Informer().HasSynced
	}
	if utilfeature.DefaultFeatureGate.Enabled(features.VolcanoCronJobSupport) {
		cc.cronJobInformer = factory.Batch().V1alpha1().CronJobs()
		cc.cronJobList = cc.cronJobInformer.Lister()
		cc.cronJobSync = cc.cronJobInformer.Informer().HasSynced
		cc.cronJobInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				cc.addCronjobcontrollerQueue(obj, 0)
			},
			UpdateFunc: func(oldObj interface{}, newObj interface{}) {
				cc.updateCronJob(oldObj, newObj)
			},
			DeleteFunc: func(obj interface{}) {
				cc.addCronjobcontrollerQueue(obj, 0)
			},
		})
	}
	cc.now = time.Now
	return nil
}

// Run start JobController.
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
		klog.V(4).Infof("Requeueing key %s after %v", key, *requeueAfter)
		cc.queue.AddAfter(key, *requeueAfter)
	}
	cc.queue.Forget(key)
	return true
}
func (cc *cronjobcontroller) sync(cronJobKey string) (*time.Duration, error) {
	klog.V(3).Infof("Starting to sync up CronJob <%s>", cronJobKey)
	defer klog.V(3).Infof("Finished CronJob <%s> sync up", cronJobKey)
	ns, name, err := cache.SplitMetaNamespaceKey(cronJobKey)
	if err != nil {
		return nil, err
	}
	cronJob, err := cc.cronJobList.CronJobs(ns).Get(name)
	switch {
	case errors.IsNotFound(err):
		klog.Infof("CronJob <%s> not found, err <%s>.",
			cronJobKey, err)
		return nil, nil
	case err != nil:
		return nil, err
	}
	// If the cronjob is terminating, skip process.
	if cronJob.DeletionTimestamp != nil {
		klog.Infof("CronJob <%s> is terminating, skip process.",
			cronJobKey)
		return nil, nil
	}
	// deep copy cronjob to prevent mutate it
	cronJob = cronJob.DeepCopy()

	// Get all jobs controlled by the cronjob for reconciliation
	jobsByCronJob, err := cc.getJobsByCronJob(cronJob)
	if err != nil {
		return nil, err
	}
	//core process the cronjob
	requeueAfter, updateStatus, syncErr := cc.syncCronJob(cronJob, jobsByCronJob)
	if syncErr != nil {
		klog.Errorf("Error syncing cronjob %s: %v", cronJobKey, syncErr)
		return nil, syncErr
	}

	//update the cronjob status if needed
	if updateStatus {
		if _, err := cc.vcClient.BatchV1alpha1().CronJobs(ns).UpdateStatus(context.TODO(), cronJob, metav1.UpdateOptions{}); err != nil {
			klog.Errorf("Failed to update status for CronJob <%s>, err: %v", cronJobKey, err)
			return nil, syncErr
		}
	}
	return requeueAfter, nil
}
func (cc *cronjobcontroller) syncCronJob(cronJob *batchv1.CronJob, jobsByCronJob []*batchv1.Job) (*time.Duration, bool, error) {
	updateStatus := false
	now := cc.now()

	// process the finished jobs: delete old jobs, update status, etc.
	print("processFinishedJobs\n")
	statusAfterProcessFi := cc.processFinishedJobs(cronJob, jobsByCronJob)

	// process the controller jobs and active jobs
	print("processCtljobAndActiveJob\n")
	statusAfterProcessJobs, err := cc.processCtljobAndActiveJob(cronJob, jobsByCronJob)

	updateStatus = statusAfterProcessFi || statusAfterProcessJobs
	if err != nil {
		klog.V(2).Info("Error reconciling cronjob", "cronjob", klog.KObj(cronJob), "err", err)
		return nil, updateStatus, err
	}
	// If the cronjob is suspended, we do not create new jobs.
	if cronJob.Spec.Suspend != nil && *cronJob.Spec.Suspend {
		klog.V(4).Info("Not starting job because the cron is suspended", "cronjob", klog.KObj(cronJob))
		return nil, updateStatus, nil
	}

	// check TZ and schedule validity, if valid, get format scheduled time
	print("validateTZandSchedule\n")
	sch, validErr := cc.validateTZandSchedule(cronJob, cc.recorder)
	if validErr != nil {
		klog.V(2).Info("Error validating cronjob schedule", "cronjob", klog.KObj(cronJob), "err", validErr)
		return nil, updateStatus, validErr
	}

	// scheduleTime is the next time to run the job. if it is nil, it means there are no unmet start times.
	scheduledTime, err := nextScheduleTime(cronJob, now, sch, cc.recorder)
	print(scheduledTime, "\n")
	if err != nil {
		klog.V(2).Info("Error getting schedule time", "cronjob", klog.KObj(cronJob), "err", err)
		cc.recorder.Eventf(cronJob, v1.EventTypeWarning, "InvalidSchedule", "Error getting schedule time: %v", err)
		return nil, updateStatus, err
	}
	// If there is no unmet start times, we skip job creation.
	if scheduledTime == nil {
		print("scheduleTime is nil\n")
		klog.V(2).Info("No unmet start times, skipping job creation",
			"cronjob", klog.KObj(cronJob))
		t := nextScheduleTimeDuration(cronJob, now, sch)
		return t, updateStatus, nil
	}

	// If the cronjob has a starting deadline, check if the scheduled time is after the deadline.
	// if cronJob.Spec.StartingDeadlineSeconds != nil {
	// 	if now.After(scheduledTime.Add(time.Duration(*cronJob.Spec.StartingDeadlineSeconds) * time.Second)) {
	// 		klog.V(2).Info("Missed job creation because the starting deadline has passed",
	// 			"cronjob", klog.KObj(cronJob), "scheduledTime", scheduledTime)
	// 		cc.recorder.Eventf(cronJob, v1.EventTypeWarning, "StartingDeadlineExceeded", "Missed job creation because the starting deadline has passed: %v", *cronJob.Spec.StartingDeadlineSeconds)
	// 		t := nextScheduleTimeDuration(cronJob, now, sch)
	// 		return t, updateStatus, nil
	// 	}
	// }

	// check if the scheduled time is already processed.
	print("already process\n")
	if inActiveListByName(cronJob, &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      getJobName(cronJob, *scheduledTime),
			Namespace: cronJob.Namespace,
		}}) || cronJob.Status.LastScheduleTime.Equal(&metav1.Time{Time: *scheduledTime}) {
		klog.V(4).Info("Not starting job because the scheduled time is already processed", "cronjob", klog.KObj(cronJob), "schedule", scheduledTime)
		t := nextScheduleTimeDuration(cronJob, now, sch)
		return t, updateStatus, nil
	}

	// If the cronjob has a concurrency policy, process it.
	// isAddqueueAfter indicates whether the cronjob should be requeued after processing.
	print("processConcurrencyPolicy\n")
	isAddqueueAfter, statusAfterProcessCC, err := cc.processConcurrencyPolicy(cronJob)
	print("isAddqueueAfter:", isAddqueueAfter, "\n")
	updateStatus = updateStatus || statusAfterProcessCC
	if err != nil {
		return nil, updateStatus, err
	}
	if isAddqueueAfter {
		t := nextScheduleTimeDuration(cronJob, now, sch)
		return t, updateStatus, nil
	}

	// create a new job
	print("createJob\n")
	job, err := cc.createJob(cronJob, *scheduledTime)
	if err != nil {
		return nil, updateStatus, err
	}
	if job == nil || inActiveList(cronJob, job.UID) {
		t := nextScheduleTimeDuration(cronJob, now, sch)
		return t, updateStatus, nil
	}

	// Add the job to the active list of the cronjob
	// and update the last schedule time.
	print("getRef\n")
	// fmt.Printf("Created Job: %+v\n", job)
	jobRef, err := getRef(job)
	if err != nil {
		klog.V(2).Info("Unable to make object reference", "cronjob", klog.KObj(cronJob), "err", err)
		return nil, updateStatus, fmt.Errorf("unable to make object reference for job for %s", klog.KObj(cronJob))
	}
	cronJob.Status.Active = append(cronJob.Status.Active, *jobRef)
	cronJob.Status.LastScheduleTime = &metav1.Time{Time: *scheduledTime}
	print("cronJob.Status.LastScheduleTime: ", cronJob.Status.LastScheduleTime, "\n")
	updateStatus = true

	t := nextScheduleTimeDuration(cronJob, now, sch)
	return t, updateStatus, nil
}
