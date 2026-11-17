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
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubeclient "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	testingclock "k8s.io/utils/clock/testing"
	"k8s.io/utils/ptr"

	"volcano.sh/apis/pkg/apis/helpers"
	scheduling "volcano.sh/apis/pkg/apis/scheduling/v1beta1"
	vcclient "volcano.sh/apis/pkg/client/clientset/versioned/fake"
	"volcano.sh/volcano/pkg/scheduler/api"
)

func recoveryObjects(kind string, replicas int32) (runtime.Object, *v1.Pod, *scheduling.PodGroup) {
	meta := metav1.ObjectMeta{Name: "workload", Namespace: "test", UID: "workload-uid",
		Annotations: map[string]string{scheduling.VolcanoGroupMinMemberAnnotationKey: "3"}}
	selector := &metav1.LabelSelector{MatchLabels: map[string]string{"app": "shared"}}
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "workload-pod", Namespace: meta.Namespace, UID: "pod-uid",
			Labels:          map[string]string{"app": "shared", controllerRevisionHashLabelKey: "revision"},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: kind, Name: meta.Name, UID: meta.UID, Controller: ptr.To(true)}}},
		Spec: v1.PodSpec{SchedulerName: "volcano", Containers: []v1.Container{{Name: "container", Image: "nginx",
			Resources: v1.ResourceRequirements{Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("100m")}}}}},
		Status: v1.PodStatus{Phase: v1.PodPending},
	}
	name := helpers.GeneratePodgroupName(pod)
	pod.Annotations = map[string]string{scheduling.KubeGroupNameAnnotationKey: name}
	group := &scheduling.PodGroup{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: meta.Namespace, UID: "group-uid", ResourceVersion: "1", OwnerReferences: newPGOwnerReferences(pod)},
		Spec: scheduling.PodGroupSpec{MinMember: 3, Queue: "namespace-queue"}}
	if kind == "ReplicaSet" {
		return &appsv1.ReplicaSet{ObjectMeta: meta, Spec: appsv1.ReplicaSetSpec{Replicas: ptr.To(replicas), Selector: selector}}, pod, group
	}
	return &appsv1.StatefulSet{ObjectMeta: meta, Spec: appsv1.StatefulSetSpec{Replicas: ptr.To(replicas), Selector: selector}, Status: appsv1.StatefulSetStatus{UpdateRevision: "revision"}}, pod, group
}

func storeRecoveryOwner(t *testing.T, c *pgcontroller, obj runtime.Object) {
	t.Helper()
	var err error
	switch owner := obj.(type) {
	case *appsv1.ReplicaSet:
		_, err = c.kubeClient.AppsV1().ReplicaSets(owner.Namespace).Create(context.Background(), owner, metav1.CreateOptions{})
	case *appsv1.StatefulSet:
		_, err = c.kubeClient.AppsV1().StatefulSets(owner.Namespace).Create(context.Background(), owner, metav1.CreateOptions{})
	}
	require.NoError(t, err)
}

