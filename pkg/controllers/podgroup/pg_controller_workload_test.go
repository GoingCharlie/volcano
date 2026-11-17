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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	kubeclient "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/ptr"

	"volcano.sh/apis/pkg/apis/helpers"
	scheduling "volcano.sh/apis/pkg/apis/scheduling/v1beta1"
	vcclient "volcano.sh/apis/pkg/client/clientset/versioned/fake"
)

func rolloutObjects(kind string, replicas int32) (runtime.Object, *v1.Pod) {
	metadata := metav1.ObjectMeta{Name: "workload", Namespace: "test", UID: "workload-uid"}
	selector := &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "app", Operator: metav1.LabelSelectorOpIn, Values: []string{"shared"}}}}
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "workload-pod", Namespace: metadata.Namespace, UID: "pod-uid",
			Labels:          map[string]string{"app": "shared", controllerRevisionHashLabelKey: "current-revision"},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: kind, Name: metadata.Name, UID: metadata.UID, Controller: ptr.To(true)}},
		},
		Spec: v1.PodSpec{SchedulerName: "volcano"}, Status: v1.PodStatus{Phase: v1.PodPending},
	}
	pod.Annotations = map[string]string{scheduling.KubeGroupNameAnnotationKey: helpers.GeneratePodgroupName(pod)}
	if kind == "ReplicaSet" {
		return &appsv1.ReplicaSet{ObjectMeta: metadata, Spec: appsv1.ReplicaSetSpec{Replicas: ptr.To(replicas), Selector: selector}}, pod
	}
	return &appsv1.StatefulSet{ObjectMeta: metadata, Spec: appsv1.StatefulSetSpec{Replicas: ptr.To(replicas), Selector: selector}, Status: appsv1.StatefulSetStatus{UpdateRevision: "current-revision"}}, pod
}

func storeRolloutWorkload(t *testing.T, c *pgcontroller, obj runtime.Object) {
	t.Helper()
	switch workload := obj.(type) {
	case *appsv1.ReplicaSet:
		_, err := c.kubeClient.AppsV1().ReplicaSets(workload.Namespace).Create(context.Background(), workload, metav1.CreateOptions{})
		require.NoError(t, err)
	case *appsv1.StatefulSet:
		_, err := c.kubeClient.AppsV1().StatefulSets(workload.Namespace).Create(context.Background(), workload, metav1.CreateOptions{})
		require.NoError(t, err)
	}
}

func enqueueRolloutWorkload(c *pgcontroller, obj runtime.Object) {
	switch workload := obj.(type) {
	case *appsv1.ReplicaSet:
		c.addReplicaSet(workload)
	case *appsv1.StatefulSet:
		c.addStatefulSet(workload)
	}
}

