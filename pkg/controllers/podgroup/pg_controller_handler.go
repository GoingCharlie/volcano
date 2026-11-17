/*
Copyright 2019 The Volcano Authors.

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
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	batchv1alpha1 "volcano.sh/apis/pkg/apis/batch/v1alpha1"
	"volcano.sh/apis/pkg/apis/helpers"
	scheduling "volcano.sh/apis/pkg/apis/scheduling/v1beta1"
	topologyv1alpha1 "volcano.sh/apis/pkg/apis/topology/v1alpha1"
	"volcano.sh/volcano/pkg/controllers/util"
)

const (
	controllerRevisionHashLabelKey = "controller-revision-hash"
	podGroupCleanupRetryInterval   = 5 * time.Second
)

type cleanupRequest struct {
	kind      string
	namespace string
	name      string
	uid       types.UID
}

type podRequest struct {
	podName      string
	podNamespace string
	podUID       types.UID
	cleanup      cleanupRequest
}

func (pg *pgcontroller) addPod(obj interface{}) {
	pod, ok := obj.(*v1.Pod)
	if !ok {
		klog.Errorf("Failed to convert %v to v1.Pod", obj)
		return
	}

	req := podRequest{
		podName:      pod.Name,
		podNamespace: pod.Namespace,
		podUID:       pod.UID,
	}

	pg.queue.Add(req)
}

func (pg *pgcontroller) addReplicaSet(obj interface{}) {
	rs, ok := obj.(*appsv1.ReplicaSet)
	if !ok {
		klog.Errorf("Failed to convert %v to appsv1.ReplicaSet", obj)
		return
	}

	if *rs.Spec.Replicas == 0 {
		pg.enqueueCleanup("ReplicaSet", rs)
		return
	}

	// In the rolling upgrade scenario, the addReplicasSet(replicas=0) event may be received before
	// the updateReplicaSet(replicas=1) event. In this event, need to create PodGroup for the pod.
	if *rs.Spec.Replicas > 0 {
		selector := metav1.LabelSelector{MatchLabels: rs.Spec.Selector.MatchLabels}
		podList, err := pg.kubeClient.CoreV1().Pods(rs.Namespace).List(context.TODO(),
			metav1.ListOptions{LabelSelector: metav1.FormatLabelSelector(&selector)})
		if err != nil {
			klog.Errorf("Failed to list pods for ReplicaSet %s: %v", klog.KObj(rs), err)
			return
		}
		if podList != nil && len(podList.Items) > 0 {
			pod := podList.Items[0]
			klog.V(4).Infof("Try to create or update podgroup for pod %s", klog.KObj(&pod))
			if !slices.Contains(pg.schedulerNames, pod.Spec.SchedulerName) {
				klog.V(4).Infof("Pod %s field SchedulerName is not matched", klog.KObj(&pod))
				return
			}
			// If the pod is already associated with a podgroup, skip creating a new one.
			if pgName := pod.Annotations[scheduling.KubeGroupNameAnnotationKey]; pgName != "" {
				klog.V(4).Infof("Pod %s is already associated with a podgroup %s", klog.KObj(&pod), pgName)
				return
			}
			// In the upgrade scenario, need to synchronize and update the podgroup-related information.
			// For example, if you update `scheduling.volcano.sh/group-min-member: "2"`, the podgroup's `minMember` needs to be updated to 2.
			err := pg.createOrUpdateNormalPodPG(&pod)
			if err != nil {
				klog.Errorf("Failed to create or update PodGroup for pod %s: %v", klog.KObj(&pod), err)
			}
		}
	}
}

func (pg *pgcontroller) updateReplicaSet(oldObj, newObj interface{}) {
	pg.addReplicaSet(newObj)
}

func (pg *pgcontroller) addStatefulSet(obj interface{}) {
	sts, ok := obj.(*appsv1.StatefulSet)
	if !ok {
		klog.Errorf("Failed to convert %v to appsv1.StatefulSet", obj)
		return
	}

	if *sts.Spec.Replicas == 0 {
		pg.enqueueCleanup("StatefulSet", sts)
		return
	}

	// In the rolling upgrade scenario, the addStatefulSet(replicas=0) event may be received before
	// the updateStatefulSet(replicas=1) event, and after the addPod event for the new created pod.
	// In this event, need to create PodGroup for the pod.
	if *sts.Spec.Replicas > 0 {
		matchLabels := make(map[string]string, len(sts.Spec.Selector.MatchLabels)+1)
		for k, v := range sts.Spec.Selector.MatchLabels {
			matchLabels[k] = v
		}
		matchLabels[controllerRevisionHashLabelKey] = sts.Status.UpdateRevision
		selector := metav1.LabelSelector{MatchLabels: matchLabels}
		labelSelector, err := metav1.LabelSelectorAsSelector(&selector)
		if err != nil {
			klog.Errorf("Failed to convert label selector for StatefulSet <%s/%s>: %v", sts.Namespace, sts.Name, err)
			return
		}
		pods, err := pg.podInformer.Lister().List(labelSelector)
		if err != nil {
			klog.Errorf("Failed to list pods for StatefulSet <%s/%s>: %v", sts.Namespace, sts.Name, err)
			return
		}
		if len(pods) > 0 {
			pod := pods[0]
			klog.V(4).Infof("Try to create or update podgroup for pod %s/%s when statefulset add or update", pod.Namespace, pod.Name)
			if !slices.Contains(pg.schedulerNames, pod.Spec.SchedulerName) {
				klog.V(4).Infof("Pod %s field SchedulerName is not matched", klog.KObj(pod))
				return
			}

			// If the pod is already associated with a podgroup, skip creating a new one. This scenario is applicable to LeaderWorkerSet,
			// which will create podgroups by itself, and Volcano does not need to create a podgroup for statefulset.
			if pgName := pod.Annotations[scheduling.KubeGroupNameAnnotationKey]; pgName != "" {
				klog.V(4).Infof("Pod %s is already associated with a podgroup %s", klog.KObj(pod), pgName)
				return
			}

			err := pg.createOrUpdateNormalPodPG(pod)
			if err != nil {
				klog.Errorf("Failed to create or update PodGroup for pod <%s/%s>: %v", pod.Namespace, pod.Name, err)
			}
		}
	}
}

func (pg *pgcontroller) updateStatefulSet(oldObj, newObj interface{}) {
	pg.addStatefulSet(newObj)
}

func (pg *pgcontroller) enqueueCleanup(kind string, owner metav1.Object) {
	pg.queue.Add(podRequest{cleanup: cleanupRequest{
		kind: kind, namespace: owner.GetNamespace(), name: owner.GetName(), uid: owner.GetUID(),
	}})
}

// Cleanup retries carry the workload identity, so they still work after its
// last Pod disappears and no further workload status updates are emitted.
func (pg *pgcontroller) cleanupPodGroup(req cleanupRequest) (bool, error) {
	var owner metav1.Object
	var replicas *int32
	var err error
	switch req.kind {
	case "ReplicaSet":
		var rs *appsv1.ReplicaSet
		rs, err = pg.kubeClient.AppsV1().ReplicaSets(req.namespace).Get(context.TODO(), req.name, metav1.GetOptions{})
		if err == nil {
			owner, replicas = rs, rs.Spec.Replicas
		}
	case "StatefulSet":
		var sts *appsv1.StatefulSet
		sts, err = pg.kubeClient.AppsV1().StatefulSets(req.namespace).Get(context.TODO(), req.name, metav1.GetOptions{})
		if err == nil {
			owner, replicas = sts, sts.Spec.Replicas
		}
	default:
		return false, nil
	}
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if owner.GetUID() != req.uid || replicas == nil || *replicas != 0 {
		return false, nil
	}
	return pg.deleteUnusedPodGroup(owner)
}

// Delete only after the current workload is scaled down and its nonterminal
// pods have disappeared. A zero-replica notification alone can be stale.
// The boolean result requests a later check while member pods still exist.
func (pg *pgcontroller) deleteUnusedPodGroup(owner metav1.Object) (bool, error) {
	name := batchv1alpha1.PodgroupNamePrefix + string(owner.GetUID())
	groups := pg.vcClient.SchedulingV1beta1().PodGroups(owner.GetNamespace())
	group, err := groups.Get(context.TODO(), name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !metav1.IsControlledBy(group, owner) {
		return false, nil
	}
	pods, err := pg.kubeClient.CoreV1().Pods(owner.GetNamespace()).List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return false, err
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if metav1.IsControlledBy(pod, owner) && pod.Status.Phase != v1.PodSucceeded && pod.Status.Phase != v1.PodFailed {
			return true, nil
		}
	}
	err = groups.Delete(context.TODO(), name, metav1.DeleteOptions{
		Preconditions: &metav1.Preconditions{UID: &group.UID, ResourceVersion: &group.ResourceVersion},
	})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	return false, err
}

func (pg *pgcontroller) deletePodGroup(obj interface{}) {
	if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = tombstone.Obj
	}
	group, ok := obj.(*scheduling.PodGroup)
	if !ok {
		return
	}
	owner := metav1.GetControllerOf(group)
	if owner == nil || group.Name != batchv1alpha1.PodgroupNamePrefix+string(owner.UID) {
		return
	}
	pods, err := pg.podLister.Pods(group.Namespace).List(labels.Everything())
	if err != nil {
		klog.Errorf("Failed to list pods for deleted PodGroup %s: %v", klog.KObj(group), err)
		return
	}
	for _, pod := range pods {
		name := pod.Annotations[scheduling.KubeGroupNameAnnotationKey]
		if (name == "" || name == group.Name) && helpers.GeneratePodgroupName(pod) == group.Name {
			pg.addPod(pod)
		}
	}
}

func (pg *pgcontroller) updatePodAnnotations(pod *v1.Pod, pgName string) error {
	if pod.Annotations[scheduling.KubeGroupNameAnnotationKey] == "" {
		patch := map[string]any{
			"metadata": map[string]any{
				"uid": pod.UID,
				"annotations": map[string]string{
					scheduling.KubeGroupNameAnnotationKey: pgName,
				},
			},
		}

		patchBytes, err := json.Marshal(patch)
		if err != nil {
			klog.Errorf("Failed to json.Marshal pod annotation: %v", err)
			return err
		}

		if _, err := pg.kubeClient.CoreV1().Pods(pod.Namespace).Patch(context.TODO(), pod.Name, types.StrategicMergePatchType, patchBytes, metav1.PatchOptions{}); err != nil {
			klog.Errorf("Failed to update pod <%s/%s>: %v", pod.Namespace, pod.Name, err)
			return err
		}
		klog.V(4).Infof("Bound Pod <%s/%s> to PodGroup <%s/%s>", pod.Namespace, pod.Name, pod.Namespace, pgName)
	} else {
		if pod.Annotations[scheduling.KubeGroupNameAnnotationKey] != pgName {
			klog.Errorf("normal pod %s/%s annotations %s value is not %s, but %s", pod.Namespace, pod.Name,
				scheduling.KubeGroupNameAnnotationKey, pgName, pod.Annotations[scheduling.KubeGroupNameAnnotationKey])
		}
	}
	return nil
}

func (pg *pgcontroller) getAnnotationsFromUpperRes(pod *v1.Pod) (map[string]string, error) {
	annotations := make(map[string]string)
	for _, reference := range pod.OwnerReferences {
		var owner metav1.Object
		var err error
		switch schema.FromAPIVersionAndKind(reference.APIVersion, reference.Kind).GroupKind() {
		case appsv1.SchemeGroupVersion.WithKind("ReplicaSet").GroupKind():
			owner, err = pg.kubeClient.AppsV1().ReplicaSets(pod.Namespace).Get(context.TODO(), reference.Name, metav1.GetOptions{})
		case appsv1.SchemeGroupVersion.WithKind("DaemonSet").GroupKind():
			owner, err = pg.kubeClient.AppsV1().DaemonSets(pod.Namespace).Get(context.TODO(), reference.Name, metav1.GetOptions{})
		case appsv1.SchemeGroupVersion.WithKind("StatefulSet").GroupKind():
			owner, err = pg.kubeClient.AppsV1().StatefulSets(pod.Namespace).Get(context.TODO(), reference.Name, metav1.GetOptions{})
		case batchv1.SchemeGroupVersion.WithKind("Job").GroupKind():
			owner, err = pg.kubeClient.BatchV1().Jobs(pod.Namespace).Get(context.TODO(), reference.Name, metav1.GetOptions{})
		default:
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("get %s %s/%s for Pod %s: %w", reference.Kind, pod.Namespace, reference.Name, pod.Name, err)
		}
		if owner.GetUID() != reference.UID {
			return nil, fmt.Errorf("%s %s/%s UID does not match Pod %s owner", reference.Kind, pod.Namespace, reference.Name, pod.Name)
		}
		for k, v := range owner.GetAnnotations() {
			if _, ok := annotations[k]; !ok {
				annotations[k] = v
			}
		}
	}
	return annotations, nil
}

func (pg *pgcontroller) getMinMemberFromUpperRes(upperAnnotations map[string]string, namespance, name string) int32 {
	minMember := int32(1)

	if minMemberAnno, ok := upperAnnotations[scheduling.VolcanoGroupMinMemberAnnotationKey]; ok {
		minMemberFromAnno, err := strconv.ParseInt(minMemberAnno, 10, 32)
		if err != nil {
			klog.Errorf("Failed to convert minMemberAnnotation of Pod owners <%s/%s> into number: %v, minMember remains as 1",
				namespance, name, err)
			return minMember
		}
		if minMemberFromAnno < 0 {
			klog.Errorf("minMemberAnnotation %d is not positive, minMember remains as 1", minMemberFromAnno)
			return minMember
		}
		minMember = int32(minMemberFromAnno)
	}

	return minMember
}

// Inherit annotations from upper resources.
func (pg *pgcontroller) inheritUpperAnnotations(upperAnnotations map[string]string, obj *scheduling.PodGroup) {
	if pg.inheritOwnerAnnotations {
		for k, v := range upperAnnotations {
			if strings.HasPrefix(k, scheduling.AnnotationPrefix) {
				obj.Annotations[k] = v
			}
		}
	}
}

func (pg *pgcontroller) createNormalPodPGIfNotExist(pod *v1.Pod) error {
	pgName := helpers.GeneratePodgroupName(pod)

	if _, err := pg.pgLister.PodGroups(pod.Namespace).Get(pgName); err != nil {
		if !apierrors.IsNotFound(err) {
			klog.Errorf("Failed to get normal PodGroup for Pod <%s/%s>: %v",
				pod.Namespace, pod.Name, err)
			return err
		}

		// A group deletion can be observed before the Pod informer catches up.
		// Revalidate only before creation; existing groups remain read-only.
		current, err := pg.kubeClient.CoreV1().Pods(pod.Namespace).Get(context.TODO(), pod.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if current.UID != pod.UID ||
			current.DeletionTimestamp != nil || current.Status.Phase == v1.PodSucceeded || current.Status.Phase == v1.PodFailed ||
			helpers.GeneratePodgroupName(current) != pgName {
			return nil
		}
		if name := current.Annotations[scheduling.KubeGroupNameAnnotationKey]; name != "" && name != pgName {
			return nil
		}
		pod = current

		podGroup, err := pg.buildPodGroupFromPod(pod, pgName)
		if err != nil {
			return err
		}
		if _, err := pg.vcClient.SchedulingV1beta1().PodGroups(pod.Namespace).Create(context.TODO(), podGroup, metav1.CreateOptions{}); err != nil {
			if !apierrors.IsAlreadyExists(err) {
				klog.Errorf("Failed to create normal PodGroup for Pod <%s/%s>: %v",
					pod.Namespace, pod.Name, err)
				return err
			} else {
				klog.V(4).Infof("PodGroup <%s/%s> already exists for Pod <%s/%s>",
					pod.Namespace, pgName, pod.Namespace, pod.Name)
			}
		} else {
			klog.V(4).Infof("PodGroup <%s/%s> created for Pod <%s/%s>",
				pod.Namespace, pgName, pod.Namespace, pod.Name)
			// The Pod may disappear after the GET but before CREATE. Reuse the
			// cleanup queue so a concurrently scaled-down owner cannot leak a group.
			if owner := metav1.GetControllerOf(pod); owner != nil {
				switch schema.FromAPIVersionAndKind(owner.APIVersion, owner.Kind).GroupKind() {
				case appsv1.SchemeGroupVersion.WithKind("ReplicaSet").GroupKind(), appsv1.SchemeGroupVersion.WithKind("StatefulSet").GroupKind():
					pg.queue.AddAfter(podRequest{cleanup: cleanupRequest{
						kind: owner.Kind, namespace: pod.Namespace, name: owner.Name, uid: owner.UID,
					}}, podGroupCleanupRetryInterval)
				}
			}
		}
	}

	return pg.updatePodAnnotations(pod, pgName)
}

// When statefulSet is updated, its associated pod template may change.
// In such cases, we need to update the corresponding PodGroup simultaneously.
func (pg *pgcontroller) createOrUpdateNormalPodPG(pod *v1.Pod) error {
	pgName := helpers.GeneratePodgroupName(pod)

	if podGroup, err := pg.pgLister.PodGroups(pod.Namespace).Get(pgName); err != nil {
		if !apierrors.IsNotFound(err) {
			klog.Errorf("Failed to get normal PodGroup for Pod <%s/%s>: %v",
				pod.Namespace, pod.Name, err)
			return err
		}

		newPodGroup, err := pg.buildPodGroupFromPod(pod, pgName)
		if err != nil {
			return err
		}
		if _, err := pg.vcClient.SchedulingV1beta1().PodGroups(pod.Namespace).Create(context.TODO(), newPodGroup, metav1.CreateOptions{}); err != nil {
			if !apierrors.IsAlreadyExists(err) {
				klog.Errorf("Failed to create normal PodGroup for Pod <%s/%s>: %v",
					pod.Namespace, pod.Name, err)
				return err
			} else {
				klog.V(4).Infof("PodGroup <%s/%s> already exists for Pod <%s/%s>",
					pod.Namespace, pgName, pod.Namespace, pod.Name)
			}
		} else {
			klog.V(4).Infof("PodGroup <%s/%s> created for Pod <%s/%s>",
				pod.Namespace, pgName, pod.Namespace, pod.Name)
		}
	} else {
		podGroupToUpdate := podGroup.DeepCopy()
		needUpdate, err := pg.shouldUpdateExistingPodGroup(podGroupToUpdate, pod)
		if err != nil {
			return err
		}
		if needUpdate {
			_, err = pg.vcClient.SchedulingV1beta1().PodGroups(pod.Namespace).Update(context.TODO(), podGroupToUpdate, metav1.UpdateOptions{})
			if err != nil {
				klog.Errorf("Failed to update PodGroup <%s/%s>: %v", pod.Namespace, pgName, err)
				return err
			}
			klog.V(4).Infof("PodGroup <%s/%s> updated for Pod <%s/%s>", pod.Namespace, pgName, pod.Namespace, pod.Name)
		}
	}

	return pg.updatePodAnnotations(pod, pgName)
}

func (pg *pgcontroller) buildPodGroupFromPod(pod *v1.Pod, pgName string) (*scheduling.PodGroup, error) {
	var minMember = int32(1)
	var ownerAnnotations = make(map[string]string)
	if pg.inheritOwnerAnnotations {
		var err error
		ownerAnnotations, err = pg.getAnnotationsFromUpperRes(pod)
		if err != nil {
			return nil, err
		}
		minMember = pg.getMinMemberFromUpperRes(ownerAnnotations, pod.Namespace, pod.Name)
	}
	minResources := util.CalTaskRequests(pod, minMember)
	obj := &scheduling.PodGroup{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       pod.Namespace,
			Name:            pgName,
			OwnerReferences: newPGOwnerReferences(pod),
			Annotations:     map[string]string{},
			Labels:          map[string]string{},
		},
		Spec: scheduling.PodGroupSpec{
			MinMember:         minMember,
			PriorityClassName: pod.Spec.PriorityClassName,
			MinResources:      &minResources,
		},
		Status: scheduling.PodGroupStatus{
			Phase: scheduling.PodGroupPending,
		},
	}

	pg.inheritUpperAnnotations(ownerAnnotations, obj)
	// Individual annotations on pods would overwrite annotations inherited from upper resources.
	if queueName, ok := pod.Annotations[scheduling.QueueNameAnnotationKey]; ok {
		obj.Spec.Queue = queueName
	}

	if value, ok := pod.Annotations[scheduling.PodPreemptable]; ok {
		obj.Annotations[scheduling.PodPreemptable] = value
	}
	if value, ok := pod.Annotations[scheduling.CooldownTime]; ok {
		obj.Annotations[scheduling.CooldownTime] = value
	}
	if value, ok := pod.Annotations[scheduling.RevocableZone]; ok {
		obj.Annotations[scheduling.RevocableZone] = value
	}
	if value, ok := pod.Labels[scheduling.PodPreemptable]; ok {
		obj.Labels[scheduling.PodPreemptable] = value
	}
	if value, ok := pod.Labels[scheduling.CooldownTime]; ok {
		obj.Labels[scheduling.CooldownTime] = value
	}

	if value, found := pod.Annotations[scheduling.JDBMinAvailable]; found {
		obj.Annotations[scheduling.JDBMinAvailable] = value
	} else if value, found := pod.Annotations[scheduling.JDBMaxUnavailable]; found {
		obj.Annotations[scheduling.JDBMaxUnavailable] = value
	}

	// Parse and set NetworkTopology from Pod annotations
	if networkTopology := parseNetworkTopologyFromPod(pod); networkTopology != nil {
		obj.Spec.NetworkTopology = networkTopology

		highestTier := 1 // default value
		if networkTopology.HighestTierAllowed != nil {
			highestTier = *networkTopology.HighestTierAllowed
		}
		klog.V(4).Infof("Set NetworkTopology for PodGroup %s/%s: mode:%s, highestTier:%d",
			obj.Namespace, obj.Name, networkTopology.Mode, highestTier)
	}

	return obj, nil
}

// parseNetworkTopologyFromPod extracts NetworkTopology configuration from Pod annotations
func parseNetworkTopologyFromPod(pod *v1.Pod) *scheduling.NetworkTopologySpec {
	annotations := pod.Annotations
	if annotations == nil {
		return nil
	}

	// Check if any NetworkTopology annotations are present
	modeStr, modeExists := annotations[topologyv1alpha1.NetworkTopologyModeAnnotationKey]
	tierStr, tierExists := annotations[topologyv1alpha1.NetworkTopologyHighestTierAnnotationKey]

	if !modeExists && !tierExists {
		return nil
	}

	nt := &scheduling.NetworkTopologySpec{
		Mode: scheduling.HardNetworkTopologyMode,
	}

	// Parse mode
	if modeExists {
		mode := scheduling.NetworkTopologyMode(strings.ToLower(modeStr))
		if mode == scheduling.HardNetworkTopologyMode || mode == scheduling.SoftNetworkTopologyMode {
			nt.Mode = mode
		} else {
			klog.Warningf("Invalid network topology mode %q in pod %s/%s, using default 'hard' mode", modeStr, pod.Namespace, pod.Name)
		}
	}

	// Parse highest tier allowed
	if tierExists {
		if tier, err := strconv.Atoi(tierStr); err == nil {
			nt.HighestTierAllowed = &tier
		} else {
			klog.Warningf("Invalid network topology highest tier %s in pod %s/%s: %v", tierStr, pod.Namespace, pod.Name, err)
		}
	}

	return nt
}

func (pg *pgcontroller) shouldUpdateExistingPodGroup(podGroup *scheduling.PodGroup, pod *v1.Pod) (bool, error) {
	isUpdated := false

	newPodGroup, err := pg.buildPodGroupFromPod(pod, podGroup.Name)
	if err != nil {
		return false, err
	}
	if !reflect.DeepEqual(newPodGroup.Spec, podGroup.Spec) {
		podGroup.Spec = newPodGroup.Spec
		isUpdated = true
	}

	if !reflect.DeepEqual(newPodGroup.Labels, podGroup.Labels) {
		podGroup.Labels = newPodGroup.Labels
		isUpdated = true
	}

	if !reflect.DeepEqual(newPodGroup.Annotations, podGroup.Annotations) {
		podGroup.Annotations = newPodGroup.Annotations
		isUpdated = true
	}

	return isUpdated, nil
}

func newPGOwnerReferences(pod *v1.Pod) []metav1.OwnerReference {
	if len(pod.OwnerReferences) != 0 {
		for _, ownerReference := range pod.OwnerReferences {
			if ownerReference.Controller != nil && *ownerReference.Controller {
				return pod.OwnerReferences
			}
		}
	}

	gvk := schema.GroupVersionKind{
		Group:   v1.SchemeGroupVersion.Group,
		Version: v1.SchemeGroupVersion.Version,
		Kind:    "Pod",
	}
	ref := metav1.NewControllerRef(pod, gvk)
	return []metav1.OwnerReference{*ref}
}
