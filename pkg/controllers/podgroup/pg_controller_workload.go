/*
Copyright 2026 The Volcano Authors.

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

package podgroup

import (
	"context"
	"fmt"
	"reflect"
	"slices"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"

	batch "volcano.sh/apis/pkg/apis/batch/v1alpha1"
	"volcano.sh/apis/pkg/apis/helpers"
	scheduling "volcano.sh/apis/pkg/apis/scheduling/v1beta1"
)

type workloadRequest struct {
	kind      string
	namespace string
	name      string
	uid       types.UID
}

func (pg *pgcontroller) enqueueWorkload(kind string, obj metav1.Object) {
	pg.workloadQueue.Add(workloadRequest{kind: kind, namespace: obj.GetNamespace(), name: obj.GetName(), uid: obj.GetUID()})
}

func (pg *pgcontroller) enqueuePodOwner(pod *v1.Pod) {
	if ref := metav1.GetControllerOf(pod); pg.rsInformer != nil && ref != nil && (ref.Kind == "ReplicaSet" || ref.Kind == "StatefulSet") {
		pg.workloadQueue.Add(workloadRequest{kind: ref.Kind, namespace: pod.Namespace, name: ref.Name, uid: ref.UID})
	}
}

func (pg *pgcontroller) workloadWorker() {
	for pg.processNextWorkload() {
	}
}

func (pg *pgcontroller) processNextWorkload() bool {
	req, shutdown := pg.workloadQueue.Get()
	if shutdown {
		return false
	}
	defer pg.workloadQueue.Done(req)
	if err := pg.syncWorkload(req); err != nil {
		klog.Errorf("Failed to reconcile %s %s/%s: %v", req.kind, req.namespace, req.name, err)
		pg.workloadQueue.AddRateLimited(req)
	} else {
		pg.workloadQueue.Forget(req)
	}
	return true
}

func (pg *pgcontroller) syncWorkload(req workloadRequest) error {
	var owner metav1.Object
	var selector *metav1.LabelSelector
	var replicas int32
	var revision string
	// Event payloads can precede pod creation by many updates. Fetch the current
	// workload instead of treating an old replicas=0 notification as authority.
	switch req.kind {
	case "ReplicaSet":
		rs, err := pg.kubeClient.AppsV1().ReplicaSets(req.namespace).Get(context.TODO(), req.name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		owner, selector, replicas = rs, rs.Spec.Selector, ptr.Deref(rs.Spec.Replicas, 1)
	case "StatefulSet":
		sts, err := pg.kubeClient.AppsV1().StatefulSets(req.namespace).Get(context.TODO(), req.name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		owner, selector, replicas, revision = sts, sts.Spec.Selector, ptr.Deref(sts.Spec.Replicas, 1), sts.Status.UpdateRevision
	default:
		return nil
	}
	if owner.GetUID() != req.uid || owner.GetDeletionTimestamp() != nil {
		return nil
	}
	labelSelector := labels.Everything()
	if replicas > 0 {
		var err error
		labelSelector, err = metav1.LabelSelectorAsSelector(selector)
		if err != nil {
			return err
		}
	}
	pods, err := pg.kubeClient.CoreV1().Pods(req.namespace).List(context.TODO(), metav1.ListOptions{LabelSelector: labelSelector.String()})
	if err != nil {
		return err
	}
	var fallback *v1.Pod
	for i := range pods.Items {
		pod := &pods.Items[i]
		if !metav1.IsControlledBy(pod, owner) {
			continue
		}
		if replicas == 0 {
			// Include terminating pods: their delete events will enqueue cleanup
			// again, after the last nonterminal member has actually disappeared.
			if pod.Status.Phase != v1.PodSucceeded && pod.Status.Phase != v1.PodFailed {
				return nil
			}
			continue
		}
		if !activePod(pod) || !slices.Contains(pg.schedulerNames, pod.Spec.SchedulerName) {
			continue
		}
		if name := pod.Annotations[scheduling.KubeGroupNameAnnotationKey]; name != "" && name != helpers.GeneratePodgroupName(pod) {
			continue
		}
		if revision != "" && pod.Labels[controllerRevisionHashLabelKey] != revision {
			fallback = pod
			continue
		}
		return pg.createOrUpdateNormalPodPG(pod)
	}
	if replicas == 0 {
		return pg.deleteWorkloadPodGroup(owner)
	}
	// During a StatefulSet rollout the update revision may not have a pod yet.
	// Keep existing members schedulable without overwriting group requirements
	// from an older revision when the group already exists.
	if fallback != nil {
		return pg.createNormalPodPGIfNotExist(fallback)
	}
	return nil
}

func (pg *pgcontroller) deleteWorkloadPodGroup(owner metav1.Object) error {
	name := batch.PodgroupNamePrefix + string(owner.GetUID())
	groups := pg.vcClient.SchedulingV1beta1().PodGroups(owner.GetNamespace())
	group, err := groups.Get(context.TODO(), name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !metav1.IsControlledBy(group, owner) {
		return fmt.Errorf("PodGroup %s/%s is not controlled by workload %s", owner.GetNamespace(), name, owner.GetName())
	}
	klog.V(4).Infof("Delete PodGroup %s/%s for scaled-down workload %s with no active pods", owner.GetNamespace(), name, owner.GetName())
	err = groups.Delete(context.TODO(), name, metav1.DeleteOptions{
		Preconditions: &metav1.Preconditions{UID: &group.UID, ResourceVersion: &group.ResourceVersion},
	})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

func activePod(pod *v1.Pod) bool {
	return pod.DeletionTimestamp == nil && pod.Status.Phase != v1.PodSucceeded && pod.Status.Phase != v1.PodFailed
}

func (pg *pgcontroller) updatePod(oldObj, newObj interface{}) {
	oldPod, newPod := oldObj.(*v1.Pod), newObj.(*v1.Pod)
	if oldPod.UID != newPod.UID || !reflect.DeepEqual(oldPod.OwnerReferences, newPod.OwnerReferences) ||
		!reflect.DeepEqual(oldPod.Annotations, newPod.Annotations) || !reflect.DeepEqual(oldPod.Labels, newPod.Labels) ||
		!reflect.DeepEqual(oldPod.Spec, newPod.Spec) || oldPod.Status.Phase != newPod.Status.Phase ||
		!reflect.DeepEqual(oldPod.DeletionTimestamp, newPod.DeletionTimestamp) {
		pg.enqueuePodOwner(oldPod)
		pg.addPod(newPod)
	}
}

func (pg *pgcontroller) deletePod(obj interface{}) {
	if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = tombstone.Obj
	}
	if pod, ok := obj.(*v1.Pod); ok {
		pg.enqueuePodOwner(pod)
	}
}

func (pg *pgcontroller) deletePodGroup(obj interface{}) {
	if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = tombstone.Obj
	}
	group, ok := obj.(*scheduling.PodGroup)
	if !ok {
		klog.Errorf("Failed to convert %v to PodGroup", obj)
		return
	}
	ref := metav1.GetControllerOf(group)
	if ref == nil || group.Name != batch.PodgroupNamePrefix+string(ref.UID) {
		return
	}
	if pg.rsInformer != nil && (ref.Kind == "ReplicaSet" || ref.Kind == "StatefulSet") {
		pg.workloadQueue.Add(workloadRequest{kind: ref.Kind, namespace: group.Namespace, name: ref.Name, uid: ref.UID})
		return
	}
	pods, err := pg.podLister.Pods(group.Namespace).List(labels.Everything())
	if err != nil {
		klog.Errorf("Failed to list pods for deleted PodGroup %s/%s: %v", group.Namespace, group.Name, err)
		return
	}
	for _, pod := range pods {
		if activePod(pod) && helpers.GeneratePodgroupName(pod) == group.Name {
			pg.addPod(pod)
		}
	}
}