func TestPodGroupCleanupDuringRollout(t *testing.T) {
	tests := []struct {
		name        string
		replicas    int32
		noPod       bool
		terminating bool
		completed   bool
		foreignPod  bool
		recreated   bool
		readError   string
		wantGroup   bool
	}{
		{name: "stale zero event after expansion", replicas: 1, wantGroup: true},
		{name: "positive desired replicas without pods", replicas: 1, noPod: true, wantGroup: true},
		{name: "zero replicas with pending pod", wantGroup: true},
		{name: "zero replicas with terminating pod", terminating: true, wantGroup: true},
		{name: "zero replicas without pods", noPod: true},
		{name: "zero replicas with completed pod", completed: true},
		{name: "same labels from another workload do not prevent cleanup", foreignPod: true},
		{name: "old UID event after workload recreation", recreated: true, noPod: true, wantGroup: true},
		{name: "failed current owner read prevents cleanup", noPod: true, readError: "owner", wantGroup: true},
		{name: "failed pod list prevents cleanup", noPod: true, readError: "pods", wantGroup: true},
	}
	for _, kind := range []string{"ReplicaSet", "StatefulSet"} {
		t.Run(kind, func(t *testing.T) {
			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					ctx := context.Background()
					c := newFakeController()
					t.Cleanup(c.queue.ShutDown)
					current, pod, group := recoveryObjects(kind, tt.replicas)
					event, _, _ := recoveryObjects(kind, 0)
					if tt.recreated {
						current.(metav1.Object).SetUID("replacement-uid")
					}
					storeRecoveryOwner(t, c, current)
					if tt.terminating {
						pod.DeletionTimestamp = ptr.To(metav1.Now())
					}
					if tt.completed {
						pod.Status.Phase = v1.PodSucceeded
					}
					if tt.foreignPod {
						pod.OwnerReferences = append([]metav1.OwnerReference(nil), pod.OwnerReferences...)
						pod.OwnerReferences[0].UID = "foreign-uid"
					}
					if !tt.noPod {
						_, err := c.kubeClient.CoreV1().Pods(pod.Namespace).Create(ctx, pod, metav1.CreateOptions{})
						require.NoError(t, err)
					}
					_, err := c.vcClient.SchedulingV1beta1().PodGroups(group.Namespace).Create(ctx, group, metav1.CreateOptions{})
					require.NoError(t, err)
					client := c.vcClient.(*vcclient.Clientset)
					client.PrependReactor("delete", "podgroups", func(action k8stesting.Action) (bool, runtime.Object, error) {
						options := action.(k8stesting.DeleteAction).GetDeleteOptions()
						assert.Equal(t, &metav1.Preconditions{UID: &group.UID, ResourceVersion: &group.ResourceVersion}, options.Preconditions)
						return false, nil, nil
					})
					if tt.readError != "" {
						verb, res := "get", strings.ToLower(kind)+"s"
						if tt.readError == "pods" {
							verb, res = "list", "pods"
						}
						c.kubeClient.(*kubeclient.Clientset).PrependReactor(verb, res, func(k8stesting.Action) (bool, runtime.Object, error) {
							return true, nil, apierrors.NewServiceUnavailable("transient read failure")
						})
					}
					switch owner := event.(type) {
					case *appsv1.ReplicaSet:
						c.addReplicaSet(owner)
					case *appsv1.StatefulSet:
						c.addStatefulSet(owner)
					}
					require.Equal(t, 1, c.queue.Len())
					require.True(t, c.processNextReq())
					if tt.readError != "" {
						req := podRequest{cleanup: cleanupRequest{kind: kind, namespace: group.Namespace, name: "workload", uid: "workload-uid"}}
						assert.Equal(t, 1, c.queue.NumRequeues(req))
					}
					stored, err := client.SchedulingV1beta1().PodGroups(group.Namespace).Get(ctx, group.Name, metav1.GetOptions{})
					if tt.wantGroup {
						require.NoError(t, err)
						assert.Equal(t, group, stored)
					} else {
						assert.True(t, apierrors.IsNotFound(err), "%v", err)
					}
				})
			}
		})
	}
}

