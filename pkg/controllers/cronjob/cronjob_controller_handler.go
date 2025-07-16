package cronjob

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	batchv1 "volcano.sh/apis/pkg/apis/batch/v1alpha1"
)

// ToDO
func (cc *cronjobcontroller) enqueueController(obj interface{}) {
	key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
	if err != nil {
		klog.Errorf("Failed to get key for object <%v>: %v", obj, err)
		return
	}
	cc.queue.Add(key)
}
func (cc *cronjobcontroller) enqueueControllerAfter(obj interface{}, duration time.Duration) {
	key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
	if err != nil {
		klog.Errorf("Failed to get key for object <%v>: %v", obj, err)
		return
	}
	cc.queue.AddAfter(key, duration)
}
func (cc *cronjobcontroller) updateCronJob(old interface{}, curr interface{}) {
	oldCJ, okOld := old.(*batchv1.CronJob)
	newCJ, okNew := curr.(*batchv1.CronJob)

	if !okOld || !okNew {
		// typecasting of one failed, handle this better, may be log entry
		return
	}
	// if the change in schedule results in next requeue having to be sooner than it already was,
	// it will be handled here by the queue. If the next requeue is further than previous schedule,
	// the sync loop will essentially be a no-op for the already queued key with old schedule.
	if oldCJ.Spec.Schedule != newCJ.Spec.Schedule || !equal(oldCJ.Spec.TimeZone, newCJ.Spec.TimeZone) {
		// schedule changed, change the requeue time, pass nil recorder so that syncCronJob will output any warnings
		sched, err := cron.ParseStandard(formatSchedule(newCJ, nil))
		if err != nil {
			// this is likely a user error in defining the spec value
			// we should log the error and not reconcile this cronjob until an update to spec
			klog.V(2).Info("Unparseable schedule for cronjob", "cronjob", klog.KObj(newCJ), "schedule", newCJ.Spec.Schedule, "err", err)
			cc.recorder.Eventf(newCJ, corev1.EventTypeWarning, "UnParseableCronJobSchedule", "unparseable schedule for cronjob: %s", newCJ.Spec.Schedule)
			return
		}
		now := cc.now()
		t := nextScheduleTimeDuration(newCJ, now, sched)

		cc.enqueueControllerAfter(curr, *t)
		return
	}

	// other parameters changed, requeue this now and if this gets triggered
	// within deadline, sync loop will work on the CJ otherwise updates will be handled
	// during the next schedule
	// TODO: need to handle the change of spec.JobTemplate.metadata.labels explicitly
	//   to cleanup jobs with old labels
	cc.enqueueController(curr)
}
func (cc *cronjobcontroller) addJob(obj interface{}) {
	job := obj.(*batchv1.Job)
	if job.DeletionTimestamp != nil {
		// on a restart of the controller, it's possible a new job shows up in a state that
		// is already pending deletion. Prevent the job from being a creation observation.
		cc.deleteJob(job)
		return
	}

	// If it has a ControllerRef, that's all that matters.
	if controllerRef := metav1.GetControllerOf(job); controllerRef != nil {
		cronJob := cc.resolveControllerRef(job.Namespace, controllerRef)
		if cronJob == nil {
			return
		}
		cc.enqueueController(cronJob)
		return
	}
}
func (cc *cronjobcontroller) resolveControllerRef(namespace string, controllerRef *metav1.OwnerReference) *batchv1.CronJob {
	// We can't look up by UID, so look up by Name and then verify UID.
	// Don't even try to look up by Name if it's the wrong Kind.
	if controllerRef.Kind != controllerKind.Kind {
		return nil
	}
	cronJob, err := cc.cronJobList.CronJobs(namespace).Get(controllerRef.Name)
	if err != nil {
		return nil
	}
	if cronJob.UID != controllerRef.UID {
		// The controller we found with this Name is not the same one that the
		// ControllerRef points to.
		return nil
	}
	return cronJob
}