func TestSyncWorkload(t *testing.T) {
	tests := []struct {
		name         string
		replicas     int32
		podPhase     v1.PodPhase
		noPod        bool
		terminating  bool
		missingGroup bool
		customGroup  bool
		foreignOwner bool
		recreated    bool
		oldRevision  bool
		wantGroup    bool
		wantRequeues int
	}{
		{name: "delayed zero event uses current positive replicas", replicas: 1, wantGroup: true},
		{name: "annotated pod repairs missing group", replicas: 1, missingGroup: true, wantGroup: true},
		{name: "old revision can recover group before new revision has pods", replicas: 1, missingGroup: true, oldRevision: true, wantGroup: true},
		{name: "zero replicas preserves group for pending pod", wantGroup: true},
		{name: "zero replicas preserves group for running pod", podPhase: v1.PodRunning, wantGroup: true},
		{name: "zero replicas waits for terminating pod", terminating: true, wantGroup: true},
		{name: "zero replicas cleans group after pods disappear", noPod: true},
		{name: "zero replicas cleans group with succeeded pod", podPhase: v1.PodSucceeded},
		{name: "zero replicas cleans group with failed pod", podPhase: v1.PodFailed},
		{name: "recreated workload ignores old UID event", noPod: true, recreated: true, wantGroup: true},
		{name: "group owned by another workload is preserved", noPod: true, foreignOwner: true, wantGroup: true, wantRequeues: 1},
		{name: "external group is not taken over", replicas: 1, customGroup: true, missingGroup: true},
		{name: "positive replicas without pods does not create group", replicas: 1, noPod: true, missingGroup: true},
	}
	for _, kind := range []string{"ReplicaSet", "StatefulSet"} {
		t.Run(kind, func(t *testing.T) {
			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					ctx := context.Background()
					c := newFakeController()
					t.Cleanup(c.queue.ShutDown)
					t.Cleanup(c.workloadQueue.ShutDown)
					current, pod := rolloutObjects(kind, tt.replicas)
					// A historical zero-replica event can run after both workload
					// expansion and the new pod's annotation patch have completed.
					event, _ := rolloutObjects(kind, 0)
					if tt.recreated {
						current.(metav1.Object).SetUID("replacement-workload-uid")
					}
					storeRolloutWorkload(t, c, current)
					if tt.podPhase != "" {
						pod.Status.Phase = tt.podPhase
					}
					if tt.terminating {
						pod.DeletionTimestamp = ptr.To(metav1.Now())
					}
					if tt.oldRevision {
						pod.Labels[controllerRevisionHashLabelKey] = "old-revision"
					}
					if tt.customGroup {
						pod.Annotations[scheduling.KubeGroupNameAnnotationKey] = "external-group"
					}
					if !tt.noPod {
						_, err := c.kubeClient.CoreV1().Pods(pod.Namespace).Create(ctx, pod, metav1.CreateOptions{})
						require.NoError(t, err)
					}
					name := helpers.GeneratePodgroupName(pod)
					if !tt.missingGroup {
						group := c.buildPodGroupFromPod(pod, name).DeepCopy()
						group.UID, group.ResourceVersion = "group-uid", "1"
						if tt.foreignOwner {
							group.OwnerReferences[0].UID = "foreign-workload-uid"
						}
						_, err := c.vcClient.SchedulingV1beta1().PodGroups(pod.Namespace).Create(ctx, group, metav1.CreateOptions{})
						require.NoError(t, err)
					}
					enqueueRolloutWorkload(c, event)
					require.Equal(t, 1, c.workloadQueue.Len())
					require.True(t, c.processNextWorkload())
					req := workloadRequest{kind: kind, namespace: pod.Namespace, name: "workload", uid: "workload-uid"}
					assert.Equal(t, tt.wantRequeues, c.workloadQueue.NumRequeues(req))
					group, err := c.vcClient.SchedulingV1beta1().PodGroups(pod.Namespace).Get(ctx, name, metav1.GetOptions{})
					if tt.wantGroup {
						require.NoError(t, err)
						if !tt.foreignOwner {
							assert.Equal(t, pod.OwnerReferences[0].UID, metav1.GetControllerOf(group).UID)
						}
					} else {
						assert.True(t, apierrors.IsNotFound(err), "%v", err)
					}
				})
			}
		})
	}
}