func TestPodGroupCleanupRetries(t *testing.T) {
	tests := []struct {
		name         string
		stillPending bool
		podCompleted bool
		ownerChange  string
		retryFailure string
		wantGroup    bool
	}{
		{name: "last terminating pod disappears without an owner event"},
		{name: "terminating pod survives the first retry", stillPending: true},
		{name: "member pod completes without an owner event", podCompleted: true},
		{name: "expansion cancels pending cleanup", ownerChange: "expand", wantGroup: true},
		{name: "recreated owner cancels old UID cleanup", ownerChange: "replace", wantGroup: true},
		{name: "deleted owner leaves deletion to garbage collection", ownerChange: "delete", wantGroup: true},
		{name: "transient owner read is retried", retryFailure: "owner"},
		{name: "transient pod list is retried", retryFailure: "pods"},
		{name: "transient group read is retried", retryFailure: "group"},
		{name: "delete conflict is retried", retryFailure: "delete"},
	}
	for _, kind := range []string{"ReplicaSet", "StatefulSet"} {
		t.Run(kind, func(t *testing.T) {
			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					ctx := context.Background()
					c := newFakeController()
					// Substitute a real queue with a fake clock at the queue
					// dependency boundary; cleanup delays need no wall-clock sleep.
					c.queue.ShutDown()
					clock := testingclock.NewFakeClock(time.Now())
					c.queue = workqueue.NewTypedRateLimitingQueueWithConfig(workqueue.DefaultTypedControllerRateLimiter[podRequest](), workqueue.TypedRateLimitingQueueConfig[podRequest]{Clock: clock})
					t.Cleanup(c.queue.ShutDown)
					owner, pod, group := recoveryObjects(kind, 0)
					storeRecoveryOwner(t, c, owner)
					pod.Status.Phase = v1.PodRunning
					pod.DeletionTimestamp = ptr.To(metav1.Now())
					_, err := c.kubeClient.CoreV1().Pods(pod.Namespace).Create(ctx, pod, metav1.CreateOptions{})
					require.NoError(t, err)
					_, err = c.vcClient.SchedulingV1beta1().PodGroups(group.Namespace).Create(ctx, group, metav1.CreateOptions{})
					require.NoError(t, err)
					switch obj := owner.(type) {
					case *appsv1.ReplicaSet:
						require.Nil(t, obj.Status.TerminatingReplicas)
						c.addReplicaSet(obj)
					case *appsv1.StatefulSet:
						c.addStatefulSet(obj)
					}
					require.True(t, c.processNextReq())
					req := podRequest{cleanup: cleanupRequest{kind: kind, namespace: group.Namespace, name: "workload", uid: "workload-uid"}}
					assert.Zero(t, c.queue.NumRequeues(req), "waiting for a member is not an API error")
					if tt.stillPending {
						clock.Step(podGroupCleanupRetryInterval)
						require.Eventually(t, func() bool { return c.queue.Len() == 1 }, 5*time.Second, time.Millisecond)
						require.True(t, c.processNextReq())
						_, err := c.vcClient.SchedulingV1beta1().PodGroups(group.Namespace).Get(ctx, group.Name, metav1.GetOptions{})
						require.NoError(t, err)
					}
					// No Pod callback or workload callback is delivered from here
					// onward: only the already-scheduled cleanup can make progress.
					if tt.podCompleted {
						pod.Status.Phase = v1.PodSucceeded
						_, err = c.kubeClient.CoreV1().Pods(pod.Namespace).UpdateStatus(ctx, pod, metav1.UpdateOptions{})
						require.NoError(t, err)
					} else {
						require.NoError(t, c.kubeClient.CoreV1().Pods(pod.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{}))
					}
					if tt.ownerChange != "" {
						switch obj := owner.(type) {
						case *appsv1.ReplicaSet:
							if tt.ownerChange == "expand" {
								obj.Spec.Replicas = ptr.To[int32](1)
								_, err = c.kubeClient.AppsV1().ReplicaSets(obj.Namespace).Update(ctx, obj, metav1.UpdateOptions{})
							} else {
								err = c.kubeClient.AppsV1().ReplicaSets(obj.Namespace).Delete(ctx, obj.Name, metav1.DeleteOptions{})
							}
						case *appsv1.StatefulSet:
							if tt.ownerChange == "expand" {
								obj.Spec.Replicas = ptr.To[int32](1)
								_, err = c.kubeClient.AppsV1().StatefulSets(obj.Namespace).Update(ctx, obj, metav1.UpdateOptions{})
							} else {
								err = c.kubeClient.AppsV1().StatefulSets(obj.Namespace).Delete(ctx, obj.Name, metav1.DeleteOptions{})
							}
						}
						require.NoError(t, err)
						if tt.ownerChange == "replace" {
							owner.(metav1.Object).SetUID("replacement-uid")
							storeRecoveryOwner(t, c, owner)
						}
					}
					if tt.retryFailure != "" {
						failed := false
						failOnce := func(k8stesting.Action) (bool, runtime.Object, error) {
							if failed {
								return false, nil, nil
							}
							failed = true
							return true, nil, apierrors.NewServiceUnavailable("transient cleanup failure")
						}
						switch tt.retryFailure {
						case "owner":
							c.kubeClient.(*kubeclient.Clientset).PrependReactor("get", strings.ToLower(kind)+"s", failOnce)
						case "pods":
							c.kubeClient.(*kubeclient.Clientset).PrependReactor("list", "pods", failOnce)
						case "group":
							c.vcClient.(*vcclient.Clientset).PrependReactor("get", "podgroups", failOnce)
						case "delete":
							c.vcClient.(*vcclient.Clientset).PrependReactor("delete", "podgroups", func(k8stesting.Action) (bool, runtime.Object, error) {
								if failed {
									return false, nil, nil
								}
								failed = true
								return true, nil, apierrors.NewConflict(scheduling.Resource("podgroups"), group.Name, nil)
							})
						}
					}
					clock.Step(podGroupCleanupRetryInterval)
					require.Eventually(t, func() bool { return c.queue.Len() == 1 }, 5*time.Second, time.Millisecond)
					require.True(t, c.processNextReq())
					if tt.retryFailure != "" {
						require.Equal(t, 1, c.queue.NumRequeues(req))
						_, err := c.vcClient.SchedulingV1beta1().PodGroups(group.Namespace).Get(ctx, group.Name, metav1.GetOptions{})
						require.NoError(t, err)
						clock.Step(podGroupCleanupRetryInterval)
						require.Eventually(t, func() bool { return c.queue.Len() == 1 }, 5*time.Second, time.Millisecond)
						require.True(t, c.processNextReq())
					}
					assert.Zero(t, c.queue.NumRequeues(req))
					stored, err := c.vcClient.SchedulingV1beta1().PodGroups(group.Namespace).Get(ctx, group.Name, metav1.GetOptions{})
					if tt.wantGroup {
						require.NoError(t, err)
						assert.Equal(t, group, stored)
					} else {
						assert.True(t, apierrors.IsNotFound(err), "%v", err)
					}
					assert.Zero(t, c.queue.Len())
				})
			}
		})
	}
}