func (cc *cronjobcontroller) deleteJob(obj interface{}) {
	job, ok := obj.(*batchv1.Job)

	// When a delete is dropped, the relist will notice a job in the store not
	// in the list, leading to the insertion of a tombstone object which contains
	// the deleted key/value. Note that this value might be stale.
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("couldn't get object from tombstone %#v", obj))
			return
		}
		job, ok = tombstone.Obj.(*batchv1.Job)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("tombstone contained object that is not a Job %#v", obj))
			return
		}
	}

	controllerRef := metav1.GetControllerOf(job)
	if controllerRef == nil {
		// No controller should care about orphans being deleted.
		return
	}
	cronJob := cc.resolveControllerRef(job.Namespace, controllerRef)
	if cronJob == nil {
		return
	}
	cc.enqueueController(cronJob)
}
func (cc *cronjobcontroller) updateJob(old, cur interface{}) {
	curJob := cur.(*batchv1.Job)
	oldJob := old.(*batchv1.Job)
	if curJob.ResourceVersion == oldJob.ResourceVersion {
		// Periodic resync will send update events for all known jobs.
		// Two different versions of the same jobs will always have different RVs.
		return
	}

	curControllerRef := metav1.GetControllerOf(curJob)
	oldControllerRef := metav1.GetControllerOf(oldJob)
	controllerRefChanged := !reflect.DeepEqual(curControllerRef, oldControllerRef)
	if controllerRefChanged && oldControllerRef != nil {
		// The ControllerRef was changed. Sync the old controller, if any.
		if cronJob := cc.resolveControllerRef(oldJob.Namespace, oldControllerRef); cronJob != nil {
			cc.enqueueController(cronJob)
		}
	}

	// If it has a ControllerRef, that's all that matters.
	if curControllerRef != nil {
		cronJob := cc.resolveControllerRef(curJob.Namespace, curControllerRef)
		if cronJob == nil {
			return
		}
		cc.enqueueController(cronJob)
		return
	}
}
func (cc *cronjobcontroller) getJobsToBeReconciled(cronJob *batchv1.CronJob) ([]*batchv1.Job, error) {
	jobList, err := cc.jobLister.Jobs(cronJob.Namespace).List(labels.Everything())
	if err != nil {
		return nil, err
	}

	jobsToBeReconciled := []*batchv1.Job{}

	for _, job := range jobList {
		// If it has a ControllerRef, that's all that matters.
		if controllerRef := metav1.GetControllerOf(job); controllerRef != nil && controllerRef.Name == cronJob.Name {
			// this job is needs to be reconciled
			jobsToBeReconciled = append(jobsToBeReconciled, job)
		}
	}
	return jobsToBeReconciled, nil
}
func (cc *cronjobcontroller) cleanupFinishedJobs(cronJob *batchv1.CronJob, jobsToBeReconciled []*batchv1.Job) bool {
	if cronJob.Spec.FailedJobsHistoryLimit == nil && cronJob.Spec.SuccessfulJobsHistoryLimit == nil {
		return false
	}

	updateStatus := false
	failedJobs := []*batchv1.Job{}
	successfulJobs := []*batchv1.Job{}

	for _, job := range jobsToBeReconciled {
		isFinished, finishedStatus := cc.getFinishedStatus(job)
		if isFinished && finishedStatus == batchv1.JobComplete {
			successfulJobs = append(successfulJobs, job)
		} else if isFinished && finishedStatus == batchv1.JobFailed {
			failedJobs = append(failedJobs, job)
		}
	}

	if cronJob.Spec.SuccessfulJobsHistoryLimit != nil &&
		cc.removeOldestJobs(cronJob,
			successfulJobs,
			*cronJob.Spec.SuccessfulJobsHistoryLimit) {
		updateStatus = true
	}

	if cronJob.Spec.FailedJobsHistoryLimit != nil &&
		cc.removeOldestJobs(cronJob,
			failedJobs,
			*cronJob.Spec.FailedJobsHistoryLimit) {
		updateStatus = true
	}

	return updateStatus
}
func (cc *cronjobcontroller) syncCronJob(ronJob *batchv1.CronJob, jobsToBeReconciled []*batchv1.Job) (*time.Duration, bool, error) {
	return nil, false, nil
}
func equal[T comparable](a, b *T) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	if a == nil {
		return true
	}
	return *a == *b
}
func formatSchedule(cj *batchv1.CronJob, recorder record.EventRecorder) string {
	if strings.Contains(cj.Spec.Schedule, "TZ") {
		if recorder != nil {
			recorder.Eventf(cj, corev1.EventTypeWarning, "UnsupportedSchedule", "CRON_TZ or TZ used in schedule %q is not officially supported, see https://kubernetes.io/docs/concepts/workloads/controllers/cron-jobs/ for more details", cj.Spec.Schedule)
		}

		return cj.Spec.Schedule
	}

	if cj.Spec.TimeZone != nil {
		if _, err := time.LoadLocation(*cj.Spec.TimeZone); err != nil {
			return cj.Spec.Schedule
		}

		return fmt.Sprintf("TZ=%s %s", *cj.Spec.TimeZone, cj.Spec.Schedule)
	}

	return cj.Spec.Schedule
}
func (cc *cronjobcontroller) removeOldestJobs(cj *batchv1.CronJob, js []*batchv1.Job, maxJobs int32) bool {

	updateStatus := false
	numToDelete := len(js) - int(maxJobs)
	if numToDelete <= 0 {
		return updateStatus
	}
	klog.V(4).Info("Cleaning up jobs from CronJob list", "deletejobnum", numToDelete, "jobnum", len(js), "cronjob", klog.KObj(cj))

	sort.Sort(byJobStartTime(js))
	//func deleteJob(cc *cronjobcontroller, cj *batchv1.CronJob, job *batchv1.Job, recorder record.EventRecorder)
	for i := 0; i < numToDelete; i++ {
		klog.V(4).Info("Removing job from CronJob list", "job", js[i].Name, "cronjob", klog.KObj(cj))
		if deleteJob(cc, cj, js[i], cc.recorder) {
			updateStatus = true
		}
	}
	return updateStatus
}
