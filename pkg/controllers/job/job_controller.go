/*
Copyright 2017 The Volcano Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package job

import (
	"fmt"
	"hash/fnv"
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/informers"
	coreinformers "k8s.io/client-go/informers/core/v1"
	kubeschedulinginformers "k8s.io/client-go/informers/scheduling/v1"
	"k8s.io/client-go/kubernetes"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	kubeschedulinglisters "k8s.io/client-go/listers/scheduling/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	batchv1alpha1 "volcano.sh/apis/pkg/apis/batch/v1alpha1"
	busv1alpha1 "volcano.sh/apis/pkg/apis/bus/v1alpha1"
	vcclientset "volcano.sh/apis/pkg/client/clientset/versioned"
	vcscheme "volcano.sh/apis/pkg/client/clientset/versioned/scheme"
	vcinformer "volcano.sh/apis/pkg/client/informers/externalversions"
	batchinformer "volcano.sh/apis/pkg/client/informers/externalversions/batch/v1alpha1"
	businformer "volcano.sh/apis/pkg/client/informers/externalversions/bus/v1alpha1"
	schedulinginformers "volcano.sh/apis/pkg/client/informers/externalversions/scheduling/v1beta1"
	batchlister "volcano.sh/apis/pkg/client/listers/batch/v1alpha1"
	buslister "volcano.sh/apis/pkg/client/listers/bus/v1alpha1"
	schedulinglisters "volcano.sh/apis/pkg/client/listers/scheduling/v1beta1"

	"volcano.sh/volcano/pkg/controllers/apis"
	jobcache "volcano.sh/volcano/pkg/controllers/cache"
	"volcano.sh/volcano/pkg/controllers/framework"
	"volcano.sh/volcano/pkg/controllers/job/state"
	"volcano.sh/volcano/pkg/features"
)

func init() {
	framework.RegisterController(&jobcontroller{})
}

type delayAction struct {
	// request is the event request that registered the delayed action. It is
	// kept on the internal queue item so delayed execution does not need to add
	// controller-only fields to the public Request type.
	request apis.Request

	// job identifies the exact Job incarnation. Namespace/name alone is not
	// sufficient because a Job can be deleted and recreated while an item is
	// waiting in the delaying queue.
	job jobIdentity

	// The name of the task
	taskName string

	// The name of the pod
	podName string

	// The UID of the pod
	podUID types.UID

	partition string

	// The event caused the action
	event busv1alpha1.Event

	// The action to take.
	action busv1alpha1.Action

	// The delay before the action is executed
	delay time.Duration
}

type jobIdentity struct {
	key string
	uid types.UID
}

// jobcontroller the Job jobcontroller type.
type jobcontroller struct {
	kubeClient kubernetes.Interface
	vcClient   vcclientset.Interface

	jobInformer   batchinformer.JobInformer
	podInformer   coreinformers.PodInformer
	pvcInformer   coreinformers.PersistentVolumeClaimInformer
	pgInformer    schedulinginformers.PodGroupInformer
	svcInformer   coreinformers.ServiceInformer
	cmdInformer   businformer.CommandInformer
	pcInformer    kubeschedulinginformers.PriorityClassInformer
	queueInformer schedulinginformers.QueueInformer

	informerFactory   informers.SharedInformerFactory
	vcInformerFactory vcinformer.SharedInformerFactory

	// A store of jobs
	jobLister batchlister.JobLister
	jobSynced func() bool

	// A store of pods
	podLister corelisters.PodLister
	podSynced func() bool

	pvcLister corelisters.PersistentVolumeClaimLister
	pvcSynced func() bool

	// A store of podgroups
	pgLister schedulinglisters.PodGroupLister
	pgSynced func() bool

	// A store of service
	svcLister corelisters.ServiceLister
	svcSynced func() bool

	cmdLister buslister.CommandLister
	cmdSynced func() bool

	pcLister kubeschedulinglisters.PriorityClassLister
	pcSynced func() bool

	queueLister schedulinglisters.QueueLister
	queueSynced func() bool

	// queue that need to sync up
	queueList    []workqueue.TypedRateLimitingInterface[any]
	commandQueue workqueue.TypedRateLimitingInterface[any]
	cache        jobcache.Cache
	// Job Event recorder
	recorder record.EventRecorder

	errTasks      workqueue.TypedRateLimitingInterface[any]
	workers       uint32
	maxRequeueNum int

	delayActionMapLock sync.RWMutex
	// delayActionMap stores the current generation of each delayed action. Old
	// generations can remain in the workqueue after replacement or cancellation,
	// but pointer identity makes them harmless stale items.
	delayActionMap map[jobIdentity]map[string]*delayAction
}

func (cc *jobcontroller) Name() string {
	return "job-controller"
}

// Initialize creates the new Job controller.
func (cc *jobcontroller) Initialize(opt *framework.ControllerOption) error {
	cc.kubeClient = opt.KubeClient
	cc.vcClient = opt.VolcanoClient

	sharedInformers := opt.SharedInformerFactory
	workers := opt.WorkerNum
	// Initialize event client
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(klog.Infof)
	eventBroadcaster.StartRecordingToSink(&corev1.EventSinkImpl{Interface: cc.kubeClient.CoreV1().Events("")})
	recorder := eventBroadcaster.NewRecorder(vcscheme.Scheme, v1.EventSource{Component: "vc-controller-manager"})

	cc.informerFactory = sharedInformers
	cc.queueList = make([]workqueue.TypedRateLimitingInterface[any], workers)
	cc.commandQueue = workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[any]())
	cc.cache = jobcache.New()
	cc.errTasks = newRateLimitingQueue()
	cc.recorder = recorder
	cc.workers = workers
	cc.maxRequeueNum = opt.MaxRequeueNum
	if cc.maxRequeueNum < 0 {
		cc.maxRequeueNum = -1
	}

	var i uint32
	for i = 0; i < workers; i++ {
		cc.queueList[i] = workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[any]())
	}

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

	if utilfeature.DefaultFeatureGate.Enabled(features.QueueCommandSync) {
		cc.cmdInformer = factory.Bus().V1alpha1().Commands()
		cc.cmdInformer.Informer().AddEventHandler(
			cache.FilteringResourceEventHandler{
				FilterFunc: func(obj interface{}) bool {
					switch v := obj.(type) {
					case *busv1alpha1.Command:
						if v.TargetObject != nil &&
							v.TargetObject.APIVersion == batchv1alpha1.SchemeGroupVersion.String() &&
							v.TargetObject.Kind == "Job" {
							return true
						}

						return false
					default:
						return false
					}
				},
				Handler: cache.ResourceEventHandlerFuncs{
					AddFunc: cc.addCommand,
				},
			},
		)
		cc.cmdLister = cc.cmdInformer.Lister()
		cc.cmdSynced = cc.cmdInformer.Informer().HasSynced
	}

	cc.podInformer = sharedInformers.Core().V1().Pods()
	cc.podInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    cc.addPod,
		UpdateFunc: cc.updatePod,
		DeleteFunc: cc.deletePod,
	})

	cc.podLister = cc.podInformer.Lister()
	cc.podSynced = cc.podInformer.Informer().HasSynced

	cc.pvcInformer = sharedInformers.Core().V1().PersistentVolumeClaims()
	cc.pvcLister = cc.pvcInformer.Lister()
	cc.pvcSynced = cc.pvcInformer.Informer().HasSynced

	cc.svcInformer = sharedInformers.Core().V1().Services()
	cc.svcLister = cc.svcInformer.Lister()
	cc.svcSynced = cc.svcInformer.Informer().HasSynced

	cc.pgInformer = factory.Scheduling().V1beta1().PodGroups()
	cc.pgInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		UpdateFunc: cc.updatePodGroup,
	})
	cc.pgLister = cc.pgInformer.Lister()
	cc.pgSynced = cc.pgInformer.Informer().HasSynced

	if utilfeature.DefaultFeatureGate.Enabled(features.PriorityClass) {
		cc.pcInformer = sharedInformers.Scheduling().V1().PriorityClasses()
		cc.pcLister = cc.pcInformer.Lister()
		cc.pcSynced = cc.pcInformer.Informer().HasSynced
	}

	cc.queueInformer = factory.Scheduling().V1beta1().Queues()
	cc.queueLister = cc.queueInformer.Lister()
	cc.queueSynced = cc.queueInformer.Informer().HasSynced

	cc.delayActionMap = make(map[jobIdentity]map[string]*delayAction)

	// Register actions
	state.SyncJob = cc.syncJob
	state.KillJob = cc.killJob
	state.KillTarget = cc.killTarget
	return nil
}

// Run start JobController.
func (cc *jobcontroller) Run(stopCh <-chan struct{}) {
	cc.informerFactory.Start(stopCh)
	cc.vcInformerFactory.Start(stopCh)

	for informerType, ok := range cc.informerFactory.WaitForCacheSync(stopCh) {
		if !ok {
			klog.Errorf("caches failed to sync: %v", informerType)
			return
		}
	}

	for informerType, ok := range cc.vcInformerFactory.WaitForCacheSync(stopCh) {
		if !ok {
			klog.Errorf("caches failed to sync: %v", informerType)
			return
		}
	}

	go wait.Until(cc.handleCommands, 0, stopCh)
	var i uint32
	for i = 0; i < cc.workers; i++ {
		go func(num uint32) {
			wait.Until(
				func() {
					cc.worker(num)
				},
				time.Second,
				stopCh)
		}(i)
	}

	go cc.cache.Run(stopCh)

	// Re-sync error tasks.
	go wait.Until(cc.processResyncTask, 0, stopCh)

	klog.Infof("JobController is running ...... ")
}

func (cc *jobcontroller) worker(i uint32) {
	klog.Infof("worker %d start ...... ", i)

	for cc.processNextReq(i) {
	}
}

func (cc *jobcontroller) belongsToThisRoutine(key string, count uint32) bool {
	val := cc.genHash(key)
	return val%cc.workers == count
}

func (cc *jobcontroller) getWorkerQueue(key string) workqueue.TypedRateLimitingInterface[any] {
	val := cc.genHash(key)
	queue := cc.queueList[val%cc.workers]
	return queue
}

func (cc *jobcontroller) genHash(key string) uint32 {
	hashVal := fnv.New32()
	hashVal.Write([]byte(key))
	return hashVal.Sum32()
}

func (cc *jobcontroller) processNextReq(count uint32) bool {
	queue := cc.queueList[count]
	queueItem, shutdown := queue.Get()
	if shutdown {
		klog.Errorf("Fail to pop item from queue")
		return false
	}
	defer queue.Done(queueItem)

	var (
		req        apis.Request
		delayedAct *delayAction
	)
	switch item := queueItem.(type) {
	case apis.Request:
		req = item
	case *delayAction:
		delayedAct = item
		req = item.request
		req.JobUid = item.job.uid
		req.Action = item.action
	default:
		klog.Errorf("Unexpected Job queue item type %T", queueItem)
		queue.Forget(queueItem)
		return true
	}

	key := jobcache.JobKeyByReq(&req)
	if !cc.belongsToThisRoutine(key, count) {
		klog.Errorf("should not occur The job does not belongs to this routine key:%s, worker:%d...... ", key, count)
		queueLocal := cc.getWorkerQueue(key)
		queueLocal.Add(queueItem)
		queue.Forget(queueItem)
		return true
	}

	klog.V(3).Infof("Try to handle request <%v>", req)

	// A normal Pod phase event can invalidate a delayed action before its token
	// reaches this worker. Delayed tokens never cancel themselves.
	if delayedAct == nil {
		cc.CleanPodDelayActionsIfNeed(req)
	}

	jobInfo, err := cc.cache.Get(key)
	if err != nil {
		// TODO(k82cn): ignore not-ready error.
		klog.Errorf("Failed to get job by <%v> from cache: %v", req, err)
		if delayedAct != nil {
			cc.discardDelayAction(delayedAct)
		}
		queue.Forget(queueItem)
		return true
	}
	if delayedAct != nil && !cc.delayActionCanExecute(delayedAct, jobInfo) {
		klog.V(4).Infof("Skip stale delayed action %s for Job<%s/%s>",
			delayedAct.action, req.Namespace, req.JobName)
		cc.discardDelayAction(delayedAct)
		queue.Forget(queueItem)
		return true
	}

	st := state.NewState(jobInfo)
	if st == nil {
		klog.Errorf("Invalid state <%s> of Job <%v/%v>",
			jobInfo.Job.Status.State, jobInfo.Job.Namespace, jobInfo.Job.Name)
		if delayedAct != nil {
			cc.discardDelayAction(delayedAct)
		}
		queue.Forget(queueItem)
		return true
	}

	delayAct := delayedAct
	if delayAct == nil {
		delayAct = applyPolicies(jobInfo.Job, &req)
	}

	if delayedAct == nil && delayAct.delay != 0 {
		klog.V(3).Infof("Execute <%v> on Job <%s/%s> after %s",
			delayAct.action, req.Namespace, req.JobName, delayAct.delay.String())
		cc.recordJobEvent(jobInfo.Job.Namespace, jobInfo.Job.Name, batchv1alpha1.ExecuteAction, fmt.Sprintf(
			"Execute action %s after %s", delayAct.action, delayAct.delay.String()))
		cc.scheduleDelayAction(req, delayAct)
		queue.Forget(queueItem)
		return true
	}

	// Claim only in the Job worker, immediately before execution. If a Running
	// event canceled this action after AddAfter made it ready, the token is no
	// longer current and cannot execute.
	if delayedAct != nil && !cc.claimDelayAction(delayedAct) {
		klog.V(4).Infof("Skip canceled delayed action %s for Job<%s/%s>",
			delayedAct.action, req.Namespace, req.JobName)
		queue.Forget(queueItem)
		return true
	}

	klog.V(3).Infof("Execute <%v> on Job <%s/%s> in <%s> by <%T>.",
		delayAct.action, req.Namespace, req.JobName, jobInfo.Job.Status.State.Phase, st)

	if delayAct.action != busv1alpha1.SyncJobAction {
		cc.recordJobEvent(jobInfo.Job.Namespace, jobInfo.Job.Name, batchv1alpha1.ExecuteAction, fmt.Sprintf(
			"Start to execute action %s ", delayAct.action))
	}

	action := GetStateAction(delayAct)

	if err := st.Execute(action); err != nil {
		// A claimed delayed token is single-use. Retry with an explicit Request;
		// its captured Job UID still protects a replacement Job with the same key.
		cc.handleJobError(queue, req, st, err, delayAct.action)
		if delayedAct != nil {
			queue.Forget(queueItem)
		}
		return true
	}

	// If no error, forget it.
	queue.Forget(queueItem)

	// If the action is not an internal action, cancel all delayed actions
	if !isInternalAction(delayAct.action) {
		cc.cleanupDelayActions(delayAct)
	}

	return true
}

// CleanPodDelayActionsIfNeed is used to clean delayed actions for Pod events when the pod phase changed:
// if the event is not PodPending event:
//   - cancel corresponding Pod Pending delayed action
//   - if the event is PodRunning state, cancel corresponding Pod Failed and Pod Evicted delayed actions
func (cc *jobcontroller) CleanPodDelayActionsIfNeed(req apis.Request) {
	// Skip cleaning delayed actions for non-pod events
	if !cc.isPodEvent(req) {
		return
	}

	if req.Event != busv1alpha1.PodPendingEvent {
		job := jobIdentity{key: jobcache.JobKeyByReq(&req), uid: req.JobUid}
		cc.delayActionMapLock.Lock()
		defer cc.delayActionMapLock.Unlock()

		if taskMap, exists := cc.delayActionMap[job]; exists {
			if delayAct, exists := taskMap[req.PodName]; exists {
				shouldCancel := false

				if delayAct.event == busv1alpha1.PodPendingEvent {
					// For PodPending delayed action, we need to check if the Pod UID matches
					// Because if a Pod is deleted and immediately recreated,
					// the new Pod's pending event may be queued before the old Pod's delete event
					if req.PodUID == delayAct.podUID {
						shouldCancel = true
					}
				}

				if (delayAct.event == busv1alpha1.PodFailedEvent || delayAct.event == busv1alpha1.PodEvictedEvent) &&
					req.Event == busv1alpha1.PodRunningEvent {
					shouldCancel = true
				}

				if shouldCancel {
					klog.V(3).Infof("Cancel delayed action <%v> for pod <%s> because of event <%s> of Job <%s>", delayAct.action, req.PodName, req.Event, delayAct.job.key)
					delete(taskMap, req.PodName)
					if len(taskMap) == 0 {
						delete(cc.delayActionMap, job)
					}
				}
			}
		}
	}
}

func (cc *jobcontroller) isPodEvent(req apis.Request) bool {
	return req.Event == busv1alpha1.PodPendingEvent ||
		req.Event == busv1alpha1.PodRunningEvent ||
		req.Event == busv1alpha1.PodFailedEvent ||
		req.Event == busv1alpha1.PodEvictedEvent
}

// scheduleDelayAction registers a new generation and lets the workqueue make
// it ready after the requested delay. AddAfter does not remove the registration:
// cancellation and execution are both serialized through the Job worker.
func (cc *jobcontroller) scheduleDelayAction(req apis.Request, delayAct *delayAction) {
	delayAct.request = req
	delayAct.request.JobUid = delayAct.job.uid
	delayAct.request.Action = delayAct.action

	cc.delayActionMapLock.Lock()
	m, ok := cc.delayActionMap[delayAct.job]
	if !ok {
		m = make(map[string]*delayAction)
		cc.delayActionMap[delayAct.job] = m
	}
	if oldDelayAct, exists := m[delayAct.podName]; exists {
		if sameDelayActionRegistration(oldDelayAct, delayAct) {
			cc.delayActionMapLock.Unlock()
			return
		}
	}
	m[delayAct.podName] = delayAct
	cc.delayActionMapLock.Unlock()

	cc.getWorkerQueue(delayAct.job.key).AddAfter(delayAct, delayAct.delay)
}

func sameDelayActionRegistration(left, right *delayAction) bool {
	return left.job == right.job &&
		left.taskName == right.taskName &&
		left.podName == right.podName &&
		left.podUID == right.podUID &&
		left.partition == right.partition &&
		left.event == right.event &&
		left.action == right.action
}

// delayActionCanExecute verifies both the generation token and the condition
// that originally scheduled it. Pending actions must never act on a replacement,
// running, or terminating Pod.
func (cc *jobcontroller) delayActionCanExecute(delayAct *delayAction, jobInfo *apis.JobInfo) bool {
	if jobInfo == nil || jobInfo.Job == nil ||
		delayAct.job.key != jobcache.JobKeyByName(jobInfo.Job.Namespace, jobInfo.Job.Name) ||
		delayAct.job.uid != jobInfo.Job.UID {
		return false
	}

	cc.delayActionMapLock.RLock()
	m, exists := cc.delayActionMap[delayAct.job]
	registered := exists && m[delayAct.podName] == delayAct
	cc.delayActionMapLock.RUnlock()
	if !registered {
		return false
	}

	if delayAct.event != busv1alpha1.PodPendingEvent {
		return true
	}
	pods, exists := jobInfo.Pods[delayAct.taskName]
	if !exists {
		return false
	}
	pod, exists := pods[delayAct.podName]
	return exists && pod.UID == delayAct.podUID &&
		pod.DeletionTimestamp == nil && pod.Status.Phase == v1.PodPending
}

// claimDelayAction atomically claims a validated delayed action and removes all
// equivalent delayed actions in its scope. It prevents multiple pod tokens in
// one task or partition from executing the same action more than once.
func (cc *jobcontroller) claimDelayAction(currentDelayAction *delayAction) bool {
	cc.delayActionMapLock.Lock()
	defer cc.delayActionMapLock.Unlock()

	m, exists := cc.delayActionMap[currentDelayAction.job]
	if !exists {
		return false
	}
	registered, exists := m[currentDelayAction.podName]
	if !exists || registered != currentDelayAction {
		return false
	}

	cc.cleanupDelayActionsLocked(currentDelayAction)
	return true
}

// discardDelayAction removes only this generation. It must not remove a newer
// action that replaced the token while it was waiting in the queue.
func (cc *jobcontroller) discardDelayAction(delayAct *delayAction) {
	cc.delayActionMapLock.Lock()
	defer cc.delayActionMapLock.Unlock()

	m, exists := cc.delayActionMap[delayAct.job]
	if !exists || m[delayAct.podName] != delayAct {
		return
	}
	delete(m, delayAct.podName)
	if len(m) == 0 {
		delete(cc.delayActionMap, delayAct.job)
	}
}

func (cc *jobcontroller) cleanupDelayActionsForJob(job jobIdentity) {
	cc.delayActionMapLock.Lock()
	defer cc.delayActionMapLock.Unlock()
	delete(cc.delayActionMap, job)
}

func (cc *jobcontroller) handleJobError(queue workqueue.TypedRateLimitingInterface[any], req apis.Request, st state.State, err error, action busv1alpha1.Action) {
	if cc.maxRequeueNum == -1 || queue.NumRequeues(req) < cc.maxRequeueNum {
		klog.V(2).Infof("Failed to handle Job <%s/%s>: %v",
			req.Namespace, req.JobName, err)
		queue.AddRateLimited(req)
		return
	}

	cc.recordJobEvent(req.Namespace, req.JobName, batchv1alpha1.ExecuteAction,
		fmt.Sprintf("Job failed on action %s for retry limit reached", action))
	klog.Warningf("Terminating Job <%s/%s> and releasing resources", req.Namespace, req.JobName)

	if err = st.Execute(state.Action{Action: busv1alpha1.TerminateJobAction}); err != nil {
		klog.Errorf("Failed to terminate Job<%s/%s>: %v", req.Namespace, req.JobName, err)
	}
	klog.Warningf("Dropping job<%s/%s> out of the queue: %v because max retries has reached",
		req.Namespace, req.JobName, err)
}

// cleanupDelayActions cleans up delayed actions
// After a delayed action is executed, other delayed actions of the same type need to be invalidated to avoid duplicate execution
// Parameters:
//   - currentDelayAction: the delayed action that has just been executed
//
// Implementation logic:
//  1. Get the type of current delayed action (Job level, Task level or Pod level)
//  2. Iterate through all delayed actions under this Job
//  3. If the delayed action type matches, invalidate it by removing it from the map
//
// Usage scenarios:
//   - When a Pod failure triggers Job termination, invalidate other Job delayed actions under this Job
func (cc *jobcontroller) cleanupDelayActions(currentDelayAction *delayAction) {
	cc.delayActionMapLock.Lock()
	defer cc.delayActionMapLock.Unlock()
	cc.cleanupDelayActionsLocked(currentDelayAction)
}

// cleanupDelayActionsLocked removes delayed actions equivalent to
// currentDelayAction. The caller must hold delayActionMapLock.
func (cc *jobcontroller) cleanupDelayActionsLocked(currentDelayAction *delayAction) {
	m, exists := cc.delayActionMap[currentDelayAction.job]
	if !exists {
		return
	}

	for podName, delayAct := range m {
		if !sameDelayActionScope(currentDelayAction, delayAct) {
			continue
		}
		klog.V(3).Infof("Invalidate delayed action <%v> for pod <%s> because of event <%s> and action <%s> of Job <%s>", delayAct.action, delayAct.podName, currentDelayAction.event, currentDelayAction.action, delayAct.job.key)
		delete(m, podName)
	}
	if len(m) == 0 {
		delete(cc.delayActionMap, currentDelayAction.job)
	}
}

func sameDelayActionScope(currentDelayAction, delayAct *delayAction) bool {
	if delayAct.job != currentDelayAction.job {
		return false
	}

	actionType := GetActionType(currentDelayAction.action)
	if GetActionType(delayAct.action) != actionType {
		return false
	}

	switch actionType {
	case TaskAction:
		return delayAct.taskName == currentDelayAction.taskName
	case PodAction:
		return delayAct.podName == currentDelayAction.podName
	case PartitionAction:
		return delayAct.taskName == currentDelayAction.taskName &&
			delayAct.partition == currentDelayAction.partition
	default:
		return true
	}
}