func TestPodGroupCleanupWithoutGroup(t *testing.T) {
	ctx := context.Background()
	c := newFakeController()
	t.Cleanup(c.queue.ShutDown)
	obj, pod, _ := recoveryObjects("ReplicaSet", 0)
	storeRecoveryOwner(t, c, obj)
	pod.DeletionTimestamp = ptr.To(metav1.Now())
	_, err := c.kubeClient.CoreV1().Pods(pod.Namespace).Create(ctx, pod, metav1.CreateOptions{})
	require.NoError(t, err)
	// A workload without a managed group has nothing to clean up, even if
	// one of its pods remains terminating. It must not keep polling forever.
	pending, err := c.cleanupPodGroup(cleanupRequest{kind: "ReplicaSet", namespace: pod.Namespace, name: "workload", uid: "workload-uid"})
	require.NoError(t, err)
	assert.False(t, pending)
}

func TestDeletePodGroupRecovery(t *testing.T) {
	tests := []struct {
		name        string
		kind        string
		customOwner bool
		staleCache  bool
		tombstone   bool
		customGroup bool
		terminating bool
	}{
		{name: "replicaset", kind: "ReplicaSet"},
		{name: "statefulset tombstone", kind: "StatefulSet", tombstone: true},
		{name: "pod cache has not observed annotation patch", kind: "ReplicaSet", staleCache: true},
		{name: "same kind custom statefulset", kind: "StatefulSet", customOwner: true},
		{name: "same kind custom job", kind: "Job", customOwner: true},
		{name: "explicit external group is preserved", kind: "ReplicaSet", customGroup: true},
		{name: "terminating pod does not revive group", kind: "ReplicaSet", terminating: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			c := newFakeController()
			t.Cleanup(c.queue.ShutDown)
			obj, pod, deleted := recoveryObjects(tt.kind, 1)
			if tt.customOwner {
				pod.OwnerReferences[0].APIVersion = "workloads.example.com/v1"
			} else {
				storeRecoveryOwner(t, c, obj)
			}
			if tt.customGroup {
				pod.Annotations[scheduling.KubeGroupNameAnnotationKey] = "external-group"
			}
			if tt.terminating {
				pod.DeletionTimestamp = ptr.To(metav1.Now())
			}
			_, err := c.kubeClient.CoreV1().Pods(pod.Namespace).Create(ctx, pod, metav1.CreateOptions{})
			require.NoError(t, err)
			cached := pod.DeepCopy()
			if tt.staleCache {
				cached.Annotations = nil
			}
			require.NoError(t, c.podInformer.Informer().GetIndexer().Add(cached))
			// Fake clients do not run CREATE admission. Emulate the resolved
			// namespace queue returned by admission, without any UPDATE reactor.
			client := c.vcClient.(*vcclient.Clientset)
			client.PrependReactor("create", "podgroups", func(action k8stesting.Action) (bool, runtime.Object, error) {
				group := action.(k8stesting.CreateAction).GetObject().(*scheduling.PodGroup).DeepCopy()
				group.Spec.Queue = "namespace-queue"
				if err := client.Tracker().Create(action.GetResource(), group, group.Namespace); err != nil {
					return true, nil, err
				}
				return true, group, nil
			})
			var event interface{} = deleted
			if tt.tombstone {
				event = cache.DeletedFinalStateUnknown{Obj: deleted}
			}
			c.deletePodGroup(event)
			if tt.customGroup {
				assert.Zero(t, c.queue.Len())
			} else {
				require.Equal(t, 1, c.queue.Len())
				require.True(t, c.processNextReq())
			}
			group, err := client.SchedulingV1beta1().PodGroups(deleted.Namespace).Get(ctx, deleted.Name, metav1.GetOptions{})
			if tt.customGroup || tt.terminating {
				assert.True(t, apierrors.IsNotFound(err), "%v", err)
				return
			}
			require.NoError(t, err)
			minMember := int32(3)
			if tt.customOwner {
				minMember = 1
			}
			assert.Equal(t, minMember, group.Spec.MinMember)
			assert.EqualValues(t, minMember*100, group.Spec.MinResources.Cpu().MilliValue())
			assert.Equal(t, "namespace-queue", group.Spec.Queue)
			stored, err := c.kubeClient.CoreV1().Pods(pod.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
			require.NoError(t, err)
			assert.Equal(t, deleted.Name, stored.Annotations[scheduling.KubeGroupNameAnnotationKey])
			for _, action := range client.Actions() {
				assert.NotEqual(t, "update", action.GetVerb(), "recovery must only create, never rewrite the admitted group")
			}
		})
	}
}

