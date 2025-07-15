package cronjob

func (cc *cronjobcontroller) enqueueController(obj interface{}) {

}
func (cc *cronjobcontroller) updateCronJob(old interface{}, curr interface{}) {}
func (cc *cronjobcontroller) addJob(obj interface{}) {
	// job := obj.(*batchv1.Job)
	// if job.DeletionTimestamp != nil {
	// 	// on a restart of the controller, it's possible a new job shows up in a state that
	// 	// is already pending deletion. Prevent the job from being a creation observation.
	// 	cc.deleteJob(job)
	// 	return
	// }

	// // If it has a ControllerRef, that's all that matters.
	// if controllerRef := metav1.GetControllerOf(job); controllerRef != nil {
	// 	cronJob := cc.resolveControllerRef(job.Namespace, controllerRef)
	// 	if cronJob == nil {
	// 		return
	// 	}
	// 	cc.enqueueController(cronJob)
	// 	return
	// }
}
func (cc *cronjobcontroller) deleteJob(obj interface{}) {
	// job, ok := obj.(*batchv1.Job)

	// // When a delete is dropped, the relist will notice a job in the store not
	// // in the list, leading to the insertion of a tombstone object which contains
	// // the deleted key/value. Note that this value might be stale.
	// if !ok {
	// 	tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
	// 	if !ok {
	// 		utilruntime.HandleError(fmt.Errorf("couldn't get object from tombstone %#v", obj))
	// 		return
	// 	}
	// 	job, ok = tombstone.Obj.(*batchv1.Job)
	// 	if !ok {
	// 		utilruntime.HandleError(fmt.Errorf("tombstone contained object that is not a Job %#v", obj))
	// 		return
	// 	}
	// }

	// controllerRef := metav1.GetControllerOf(job)
	// if controllerRef == nil {
	// 	// No controller should care about orphans being deleted.
	// 	return
	// }
	// cronJob := jm.resolveControllerRef(job.Namespace, controllerRef)
	// if cronJob == nil {
	// 	return
	// }
	// cc.enqueueController(cronJob)
}
func (cc *cronjobcontroller) updateJob(old, cur interface{}) {
	// curJob := cur.(*batchv1.Job)
	// oldJob := old.(*batchv1.Job)
	// if curJob.ResourceVersion == oldJob.ResourceVersion {
	// 	// Periodic resync will send update events for all known jobs.
	// 	// Two different versions of the same jobs will always have different RVs.
	// 	return
	// }

	// curControllerRef := metav1.GetControllerOf(curJob)
	// oldControllerRef := metav1.GetControllerOf(oldJob)
	// controllerRefChanged := !reflect.DeepEqual(curControllerRef, oldControllerRef)
	// if controllerRefChanged && oldControllerRef != nil {
	// 	// The ControllerRef was changed. Sync the old controller, if any.
	// 	if cronJob := jm.resolveControllerRef(oldJob.Namespace, oldControllerRef); cronJob != nil {
	// 		cc.enqueueController(cronJob)
	// 	}
	// }

	// // If it has a ControllerRef, that's all that matters.
	// if curControllerRef != nil {
	// 	cronJob := jm.resolveControllerRef(curJob.Namespace, curControllerRef)
	// 	if cronJob == nil {
	// 		return
	// 	}
	// 	cc.enqueueController(cronJob)
	// 	return
	// }
}
