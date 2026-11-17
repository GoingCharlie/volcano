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
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"

	"volcano.sh/apis/pkg/apis/helpers"
	scheduling "volcano.sh/apis/pkg/apis/scheduling/v1beta1"
	topologyv1alpha1 "volcano.sh/apis/pkg/apis/topology/v1alpha1"
	"volcano.sh/volcano/pkg/controllers/util"
)

const (
	controllerRevisionHashLabelKey = "controller-revision-hash"
)

type podRequest struct {
	podName      string
	podNamespace string
	podUID       types.UID
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
	pg.enqueuePodOwner(pod)
}

func (pg *pgcontroller) addReplicaSet(obj interface{}) {
	rs, ok := obj.(*appsv1.ReplicaSet)
	if !ok {
		klog.Errorf("Failed to convert %v to appsv1.ReplicaSet", obj)
		return
	}
	pg.enqueueWorkload("ReplicaSet", rs)
}

func (pg *pgcontroller) updateReplicaSet(oldObj, newObj interface{}) {
	oldRS := oldObj.(*appsv1.ReplicaSet)
	newRS := newObj.(*appsv1.ReplicaSet)
	if oldRS.UID != newRS.UID || !reflect.DeepEqual(oldRS.Spec, newRS.Spec) ||
		!reflect.DeepEqual(oldRS.Annotations, newRS.Annotations) || !reflect.DeepEqual(oldRS.DeletionTimestamp, newRS.DeletionTimestamp) {
		pg.addReplicaSet(newObj)
	}
}

func (pg *pgcontroller) addStatefulSet(obj interface{}) {
	sts, ok := obj.(*appsv1.StatefulSet)
	if !ok {
		klog.Errorf("Failed to convert %v to appsv1.StatefulSet", obj)
		return
	}
	pg.enqueueWorkload("StatefulSet", sts)
}