func TestPodGroupRecoveryChecksLivePod(t *testing.T) {
	tests := []struct {
		name      string
		state     string
		readError bool
		wantGroup bool
	}{
		{name: "deleted pod", state: "missing"},
		{name: "same name with a new UID", state: "recreated"},
		{name: "pod is now terminating", state: "terminating"},
		{name: "pod is now completed", state: "completed"},
		{name: "pod has chosen an external group", state: "external"},
		{name: "pod owner has changed", state: "owner"},
		{name: "current pod requirements are used", wantGroup: true},
		{name: "transient pod read is retried", readError: true, wantGroup: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			c := newFakeController()
			t.Cleanup(c.queue.ShutDown)
			owner, cached, deleted := recoveryObjects("ReplicaSet", 1)
			storeRecoveryOwner(t, c, owner)
			current := cached.DeepCopy()
			current.Spec.Containers[0].Resources.Requests[v1.ResourceCPU] = resource.MustParse("200m")
			current.Annotations[scheduling.QueueNameAnnotationKey] = "live-pod-queue"
			switch tt.state {
			case "recreated":
				current.UID = "new-pod-uid"
			case "terminating":
				current.DeletionTimestamp = ptr.To(metav1.Now())
			case "completed":
				current.Status.Phase = v1.PodSucceeded
			case "external":
				current.Annotations[scheduling.KubeGroupNameAnnotationKey] = "external-group"
			case "owner":
				current.OwnerReferences[0].UID = "different-owner-uid"
				current.OwnerReferences[0].Name = "new-workload"
				newOwner := owner.DeepCopyObject()
				newOwner.(metav1.Object).SetName("new-workload")
				newOwner.(metav1.Object).SetUID("different-owner-uid")
				storeRecoveryOwner(t, c, newOwner)
			}
			if tt.state != "missing" {
				_, err := c.kubeClient.CoreV1().Pods(current.Namespace).Create(ctx, current, metav1.CreateOptions{})
				require.NoError(t, err)
			}
			require.NoError(t, c.podInformer.Informer().GetIndexer().Add(cached.DeepCopy()))
			if tt.readError {
				failed := false
				c.kubeClient.(*kubeclient.Clientset).PrependReactor("get", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
					if !failed {
						failed = true
						return true, nil, apierrors.NewServiceUnavailable("transient pod read failure")
					}
					return false, nil, nil
				})
			}
			c.deletePodGroup(deleted)
			require.True(t, c.processNextReq())
			req := podRequest{podName: cached.Name, podNamespace: cached.Namespace, podUID: cached.UID}
			if tt.readError {
				require.Equal(t, 1, c.queue.NumRequeues(req))
				_, err := c.vcClient.SchedulingV1beta1().PodGroups(deleted.Namespace).Get(ctx, deleted.Name, metav1.GetOptions{})
				require.True(t, apierrors.IsNotFound(err))
				require.True(t, c.processNextReq())
			}
			assert.Zero(t, c.queue.NumRequeues(req))
			groups, err := c.vcClient.SchedulingV1beta1().PodGroups(deleted.Namespace).List(ctx, metav1.ListOptions{})
			require.NoError(t, err)
			if tt.wantGroup {
				require.Len(t, groups.Items, 1)
				assert.Equal(t, "live-pod-queue", groups.Items[0].Spec.Queue)
				assert.EqualValues(t, 600, groups.Items[0].Spec.MinResources.Cpu().MilliValue())
			} else {
				assert.Empty(t, groups.Items)
			}
		})
	}
}

func TestPodGroupCleanupWithStalePodCache(t *testing.T) {
	for _, kind := range []string{"ReplicaSet", "StatefulSet"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			c := newFakeController()
			t.Cleanup(c.queue.ShutDown)
			owner, pod, group := recoveryObjects(kind, 0)
			storeRecoveryOwner(t, c, owner)
			_, err := c.kubeClient.CoreV1().Pods(pod.Namespace).Create(ctx, pod, metav1.CreateOptions{})
			require.NoError(t, err)
			require.NoError(t, c.podInformer.Informer().GetIndexer().Add(pod.DeepCopy()))
			_, err = c.vcClient.SchedulingV1beta1().PodGroups(group.Namespace).Create(ctx, group, metav1.CreateOptions{})
			require.NoError(t, err)
			require.NoError(t, c.pgInformer.Informer().GetIndexer().Add(group.DeepCopy()))
			require.NoError(t, c.kubeClient.CoreV1().Pods(pod.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{}))
			switch obj := owner.(type) {
			case *appsv1.ReplicaSet:
				c.addReplicaSet(obj)
			case *appsv1.StatefulSet:
				c.addStatefulSet(obj)
			}
			require.True(t, c.processNextReq())
			_, err = c.vcClient.SchedulingV1beta1().PodGroups(group.Namespace).Get(ctx, group.Name, metav1.GetOptions{})
			require.True(t, apierrors.IsNotFound(err))
			// The group deletion arrives before the Pod deletion in its informer.
			require.NoError(t, c.pgInformer.Informer().GetIndexer().Delete(group))
			c.deletePodGroup(group)
			require.Equal(t, 1, c.queue.Len())
			require.True(t, c.processNextReq())
			_, err = c.vcClient.SchedulingV1beta1().PodGroups(group.Namespace).Get(ctx, group.Name, metav1.GetOptions{})
			assert.True(t, apierrors.IsNotFound(err), "an absent Pod must not resurrect the cleaned group")
			assert.Zero(t, c.queue.Len())
		})
	}
}

