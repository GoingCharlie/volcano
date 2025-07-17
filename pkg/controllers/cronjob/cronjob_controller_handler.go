package cronjob

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	ref "k8s.io/client-go/tools/reference"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/controller/cronjob/metrics"
	"k8s.io/utils/ptr"
	batchv1 "volcano.sh/apis/pkg/apis/batch/v1alpha1"
	"volcano.sh/apis/pkg/apis/scheduling/scheme"
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
	if oldCJ.Spec.Schedule != newCJ.Spec.Schedule || !ptr.Equal(oldCJ.Spec.TimeZone, newCJ.Spec.TimeZone) {
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
		isFinish, phase := IsJobFinished(job)
		if isFinish {
			found := inActiveList(cronJob, job.ObjectMeta.UID)
			if found {
				deleteFromActiveList(cronJob, job.ObjectMeta.UID)
				cc.recorder.Eventf(cronJob, corev1.EventTypeNormal, "SawCompletedJob", "Saw completed job: %s", job.Name)
				updateStatus = true
			}
			switch phase {
			case batchv1.Completed:
				jobFinishTime := job.Status.State.LastTransitionTime
				if cronJob.Status.LastSuccessfulTime == nil {
					cronJob.Status.LastSuccessfulTime = &jobFinishTime
					updateStatus = true
				}
				if !jobFinishTime.IsZero() && cronJob.Status.LastSuccessfulTime != nil && jobFinishTime.After(cronJob.Status.LastSuccessfulTime.Time) {
					cronJob.Status.LastSuccessfulTime = &jobFinishTime // 注意：赋值时需要取地址
					updateStatus = true
				}
				successfulJobs = append(successfulJobs, job)
			case batchv1.Failed:
				failedJobs = append(failedJobs, job)
			}
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

func (cc *cronjobcontroller) syncCronJob(cronJob *batchv1.CronJob,
	jobs []*batchv1.Job) (*time.Duration, bool, error) {
	now := cc.now()
	updateStatus := false

	childrenJobs := make(map[types.UID]bool)
	for _, j := range jobs {
		childrenJobs[j.ObjectMeta.UID] = true
		found := inActiveList(cronJob, j.ObjectMeta.UID)
		isFinish, _ := IsJobFinished(j)
		if !found && !isFinish {
			cjCopy, err := getCronJobApi(cc.vcClient, cronJob.Namespace, cronJob.Name)
			if err != nil {
				return nil, updateStatus, err
			}
			if inActiveList(cjCopy, j.ObjectMeta.UID) {
				cronJob = cjCopy
				continue
			}
			cc.recorder.Eventf(cronJob, corev1.EventTypeWarning, "UnexpectedJob", "Saw a job that the controller did not create or forgot: %s", j.Name)
			// We found an unfinished job that has us as the parent, but it is not in our Active list.
			// This could happen if we crashed right after creating the Job and before updating the status,
			// or if our jobs list is newer than our cj status after a relist, or if someone intentionally created
			// a job that they wanted us to adopt.
		}
	}

	// Remove any job reference from the active list if the corresponding job does not exist any more.
	// Otherwise, the cronjob may be stuck in active mode forever even though there is no matching
	// job running.
	for _, j := range cronJob.Status.Active {
		_, found := childrenJobs[j.UID]
		if found {
			continue
		}
		// Explicitly try to get the job from api-server to avoid a slow watch not able to update
		// the job lister on time, giving an unwanted miss
		_, err := getJobApi(cc.vcClient, j.Namespace, j.Name)
		switch {
		case errors.IsNotFound(err):
			// The job is actually missing, delete from active list and schedule a new one if within
			// deadline
			cc.recorder.Eventf(cronJob, corev1.EventTypeNormal, "MissingJob", "Active job went missing: %v", j.Name)
			deleteFromActiveList(cronJob, j.UID)
			updateStatus = true
		case err != nil:
			return nil, updateStatus, err
		}
		// the job is missing in the lister but found in api-server
	}

	if cronJob.DeletionTimestamp != nil {
		// The CronJob is being deleted.
		// Don't do anything other than updating status.
		return nil, updateStatus, nil
	}

	if cronJob.Spec.TimeZone != nil {
		timeZone := ptr.Deref(cronJob.Spec.TimeZone, "")
		if _, err := time.LoadLocation(timeZone); err != nil {
			klog.V(4).Info("Not starting job because timeZone is invalid", "cronjob", klog.KObj(cronJob), "timeZone", timeZone, "err", err)
			cc.recorder.Eventf(cronJob, corev1.EventTypeWarning, "UnknownTimeZone", "invalid timeZone: %q: %s", timeZone, err)
			return nil, updateStatus, nil
		}
	}

	if cronJob.Spec.Suspend != nil && *cronJob.Spec.Suspend {
		klog.V(4).Info("Not starting job because the cron is suspended", "cronjob", klog.KObj(cronJob))
		return nil, updateStatus, nil
	}

	sched, err := cron.ParseStandard(formatSchedule(cronJob, cc.recorder))
	if err != nil {
		// this is likely a user error in defining the spec value
		// we should log the error and not reconcile this cronjob until an update to spec
		klog.V(2).Info("Unparseable schedule", "cronjob", klog.KObj(cronJob), "schedule", cronJob.Spec.Schedule, "err", err)
		cc.recorder.Eventf(cronJob, corev1.EventTypeWarning, "UnparseableSchedule", "unparseable schedule: %q : %s", cronJob.Spec.Schedule, err)
		return nil, updateStatus, nil
	}

	scheduledTime, err := nextScheduleTime(cronJob, now, sched, cc.recorder)
	if err != nil {
		// this is likely a user error in defining the spec value
		// we should log the error and not reconcile this cronjob until an update to spec
		klog.V(2).Info("Invalid schedule", "cronjob", klog.KObj(cronJob), "schedule", cronJob.Spec.Schedule, "err", err)
		cc.recorder.Eventf(cronJob, corev1.EventTypeWarning, "InvalidSchedule", "invalid schedule: %s : %s", cronJob.Spec.Schedule, err)
		return nil, updateStatus, nil
	}
	if scheduledTime == nil {
		// no unmet start time, return cj,.
		// The only time this should happen is if queue is filled after restart.
		// Otherwise, the queue is always suppose to trigger sync function at the time of
		// the scheduled time, that will give atleast 1 unmet time schedule
		klog.V(4).Info("No unmet start times", "cronjob", klog.KObj(cronJob))
		t := nextScheduleTimeDuration(cronJob, now, sched)
		return t, updateStatus, nil
	}

	tooLate := false
	if cronJob.Spec.StartingDeadlineSeconds != nil {
		tooLate = scheduledTime.Add(time.Second * time.Duration(*cronJob.Spec.StartingDeadlineSeconds)).Before(now)
	}
	if tooLate {
		klog.V(4).Info("Missed starting window", "cronjob", klog.KObj(cronJob))
		cc.recorder.Eventf(cronJob, corev1.EventTypeWarning, "MissSchedule", "Missed scheduled time to start a job: %s", scheduledTime.UTC().Format(time.RFC1123Z))

		// TODO: Since we don't set LastScheduleTime when not scheduling, we are going to keep noticing
		// the miss every cycle.  In order to avoid sending multiple events, and to avoid processing
		// the cj again and again, we could set a Status.LastMissedTime when we notice a miss.
		// Then, when we call getRecentUnmetScheduleTimes, we can take max(creationTimestamp,
		// Status.LastScheduleTime, Status.LastMissedTime), and then so we won't generate
		// and event the next time we process it, and also so the user looking at the status
		// can see easily that there was a missed execution.
		t := nextScheduleTimeDuration(cronJob, now, sched)
		return t, updateStatus, nil
	}
	if inActiveListByName(cronJob, &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      getJobName(cronJob, *scheduledTime),
			Namespace: cronJob.Namespace,
		}}) || cronJob.Status.LastScheduleTime.Equal(&metav1.Time{Time: *scheduledTime}) {
		klog.V(4).Info("Not starting job because the scheduled time is already processed", "cronjob", klog.KObj(cronJob), "schedule", scheduledTime)
		t := nextScheduleTimeDuration(cronJob, now, sched)
		return t, updateStatus, nil
	}
	if cronJob.Spec.ConcurrencyPolicy == batchv1.ForbidConcurrent && len(cronJob.Status.Active) > 0 {
		// Regardless which source of information we use for the set of active jobs,
		// there is some risk that we won't see an active job when there is one.
		// (because we haven't seen the status update to the SJ or the created pod).
		// So it is theoretically possible to have concurrency with Forbid.
		// As long the as the invocations are "far enough apart in time", this usually won't happen.
		//
		// TODO: for Forbid, we could use the same name for every execution, as a lock.
		// With replace, we could use a name that is deterministic per execution time.
		// But that would mean that you could not inspect prior successes or failures of Forbid jobs.
		klog.V(4).Info("Not starting job because prior execution is still running and concurrency policy is Forbid", "cronjob", klog.KObj(cronJob))
		cc.recorder.Eventf(cronJob, corev1.EventTypeNormal, "JobAlreadyActive", "Not starting job because prior execution is running and concurrency policy is Forbid")
		t := nextScheduleTimeDuration(cronJob, now, sched)
		return t, updateStatus, nil
	}
	if cronJob.Spec.ConcurrencyPolicy == batchv1.ReplaceConcurrent {
		for _, j := range cronJob.Status.Active {
			klog.V(4).Info("Deleting job that was still running at next scheduled start time", "job", klog.KRef(j.Namespace, j.Name))
			job, err := getJobApi(cc.vcClient, j.Namespace, j.Name)
			if err != nil {
				cc.recorder.Eventf(cronJob, corev1.EventTypeWarning, "FailedGet", "Get job: %v", err)
				return nil, updateStatus, err
			}
			if !deleteJob(cc.vcClient, cronJob, job, cc.recorder) {
				return nil, updateStatus, fmt.Errorf("could not replace job %s/%s", job.Namespace, job.Name)
			}
			updateStatus = true
		}
	}

	jobAlreadyExists := false
	jobReq, err := getJobFromTemplate(cronJob, *scheduledTime)
	if err != nil {
		klog.Error(err, "Unable to make Job from template", "cronjob", klog.KObj(cronJob))
		return nil, updateStatus, err
	}
	jobResp, err := createJobApi(cc.vcClient, cronJob.Namespace, jobReq)
	switch {
	case errors.HasStatusCause(err, corev1.NamespaceTerminatingCause):
		// if the namespace is being terminated, we don't have to do
		// anything because any creation will fail
		return nil, updateStatus, err
	case errors.IsAlreadyExists(err):
		// If the job is created by other actor, assume it has updated the cronjob status accordingly.
		// However, if the job was created by cronjob controller, this means we've previously created the job
		// but failed to update the active list in the status, in which case we should reattempt to add the job
		// into the active list and update the status.
		jobAlreadyExists = true
		job, err := getJobApi(cc.vcClient, jobReq.GetNamespace(), jobReq.GetName())
		if err != nil {
			return nil, updateStatus, err
		}
		jobResp = job

		// check that this job is owned by cronjob controller, otherwise do nothing and assume external controller
		// is updating the status.
		if !metav1.IsControlledBy(job, cronJob) {
			return nil, updateStatus, nil
		}

		// Recheck if the job is missing from the active list before attempting to update the status again.
		found := inActiveList(cronJob, job.ObjectMeta.UID)
		if found {
			return nil, updateStatus, nil
		}
	case err != nil:
		// default error handling
		cc.recorder.Eventf(cronJob, corev1.EventTypeWarning, "FailedCreate", "Error creating job: %v", err)
		return nil, updateStatus, err
	}

	if jobAlreadyExists {
		klog.Info("Job already exists", "cronjob", klog.KObj(cronJob), "job", klog.KObj(jobReq))
	} else {
		metrics.CronJobCreationSkew.Observe(jobResp.ObjectMeta.GetCreationTimestamp().Sub(*scheduledTime).Seconds())
		klog.V(4).Info("Created Job", "job", klog.KObj(jobResp), "cronjob", klog.KObj(cronJob))
		cc.recorder.Eventf(cronJob, corev1.EventTypeNormal, "SuccessfulCreate", "Created job %v", jobResp.Name)
	}

	// ------------------------------------------------------------------ //

	// If this process restarts at this point (after posting a job, but
	// before updating the status), then we might try to start the job on
	// the next time.  Actually, if we re-list the SJs and Jobs on the next
	// iteration of syncAll, we might not see our own status update, and
	// then post one again.  So, we need to use the job name as a lock to
	// prevent us from making the job twice (name the job with hash of its
	// scheduled time).

	// Add the just-started job to the status list.
	jobRef, err := getRef(jobResp)
	if err != nil {
		klog.V(2).Info("Unable to make object reference", "cronjob", klog.KObj(cronJob), "err", err)
		return nil, updateStatus, fmt.Errorf("unable to make object reference for job for %s", klog.KObj(cronJob))
	}
	cronJob.Status.Active = append(cronJob.Status.Active, convertToVolcanoJobRef(jobRef))
	cronJob.Status.LastScheduleTime = &metav1.Time{Time: *scheduledTime}
	updateStatus = true

	t := nextScheduleTimeDuration(cronJob, now, sched)
	return t, updateStatus, nil
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
		if deleteJob(cc.vcClient, cj, js[i], cc.recorder) {
			updateStatus = true
		}
	}
	return updateStatus
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
func getRef(object runtime.Object) (*corev1.ObjectReference, error) {
	return ref.GetReference(scheme.Scheme, object)
}
func convertToVolcanoJobRef(k8sRef *corev1.ObjectReference) batchv1.JobReference {
	return batchv1.JobReference{
		Kind:           k8sRef.Kind,
		Namespace:      k8sRef.Namespace,
		Name:           k8sRef.Name,
		UID:            k8sRef.UID,
		APIVersion:     k8sRef.APIVersion,
		ResourceVesion: k8sRef.ResourceVersion,
	}
}