func TestSyncWorkloadPodOwnership(t *testing.T) {
	for _, kind := range []string{"ReplicaSet", "StatefulSet"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			c := newFakeController()
			t.Cleanup(c.queue.ShutDown)
			t.Cleanup(c.workloadQueue.ShutDown)
			workload, pod := rolloutObjects(kind, 1)
			storeRolloutWorkload(t, c, workload)
			// Sort before the owned pod. Matching labels and revision do not
			// make another workload's annotated pod a member of this group.
			foreign := pod.DeepCopy()
			foreign.Name, foreign.UID = "a-foreign-pod", "foreign-pod-uid"
			foreign.OwnerReferences[0].UID = "foreign-workload-uid"
			foreign.Annotations[scheduling.KubeGroupNameAnnotationKey] = "external-group"
			otherNamespace := pod.DeepCopy()
			otherNamespace.Namespace, otherNamespace.UID = "other", "other-namespace-pod-uid"
			otherNamespace.Annotations = nil
			pod.Annotations = nil
			for _, member := range []*v1.Pod{foreign, otherNamespace, pod} {
				_, err := c.kubeClient.CoreV1().Pods(member.Namespace).Create(ctx, member, metav1.CreateOptions{})
				require.NoError(t, err)
			}
			enqueueRolloutWorkload(c, workload)
			require.True(t, c.processNextWorkload())
			for _, member := range []*v1.Pod{foreign, otherNamespace, pod} {
				stored, err := c.kubeClient.CoreV1().Pods(member.Namespace).Get(ctx, member.Name, metav1.GetOptions{})
				require.NoError(t, err)
				if member == pod {
					assert.Equal(t, helpers.GeneratePodgroupName(pod), stored.Annotations[scheduling.KubeGroupNameAnnotationKey])
				} else {
					assert.Equal(t, member.Annotations, stored.Annotations)
				}
			}
		})
	}
}

func TestDeletePodGroup(t *testing.T) {
	tests := []struct {
		name         string
		kind         string
		tombstone    bool
		replicas     int32
		missingOwner bool
		recreated    bool
		customGroup  bool
		wantGroup    bool
	}{
		{name: "replicaset group recovers", kind: "ReplicaSet", replicas: 1, wantGroup: true},
		{name: "statefulset tombstone recovers", kind: "StatefulSet", tombstone: true, replicas: 1, wantGroup: true},
		{name: "standalone pod group recovers", kind: "Pod", wantGroup: true},
		{name: "scaled down owner does not recreate group", kind: "ReplicaSet"},
		{name: "deleted owner does not recreate group", kind: "ReplicaSet", replicas: 1, missingOwner: true},
		{name: "recreated owner does not recreate old group", kind: "StatefulSet", replicas: 1, recreated: true},
		{name: "external group is ignored", kind: "ReplicaSet", replicas: 1, customGroup: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			c := newFakeController()
			t.Cleanup(c.queue.ShutDown)
			t.Cleanup(c.workloadQueue.ShutDown)
			workload, pod := rolloutObjects(tt.kind, tt.replicas)
			if tt.kind == "Pod" {
				pod.OwnerReferences = nil
				pod.Annotations[scheduling.KubeGroupNameAnnotationKey] = helpers.GeneratePodgroupName(pod)
			} else if !tt.missingOwner {
				if tt.recreated {
					workload.(metav1.Object).SetUID("replacement-workload-uid")
				}
				storeRolloutWorkload(t, c, workload)
			}
			_, err := c.kubeClient.CoreV1().Pods(pod.Namespace).Create(ctx, pod, metav1.CreateOptions{})
			require.NoError(t, err)
			require.NoError(t, c.podInformer.Informer().GetIndexer().Add(pod))
			name := helpers.GeneratePodgroupName(pod)
			group := c.buildPodGroupFromPod(pod, name).DeepCopy()
			if tt.customGroup {
				group.Name = "external-group"
			}
			var event interface{} = group
			if tt.tombstone {
				event = cache.DeletedFinalStateUnknown{Obj: group}
			}
			c.deletePodGroup(event)
			if !tt.customGroup {
				if tt.kind == "Pod" {
					require.Equal(t, 1, c.queue.Len())
					require.True(t, c.processNextReq())
				} else {
					require.Equal(t, 1, c.workloadQueue.Len())
					require.True(t, c.processNextWorkload())
				}
			} else {
				assert.Zero(t, c.workloadQueue.Len())
				assert.Zero(t, c.queue.Len())
			}
			_, err = c.vcClient.SchedulingV1beta1().PodGroups(pod.Namespace).Get(ctx, name, metav1.GetOptions{})
			if tt.wantGroup {
				assert.NoError(t, err)
			} else {
				assert.True(t, apierrors.IsNotFound(err), "%v", err)
			}
		})
	}
}