func TestPodGroupRecoveryCleansUpConcurrentPodDeletion(t *testing.T) {
	for _, kind := range []string{"ReplicaSet", "StatefulSet"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			c := newFakeController()
			c.queue.ShutDown()
			clock := testingclock.NewFakeClock(time.Now())
			c.queue = workqueue.NewTypedRateLimitingQueueWithConfig(workqueue.DefaultTypedControllerRateLimiter[podRequest](), workqueue.TypedRateLimitingQueueConfig[podRequest]{Clock: clock})
			t.Cleanup(c.queue.ShutDown)
			owner, pod, deleted := recoveryObjects(kind, 0)
			storeRecoveryOwner(t, c, owner)
			_, err := c.kubeClient.CoreV1().Pods(pod.Namespace).Create(ctx, pod, metav1.CreateOptions{})
			require.NoError(t, err)
			require.NoError(t, c.podInformer.Informer().GetIndexer().Add(pod.DeepCopy()))
			// A previous cleanup has already finished because the group was
			// absent. It cannot clean a group created later without a new request.
			switch obj := owner.(type) {
			case *appsv1.ReplicaSet:
				c.addReplicaSet(obj)
			case *appsv1.StatefulSet:
				c.addStatefulSet(obj)
			}
			require.True(t, c.processNextReq())
			creates := 0
			c.vcClient.(*vcclient.Clientset).PrependReactor("create", "podgroups", func(k8stesting.Action) (bool, runtime.Object, error) {
				creates++
				// The live Pod GET succeeded, but the Pod disappears before CREATE.
				if err := c.kubeClient.CoreV1().Pods(pod.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{}); err != nil {
					return true, nil, err
				}
				return false, nil, nil
			})
			c.deletePodGroup(deleted)
			require.True(t, c.processNextReq())
			created, err := c.vcClient.SchedulingV1beta1().PodGroups(deleted.Namespace).Get(ctx, deleted.Name, metav1.GetOptions{})
			require.NoError(t, err)
			clock.Step(podGroupCleanupRetryInterval)
			require.Eventually(t, func() bool { return c.queue.Len() == 1 }, 5*time.Second, time.Millisecond)
			require.True(t, c.processNextReq())
			_, err = c.vcClient.SchedulingV1beta1().PodGroups(deleted.Namespace).Get(ctx, deleted.Name, metav1.GetOptions{})
			require.True(t, apierrors.IsNotFound(err))
			// Even a second deletion callback must not revive the group again.
			c.deletePodGroup(created)
			require.True(t, c.processNextReq())
			_, err = c.vcClient.SchedulingV1beta1().PodGroups(deleted.Namespace).Get(ctx, deleted.Name, metav1.GetOptions{})
			assert.True(t, apierrors.IsNotFound(err))
			assert.Equal(t, 1, creates)
			assert.Zero(t, c.queue.Len())
		})
	}
}