func (pg *pgcontroller) updateStatefulSet(oldObj, newObj interface{}) {
	oldSTS := oldObj.(*appsv1.StatefulSet)
	newSTS := newObj.(*appsv1.StatefulSet)
	if oldSTS.UID != newSTS.UID || !reflect.DeepEqual(oldSTS.Spec, newSTS.Spec) ||
		!reflect.DeepEqual(oldSTS.Annotations, newSTS.Annotations) || oldSTS.Status.UpdateRevision != newSTS.Status.UpdateRevision ||
		!reflect.DeepEqual(oldSTS.DeletionTimestamp, newSTS.DeletionTimestamp) {
		pg.addStatefulSet(newObj)
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

func (pg *pgcontroller) getAnnotationsFromUpperRes(pod *v1.Pod) map[string]string {
	var annotations = make(map[string]string)

	for _, reference := range pod.OwnerReferences {
		if reference.Kind != "" && reference.Name != "" {
			tmp := make(map[string]string)
			switch reference.Kind {
			case "ReplicaSet":
				rs, err := pg.kubeClient.AppsV1().ReplicaSets(pod.Namespace).Get(context.TODO(), reference.Name, metav1.GetOptions{})
				if err != nil {
					klog.Errorf("Failed to get upper %s for Pod <%s/%s>: %v", reference.Kind, pod.Namespace, reference.Name, err)
					continue
				}
				tmp = rs.Annotations
			case "DaemonSet":
				ds, err := pg.kubeClient.AppsV1().DaemonSets(pod.Namespace).Get(context.TODO(), reference.Name, metav1.GetOptions{})
				if err != nil {
					klog.Errorf("Failed to get upper %s for Pod <%s/%s>: %v", reference.Kind, pod.Namespace, reference.Name, err)
					continue
				}
				tmp = ds.Annotations
			case "StatefulSet":
				ss, err := pg.kubeClient.AppsV1().StatefulSets(pod.Namespace).Get(context.TODO(), reference.Name, metav1.GetOptions{})
				if err != nil {
					klog.Errorf("Failed to get upper %s for Pod <%s/%s>: %v", reference.Kind, pod.Namespace, reference.Name, err)
					continue
				}
				tmp = ss.Annotations
			case "Job":
				job, err := pg.kubeClient.BatchV1().Jobs(pod.Namespace).Get(context.TODO(), reference.Name, metav1.GetOptions{})
				if err != nil {
					klog.Errorf("Failed to get upper %s for Pod <%s/%s>: %v", reference.Kind, pod.Namespace, reference.Name, err)
					continue
				}
				tmp = job.Annotations
			}

			for k, v := range tmp {
				if _, ok := annotations[k]; !ok {
					annotations[k] = v
				}
			}
		}
	}

	return annotations
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

// ensureNormalPodGroup also validates objects returned after an AlreadyExists
// response: informer caches can lag behind a delete or concurrent creation.
func (pg *pgcontroller) ensureNormalPodGroup(pod *v1.Pod, pgName string) (*scheduling.PodGroup, error) {
	group, err := pg.pgLister.PodGroups(pod.Namespace).Get(pgName)
	if apierrors.IsNotFound(err) {
		group, err = pg.vcClient.SchedulingV1beta1().PodGroups(pod.Namespace).Create(context.TODO(), pg.buildPodGroupFromPod(pod, pgName), metav1.CreateOptions{})
		if apierrors.IsAlreadyExists(err) {
			group, err = pg.vcClient.SchedulingV1beta1().PodGroups(pod.Namespace).Get(context.TODO(), pgName, metav1.GetOptions{})
		} else if err == nil {
			klog.V(4).Infof("PodGroup <%s/%s> created for Pod <%s/%s>", pod.Namespace, pgName, pod.Namespace, pod.Name)
		}
	}
	if err != nil {
		return nil, err
	}
	if group.DeletionTimestamp != nil {
		return nil, fmt.Errorf("PodGroup %s/%s is being deleted", pod.Namespace, pgName)
	}
	owner := metav1.GetControllerOf(group)
	for _, expected := range newPGOwnerReferences(pod) {
		if expected.Controller != nil && *expected.Controller && owner != nil &&
			owner.UID == expected.UID && owner.Name == expected.Name && owner.Kind == expected.Kind {
			return group, nil
		}
	}
	return nil, fmt.Errorf("PodGroup %s/%s has an unexpected controller owner", pod.Namespace, pgName)
}

func (pg *pgcontroller) createNormalPodPGIfNotExist(pod *v1.Pod) error {
	pgName := helpers.GeneratePodgroupName(pod)
	if _, err := pg.ensureNormalPodGroup(pod, pgName); err != nil {
		return err
	}
	return pg.updatePodAnnotations(pod, pgName)
}

// Workload changes can update group requirements even when pods already have
// their automatically generated group-name annotation.
func (pg *pgcontroller) createOrUpdateNormalPodPG(pod *v1.Pod) error {
	pgName := helpers.GeneratePodgroupName(pod)
	group, err := pg.ensureNormalPodGroup(pod, pgName)
	if err != nil {
		return err
	}
	updated := group.DeepCopy()
	if pg.shouldUpdateExistingPodGroup(updated, pod) {
		if _, err := pg.vcClient.SchedulingV1beta1().PodGroups(pod.Namespace).Update(context.TODO(), updated, metav1.UpdateOptions{}); err != nil {
			return err
		}
		klog.V(4).Infof("PodGroup <%s/%s> updated for Pod <%s/%s>", pod.Namespace, pgName, pod.Namespace, pod.Name)
	}
	return pg.updatePodAnnotations(pod, pgName)
}

func (pg *pgcontroller) buildPodGroupFromPod(pod *v1.Pod, pgName string) *scheduling.PodGroup {
	var minMember = int32(1)
	var ownerAnnotations = make(map[string]string)
	if pg.inheritOwnerAnnotations {
		ownerAnnotations = pg.getAnnotationsFromUpperRes(pod)
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

	return obj
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

func (pg *pgcontroller) shouldUpdateExistingPodGroup(podGroup *scheduling.PodGroup, pod *v1.Pod) bool {
	isUpdated := false

	newPodGroup := pg.buildPodGroupFromPod(pod, podGroup.Name)
	// Admission resolves an unspecified queue from the namespace on CREATE
	// only. Preserve that resolved queue when reconciling other requirements.
	if newPodGroup.Spec.Queue == "" {
		newPodGroup.Spec.Queue = podGroup.Spec.Queue
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

	return isUpdated
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