func TestProcessNextReqWithAnnotatedPod(t *testing.T) {
	tests := []struct {
		name          string
		annotation    string
		phase         v1.PodPhase
		terminating   bool
		existingGroup bool
		deletingGroup bool
		foreignGroup  bool
		wantGroup     bool
		wantRequeues  int
	}{
		{name: "missing automatic group is repaired", wantGroup: true},
		{name: "existing group absent from cache is accepted", existingGroup: true, wantGroup: true},
		{name: "external annotation is preserved", annotation: "external-group"},
		{name: "completed pod is ignored", phase: v1.PodSucceeded},
		{name: "terminating pod is ignored", terminating: true},
		{name: "deleting group is retried", existingGroup: true, deletingGroup: true, wantGroup: true, wantRequeues: 1},
		{name: "foreign group is not adopted", existingGroup: true, foreignGroup: true, wantGroup: true, wantRequeues: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			c := newFakeController()
			t.Cleanup(c.queue.ShutDown)
			t.Cleanup(c.workloadQueue.ShutDown)
			workload, pod := rolloutObjects("ReplicaSet", 1)
			storeRolloutWorkload(t, c, workload)
			if tt.annotation != "" {
				pod.Annotations[scheduling.KubeGroupNameAnnotationKey] = tt.annotation
			}
			if tt.phase != "" {
				pod.Status.Phase = tt.phase
			}
			if tt.terminating {
				pod.DeletionTimestamp = ptr.To(metav1.Now())
			}
			_, err := c.kubeClient.CoreV1().Pods(pod.Namespace).Create(ctx, pod, metav1.CreateOptions{})
			require.NoError(t, err)
			require.NoError(t, c.podInformer.Informer().GetIndexer().Add(pod))
			name := helpers.GeneratePodgroupName(pod)
			if tt.existingGroup {
				group := c.buildPodGroupFromPod(pod, name).DeepCopy()
				if tt.deletingGroup {
					group.DeletionTimestamp = ptr.To(metav1.Now())
				}
				if tt.foreignGroup {
					group.OwnerReferences[0].UID = types.UID("foreign-workload-uid")
				}
				_, err := c.vcClient.SchedulingV1beta1().PodGroups(pod.Namespace).Create(ctx, group, metav1.CreateOptions{})
				require.NoError(t, err)
			}
			c.addPod(pod)
			require.True(t, c.processNextReq())
			req := podRequest{podName: pod.Name, podNamespace: pod.Namespace, podUID: pod.UID}
			assert.Equal(t, tt.wantRequeues, c.queue.NumRequeues(req))
			_, err = c.vcClient.SchedulingV1beta1().PodGroups(pod.Namespace).Get(ctx, name, metav1.GetOptions{})
			if tt.wantGroup {
				assert.NoError(t, err)
			} else {
				assert.True(t, apierrors.IsNotFound(err), "%v", err)
			}
			stored, err := c.kubeClient.CoreV1().Pods(pod.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
			require.NoError(t, err)
			assert.Equal(t, pod.Annotations, stored.Annotations)
		})
	}
}