func TestPodGroupRecoveryRetainsUIDPatchGuard(t *testing.T) {
	ctx := context.Background()
	c := newRecreatedPodController()
	t.Cleanup(c.queue.ShutDown)
	pod := stsPod("old-pod", "owner-uid")
	storeStatefulSetForPod(t, c, pod)
	_, err := c.kubeClient.CoreV1().Pods(pod.Namespace).Create(ctx, pod, metav1.CreateOptions{})
	require.NoError(t, err)
	require.NoError(t, c.podInformer.Informer().GetIndexer().Add(pod.DeepCopy()))
	c.vcClient.(*vcclient.Clientset).PrependReactor("create", "podgroups", func(k8stesting.Action) (bool, runtime.Object, error) {
		// Replace the Pod after the live GET, before its annotation is patched.
		if err := c.kubeClient.CoreV1().Pods(pod.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{}); err != nil {
			return true, nil, err
		}
		replacement := pod.DeepCopy()
		replacement.UID = "new-pod"
		if _, err := c.kubeClient.CoreV1().Pods(pod.Namespace).Create(ctx, replacement, metav1.CreateOptions{}); err != nil {
			return true, nil, err
		}
		return false, nil, nil
	})
	c.addPod(pod)
	require.True(t, c.processNextReq())
	req := podRequest{podName: pod.Name, podNamespace: pod.Namespace, podUID: pod.UID}
	assert.Equal(t, 1, c.queue.NumRequeues(req), "the API must reject a patch carrying the old UID")
	current, err := c.kubeClient.CoreV1().Pods(pod.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
	require.NoError(t, err)
	assert.EqualValues(t, "new-pod", current.UID)
	assert.Empty(t, current.Annotations[scheduling.KubeGroupNameAnnotationKey])
}

func TestExistingPodGroupIsNotReconciled(t *testing.T) {
	for _, nilMetadata := range []bool{false, true} {
		t.Run(map[bool]string{false: "external metadata", true: "nil metadata"}[nilMetadata], func(t *testing.T) {
			ctx := context.Background()
			c := newFakeController()
			t.Cleanup(c.queue.ShutDown)
			_, pod, group := recoveryObjects("ReplicaSet", 1)
			if !nilMetadata {
				group.Labels = map[string]string{"team": "ml"}
				group.Annotations = map[string]string{api.JobAllocatedHyperNode: "hypernode-a"}
			}
			_, err := c.kubeClient.CoreV1().Pods(pod.Namespace).Create(ctx, pod, metav1.CreateOptions{})
			require.NoError(t, err)
			require.NoError(t, c.podInformer.Informer().GetIndexer().Add(pod.DeepCopy()))
			_, err = c.vcClient.SchedulingV1beta1().PodGroups(group.Namespace).Create(ctx, group, metav1.CreateOptions{})
			require.NoError(t, err)
			require.NoError(t, c.pgInformer.Informer().GetIndexer().Add(group.DeepCopy()))
			ownerReads := 0
			c.kubeClient.(*kubeclient.Clientset).PrependReactor("get", "replicasets", func(k8stesting.Action) (bool, runtime.Object, error) {
				ownerReads++
				return true, nil, apierrors.NewServiceUnavailable("owner unavailable")
			})
			podReads := 0
			c.kubeClient.(*kubeclient.Clientset).PrependReactor("get", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
				podReads++
				return true, nil, apierrors.NewServiceUnavailable("pod unavailable")
			})
			c.addPod(pod)
			require.True(t, c.processNextReq())
			// A delayed delete event can arrive after a group has been recreated.
			deleted := group.DeepCopy()
			deleted.UID = "previous-group-uid"
			c.deletePodGroup(deleted)
			require.True(t, c.processNextReq())
			stored, err := c.vcClient.SchedulingV1beta1().PodGroups(group.Namespace).Get(ctx, group.Name, metav1.GetOptions{})
			require.NoError(t, err)
			assert.Equal(t, group, stored)
			assert.Zero(t, ownerReads, "existing groups must not rebuild requirements")
			assert.Zero(t, podReads, "live Pod reads are only needed before creating a missing group")
			for _, action := range c.vcClient.(*vcclient.Clientset).Actions() {
				assert.NotEqual(t, "update", action.GetVerb())
			}
		})
	}
}

func TestPodGroupRecoveryRetriesOwnerRead(t *testing.T) {
	ctx := context.Background()
	c := newFakeController()
	t.Cleanup(c.queue.ShutDown)
	obj, pod, deleted := recoveryObjects("ReplicaSet", 1)
	storeRecoveryOwner(t, c, obj)
	_, err := c.kubeClient.CoreV1().Pods(pod.Namespace).Create(ctx, pod, metav1.CreateOptions{})
	require.NoError(t, err)
	require.NoError(t, c.podInformer.Informer().GetIndexer().Add(pod.DeepCopy()))
	failed := false
	c.kubeClient.(*kubeclient.Clientset).PrependReactor("get", "replicasets", func(k8stesting.Action) (bool, runtime.Object, error) {
		if !failed {
			failed = true
			return true, nil, apierrors.NewServiceUnavailable("transient owner read failure")
		}
		return false, nil, nil
	})
	c.deletePodGroup(deleted)
	require.True(t, c.processNextReq())
	req := podRequest{podName: pod.Name, podNamespace: pod.Namespace, podUID: pod.UID}
	require.Equal(t, 1, c.queue.NumRequeues(req))
	_, err = c.vcClient.SchedulingV1beta1().PodGroups(deleted.Namespace).Get(ctx, deleted.Name, metav1.GetOptions{})
	require.True(t, apierrors.IsNotFound(err), "a failed read must not create a group with default minMember=1")
	require.True(t, c.processNextReq())
	assert.Zero(t, c.queue.NumRequeues(req))
	group, err := c.vcClient.SchedulingV1beta1().PodGroups(deleted.Namespace).Get(ctx, deleted.Name, metav1.GetOptions{})
	require.NoError(t, err)
	assert.EqualValues(t, 3, group.Spec.MinMember)
}

func TestPodGroupRecoveryAfterConcurrentExpansion(t *testing.T) {
	ctx := context.Background()
	c := newFakeController()
	t.Cleanup(c.queue.ShutDown)
	obj, pod, group := recoveryObjects("ReplicaSet", 0)
	rs := obj.(*appsv1.ReplicaSet)
	storeRecoveryOwner(t, c, obj)
	_, err := c.vcClient.SchedulingV1beta1().PodGroups(group.Namespace).Create(ctx, group, metav1.CreateOptions{})
	require.NoError(t, err)
	require.NoError(t, c.pgInformer.Informer().GetIndexer().Add(group.DeepCopy()))
	c.vcClient.(*vcclient.Clientset).PrependReactor("delete", "podgroups", func(k8stesting.Action) (bool, runtime.Object, error) {
		// Expansion happens after both cleanup checks, just before deletion.
		expanded := rs.DeepCopy()
		expanded.Spec.Replicas = ptr.To[int32](1)
		if _, err := c.kubeClient.AppsV1().ReplicaSets(rs.Namespace).Update(ctx, expanded, metav1.UpdateOptions{}); err != nil {
			return true, nil, err
		}
		if _, err := c.kubeClient.CoreV1().Pods(pod.Namespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
			return true, nil, err
		}
		return false, nil, nil
	})
	c.addReplicaSet(rs)
	require.True(t, c.processNextReq())
	_, err = c.vcClient.SchedulingV1beta1().PodGroups(group.Namespace).Get(ctx, group.Name, metav1.GetOptions{})
	require.True(t, apierrors.IsNotFound(err))
	// Informers update their stores before delivering notifications.
	require.NoError(t, c.pgInformer.Informer().GetIndexer().Delete(group))
	require.NoError(t, c.podInformer.Informer().GetIndexer().Add(pod.DeepCopy()))
	c.deletePodGroup(group)
	require.Equal(t, 1, c.queue.Len())
	require.True(t, c.processNextReq())
	restored, err := c.vcClient.SchedulingV1beta1().PodGroups(group.Namespace).Get(ctx, group.Name, metav1.GetOptions{})
	require.NoError(t, err)
	assert.EqualValues(t, 3, restored.Spec.MinMember)
	// A further historical zero event must leave the recovered group intact.
	c.addReplicaSet(rs)
	require.True(t, c.processNextReq())
	stored, err := c.vcClient.SchedulingV1beta1().PodGroups(group.Namespace).Get(ctx, group.Name, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, restored, stored)
}

func TestPodGroupRecoveryFromInformer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	c := newFakeController()
	obj, pod, group := recoveryObjects("ReplicaSet", 1)
	storeRecoveryOwner(t, c, obj)
	_, err := c.kubeClient.CoreV1().Pods(pod.Namespace).Create(ctx, pod, metav1.CreateOptions{})
	require.NoError(t, err)
	done := make(chan struct{})
	t.Cleanup(func() {
		cancel()
		c.queue.ShutDown()
		<-done
		c.informerFactory.Shutdown()
		c.vcInformerFactory.Shutdown()
	})
	// The shared fake-controller fixture has zero background workers. Run its
	// normal cache startup, then consume its existing Pod queue with one worker.
	c.Run(ctx.Done())
	go func() {
		defer close(done)
		c.worker()
	}()
	ready := func() bool {
		current, err := c.vcClient.SchedulingV1beta1().PodGroups(group.Namespace).Get(ctx, group.Name, metav1.GetOptions{})
		if err != nil || current.Spec.MinMember != 3 {
			return false
		}
		_, err = c.pgLister.PodGroups(group.Namespace).Get(group.Name)
		return err == nil
	}
	// Bound asynchronous watch delivery; no external Kubernetes server is used.
	require.Eventually(t, ready, 5*time.Second, 10*time.Millisecond)
	require.NoError(t, c.vcClient.SchedulingV1beta1().PodGroups(group.Namespace).Delete(ctx, group.Name, metav1.DeleteOptions{}))
	require.Eventually(t, ready, 5*time.Second, 10*time.Millisecond)
}