func TestPodEventsCleanUpScaledDownWorkload(t *testing.T) {
	for _, event := range []string{"delete", "completed", "delete after scheduler changes"} {
		t.Run(event, func(t *testing.T) {
			ctx := context.Background()
			c := newFakeController()
			t.Cleanup(c.queue.ShutDown)
			t.Cleanup(c.workloadQueue.ShutDown)
			workload, pod := rolloutObjects("ReplicaSet", 0)
			if event == "delete after scheduler changes" {
				pod.Spec.SchedulerName = "default-scheduler"
			}
			storeRolloutWorkload(t, c, workload)
			_, err := c.kubeClient.CoreV1().Pods(pod.Namespace).Create(ctx, pod, metav1.CreateOptions{})
			require.NoError(t, err)
			name := helpers.GeneratePodgroupName(pod)
			_, err = c.vcClient.SchedulingV1beta1().PodGroups(pod.Namespace).Create(ctx, c.buildPodGroupFromPod(pod, name), metav1.CreateOptions{})
			require.NoError(t, err)
			enqueueRolloutWorkload(c, workload)
			require.True(t, c.processNextWorkload())
			_, err = c.vcClient.SchedulingV1beta1().PodGroups(pod.Namespace).Get(ctx, name, metav1.GetOptions{})
			require.NoError(t, err)
			if event != "completed" {
				require.NoError(t, c.kubeClient.CoreV1().Pods(pod.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{}))
				c.deletePod(cache.DeletedFinalStateUnknown{Obj: pod})
			} else {
				completed := pod.DeepCopy()
				completed.Status.Phase = v1.PodSucceeded
				_, err = c.kubeClient.CoreV1().Pods(pod.Namespace).UpdateStatus(ctx, completed, metav1.UpdateOptions{})
				require.NoError(t, err)
				c.updatePod(pod, completed)
			}
			require.Equal(t, 1, c.workloadQueue.Len())
			require.True(t, c.processNextWorkload())
			_, err = c.vcClient.SchedulingV1beta1().PodGroups(pod.Namespace).Get(ctx, name, metav1.GetOptions{})
			assert.True(t, apierrors.IsNotFound(err), "%v", err)
		})
	}
}

func TestWorkloadPodGroupDeletionRace(t *testing.T) {
	for _, replaceGroup := range []bool{false, true} {
		t.Run(fmt.Sprintf("group_replaced=%v", replaceGroup), func(t *testing.T) {
			ctx := context.Background()
			c := newFakeController()
			t.Cleanup(c.queue.ShutDown)
			t.Cleanup(c.workloadQueue.ShutDown)
			obj, pod := rolloutObjects("ReplicaSet", 0)
			rs := obj.(*appsv1.ReplicaSet)
			storeRolloutWorkload(t, c, rs)
			name := helpers.GeneratePodgroupName(pod)
			group := c.buildPodGroupFromPod(pod, name)
			group.UID, group.ResourceVersion = "old-group-uid", "1"
			_, err := c.vcClient.SchedulingV1beta1().PodGroups(rs.Namespace).Create(ctx, group, metav1.CreateOptions{})
			require.NoError(t, err)
			client := c.vcClient.(*vcclient.Clientset)
			client.PrependReactor("delete", "podgroups", func(action k8stesting.Action) (bool, runtime.Object, error) {
				// Expansion occurs after the worker checked replicas and pods,
				// but before the API server executes its delete request.
				rs.Spec.Replicas = ptr.To[int32](1)
				if _, err := c.kubeClient.AppsV1().ReplicaSets(rs.Namespace).Update(ctx, rs, metav1.UpdateOptions{}); err != nil {
					return true, nil, err
				}
				if _, err := c.kubeClient.CoreV1().Pods(pod.Namespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
					return true, nil, err
				}
				options := action.(k8stesting.DeleteAction).GetDeleteOptions()
				assert.Equal(t, &metav1.Preconditions{UID: &group.UID, ResourceVersion: &group.ResourceVersion}, options.Preconditions)
				if replaceGroup {
					replacement := group.DeepCopy()
					replacement.UID, replacement.ResourceVersion = "new-group-uid", "2"
					if err := client.Tracker().Update(action.GetResource(), replacement, rs.Namespace); err != nil {
						return true, nil, err
					}
					// Fake clientsets do not enforce delete preconditions.
					return true, nil, apierrors.NewConflict(schema.GroupResource{Group: scheduling.GroupName, Resource: "podgroups"}, name, fmt.Errorf("UID precondition failed"))
				}
				return false, nil, nil
			})
			c.addReplicaSet(rs)
			require.True(t, c.processNextWorkload())
			if replaceGroup {
				req := workloadRequest{kind: "ReplicaSet", namespace: rs.Namespace, name: rs.Name, uid: rs.UID}
				require.Equal(t, 1, c.workloadQueue.NumRequeues(req))
				// The retry observes the now-active owner and preserves the
				// replacement instead of repeating the obsolete cleanup.
				require.True(t, c.processNextWorkload())
				assert.Zero(t, c.workloadQueue.NumRequeues(req))
			} else {
				_, err := c.vcClient.SchedulingV1beta1().PodGroups(rs.Namespace).Get(ctx, name, metav1.GetOptions{})
				require.True(t, apierrors.IsNotFound(err))
				c.deletePodGroup(group)
				require.True(t, c.processNextWorkload())
			}
			restored, err := c.vcClient.SchedulingV1beta1().PodGroups(rs.Namespace).Get(ctx, name, metav1.GetOptions{})
			require.NoError(t, err)
			assert.Equal(t, rs.UID, metav1.GetControllerOf(restored).UID)
			if replaceGroup {
				assert.Equal(t, types.UID("new-group-uid"), restored.UID)
			}
		})
	}
}

func TestSyncWorkloadRetriesReadErrors(t *testing.T) {
	for _, resource := range []string{"replicasets", "pods"} {
		t.Run(resource, func(t *testing.T) {
			c := newFakeController()
			t.Cleanup(c.queue.ShutDown)
			t.Cleanup(c.workloadQueue.ShutDown)
			workload, _ := rolloutObjects("ReplicaSet", 0)
			storeRolloutWorkload(t, c, workload)
			client := c.kubeClient.(*kubeclient.Clientset)
			client.PrependReactor("*", resource, func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, fmt.Errorf("API temporarily unavailable")
			})
			enqueueRolloutWorkload(c, workload)
			require.True(t, c.processNextWorkload())
			req := workloadRequest{kind: "ReplicaSet", namespace: "test", name: "workload", uid: "workload-uid"}
			assert.Equal(t, 1, c.workloadQueue.NumRequeues(req))
		})
	}
}

func TestRunRepairsDeletedPodGroup(t *testing.T) {
	// Exercise informer registration and both real workers with fake watch
	// streams. The deadline bounds asynchronous informer delivery, not retries
	// of a deterministic assertion.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	c := newFakeController()
	workload, pod := rolloutObjects("ReplicaSet", 1)
	storeRolloutWorkload(t, c, workload)
	pod.Annotations = nil
	_, err := c.kubeClient.CoreV1().Pods(pod.Namespace).Create(ctx, pod, metav1.CreateOptions{})
	require.NoError(t, err)
	done := make(chan struct{})
	t.Cleanup(func() {
		cancel()
		<-done
		c.informerFactory.Shutdown()
		c.vcInformerFactory.Shutdown()
	})
	go func() {
		defer close(done)
		c.Run(ctx.Done())
	}()
	name := helpers.GeneratePodgroupName(pod)
	ready := func() bool {
		_, groupErr := c.vcClient.SchedulingV1beta1().PodGroups(pod.Namespace).Get(ctx, name, metav1.GetOptions{})
		stored, podErr := c.kubeClient.CoreV1().Pods(pod.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
		return groupErr == nil && podErr == nil && stored.Annotations[scheduling.KubeGroupNameAnnotationKey] == name
	}
	require.Eventually(t, ready, 5*time.Second, 10*time.Millisecond)
	require.NoError(t, c.vcClient.SchedulingV1beta1().PodGroups(pod.Namespace).Delete(ctx, name, metav1.DeleteOptions{}))
	require.Eventually(t, ready, 5*time.Second, 10*time.Millisecond)
}
