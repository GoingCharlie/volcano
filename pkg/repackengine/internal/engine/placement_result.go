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

package engine

import (
	"context"
	"fmt"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
	state "volcano.sh/volcano/pkg/controllers/repack/state"
	placementexecutor "volcano.sh/volcano/pkg/repackengine/executor/placement"
	enginestatus "volcano.sh/volcano/pkg/repackengine/status"

	"volcano.sh/volcano/pkg/repackengine/adapter"
	engineapi "volcano.sh/volcano/pkg/repackengine/api"
	engineconf "volcano.sh/volcano/pkg/repackengine/conf"
	engineframework "volcano.sh/volcano/pkg/repackengine/framework"
	schedapi "volcano.sh/volcano/pkg/scheduler/api"
	schedframework "volcano.sh/volcano/pkg/scheduler/framework"
)

func (e *Engine) finishPlacement(ctx context.Context, run *repackv1alpha1.RepackRun) engineframework.RuntimeResult {
	placementTimedOut := false
	resultSnapshotUnavailable := false
	releaseObservation := placementexecutor.NodeReleaseObservation{}
	for index := range run.Status.Relocations {
		if run.Status.Relocations[index].Placement.Phase == repackv1alpha1.PodPlacementTimedOut {
			placementTimedOut = true
		}
	}
	targetResource := engineconf.ResolveResource(run, e.config.DefaultResource)
	if placementTimedOut {
		// A timed-out replacement has been released to normal scheduling but has
		// not produced a trustworthy terminal binding. Do not claim the optimistic
		// plan benefit while workload demand may be temporarily absent.
		placementexecutor.MarkBenefitUnverified(run)
	} else {
		schedulerSession := e.clusterCache.OpenSession(e.tiers, e.configurations)
		nodes := adapter.NewSessionSnapshot(schedulerSession, targetResource, nil).Nodes()
		visible := placementexecutor.BindingsVisible(nodes, run.Status.Relocations)
		if !visible {
			schedframework.CloseSessionReadOnly(schedulerSession)
			// The nomination controller may observe Pod binding just before the
			// scheduler cache applies the same Pod update. Wait for one coherent
			// snapshot before publishing cluster-wide actual metrics.
			if !placementexecutor.ObservationDeadlinePassed(run, e.now()) {
				return engineframework.RuntimeResult{RequeueAfter: capAtExecutionDeadline(run, e.now(), placementRetryInterval)}
			}
			resultSnapshotUnavailable = true
			placementexecutor.MarkBenefitUnverified(run)
		} else {
			updateActualExecuteResult(run, nodes, targetResource)
			releaseObservation = observeNodeRelease(run, nodes, targetResource)
			comparison := placementexecutor.CompareNodeSets(run.Status.Plan.FreedNodes, releaseObservation.Released)
			verificationPending := placementexecutor.NodeReleaseVerificationPending(run, releaseObservation, e.now())
			schedframework.CloseSessionReadOnly(schedulerSession)
			// Replacement binding and source-node resource release are observed
			// through independent informer streams. A coherent scheduler snapshot
			// can therefore contain the replacement while still retaining a
			// terminating victim (or another stale source-node task) briefly.
			// Do not turn that convergence window into a permanent failed Run.
			//
			// Occupancy by an unrelated Pod is successful reuse. Only a stale
			// original victim, an unobservable source, or this run's own
			// replacement on a drain target prevents release verification.
			if verificationPending {
				klog.V(4).InfoS("repack: waiting for planned node-release observation to converge",
					"run", run.Name,
					"plannedNodes", comparison.Planned,
					"releasedNodes", comparison.Actual,
					"pendingNodes", releaseObservation.Pending,
					"blockedNodes", releaseObservation.Blocked,
					"retryAfter", placementRetryInterval)
				return engineframework.RuntimeResult{RequeueAfter: capAtExecutionDeadline(run, e.now(), placementRetryInterval)}
			}
		}
	}

	decision := placementexecutor.EvaluateTerminal(run, resultSnapshotUnavailable, releaseObservation)
	message := enginestatus.PlacementMessage(run, targetResource, decision)
	result := run.Status.Result
	resultMetrics := enginestatus.Result(run)
	selectedNodePlacementCount, alternativeNodePlacementCount, timedOutPlacementCount := enginestatus.PlacementOutcomeCounts(run)
	klog.V(3).InfoS("repack: replacement placement terminal result evaluated",
		"run", run.Name, "succeeded", decision.Succeeded, "reason", decision.Reason,
		"resultSnapshotUnavailable", resultSnapshotUnavailable,
		"selectedNodePlacementCount", selectedNodePlacementCount,
		"alternativeNodePlacementCount", alternativeNodePlacementCount,
		"timedOutPlacementCount", timedOutPlacementCount,
		"plannedNodeCount", len(decision.Nodes.Planned), "releasedNodeCount", len(decision.Nodes.Actual),
		"currentlyFreeNodeCount", len(decision.CurrentlyFree), "reusedNodeCount", len(decision.Reused),
		"missingReleasedNodeCount", len(decision.Nodes.Missing), "missingReleasedNodes", enginestatus.FormatNodeNames(decision.Nodes.Missing),
		"pendingNodeCount", len(decision.Pending), "blockedNodeCount", len(decision.Blocked),
		"fragAfterPercent", resultMetrics.FragAfter, "movedCardCount", resultMetrics.MovedCards)
	klog.V(4).InfoS("repack: terminal node-release result",
		"run", run.Name, "plannedNodes", decision.Nodes.Planned,
		"releasedNodes", decision.Nodes.Actual, "currentlyFreeNodes", decision.CurrentlyFree,
		"reusedNodes", decision.Reused, "pendingNodes", decision.Pending, "blockedNodes", decision.Blocked,
		"missingReleasedNodes", decision.Nodes.Missing, "setsEqual", decision.Nodes.Equal,
		"result", result)
	if decision.Succeeded {
		state.MarkSucceeded(run, decision.Reason, message)
	} else {
		state.MarkFailed(run, decision.Reason, message)
	}
	if err := e.updateStatusTerminal(ctx, run); err != nil {
		return runtimeError(err)
	}
	// Placement is terminal even if API cleanup needs a retry. Do not hold the
	// global Execute slot while only removing our own metadata and gates.
	if e.markExecuteDone(run.Name) {
		e.requeueGatedRuns(run.Name)
	}
	// The terminal result is durable before cleanup. Returning an error makes the
	// workqueue retry the idempotent cleanup without ever repeating eviction.
	if err := e.cleanupPlacement(ctx, run); err != nil {
		return runtimeError(fmt.Errorf("cleanup placement after terminal result: %w", err))
	}
	return engineframework.RuntimeResult{}
}

// observeNodeRelease verifies the causal result of this repack independently
// from the node's instantaneous occupancy. An unrelated target-resource Pod is
// evidence that released capacity has already been reused, not that eviction
// failed. Exact victim/replacement UIDs keep a stale victim or a replacement
// placed back on a drain target from being mistaken for unrelated reuse.
func observeNodeRelease(
	run *repackv1alpha1.RepackRun,
	nodes []*schedapi.NodeInfo,
	targetResource v1.ResourceName,
) placementexecutor.NodeReleaseObservation {
	observation := placementexecutor.NodeReleaseObservation{}
	if run == nil || run.Status.Plan == nil {
		return observation
	}

	plannedNodes := placementexecutor.SortedUniqueNodeNames(run.Status.Plan.FreedNodes)
	plannedSet := make(map[string]struct{}, len(plannedNodes))
	for _, nodeName := range plannedNodes {
		plannedSet[nodeName] = struct{}{}
	}

	nodesByName := make(map[string]*schedapi.NodeInfo, len(nodes))
	for _, node := range nodes {
		if node != nil {
			nodesByName[node.Name] = node
		}
	}

	successfulRelocations := make(map[placementexecutor.Identity]*repackv1alpha1.PodRelocationStatus, len(run.Status.Relocations))
	replacementUIDs := make(map[string]struct{}, len(run.Status.Relocations))
	blocked := make(map[string]struct{})
	pending := make(map[string]struct{})
	for index := range run.Status.Relocations {
		relocation := &run.Status.Relocations[index]
		if (relocation.Eviction.Phase != repackv1alpha1.PodEvictionAccepted &&
			relocation.Eviction.Phase != repackv1alpha1.PodEvictionIndirectlyRemoved) ||
			relocation.Placement.Phase != repackv1alpha1.PodPlacementPlaced {
			continue
		}
		successfulRelocations[placementexecutor.IdentityForRelocation(relocation)] = relocation
		if relocation.Placement.ReplacementPodUID != "" {
			replacementUIDs[string(relocation.Placement.ReplacementPodUID)] = struct{}{}
		}
		if _, isDrainTarget := plannedSet[relocation.Placement.ActualNodeName]; isDrainTarget {
			blocked[relocation.Placement.ActualNodeName] = struct{}{}
		}
	}

	hasPlannedVictim := make(map[string]bool, len(plannedNodes))
	victimUIDsBySource := make(map[string]map[string]struct{}, len(plannedNodes))
	for moveIndex := range run.Status.Plan.Moves {
		move := &run.Status.Plan.Moves[moveIndex]
		for podIndex := range move.Pods {
			pod := &move.Pods[podIndex]
			if _, planned := plannedSet[pod.FromNode]; !planned {
				continue
			}
			hasPlannedVictim[pod.FromNode] = true
			identity := placementexecutor.IdentityForMove(move.Namespace, move.PodGroupName, pod.Name, pod.ToNode)
			relocation, successful := successfulRelocations[identity]
			if !successful {
				blocked[pod.FromNode] = struct{}{}
				continue
			}
			if relocation.VictimPodUID == "" || relocation.Placement.ReplacementPodUID == "" {
				pending[pod.FromNode] = struct{}{}
				continue
			}
			if victimUIDsBySource[pod.FromNode] == nil {
				victimUIDsBySource[pod.FromNode] = make(map[string]struct{})
			}
			victimUIDsBySource[pod.FromNode][string(relocation.VictimPodUID)] = struct{}{}
		}
	}

	for _, nodeName := range plannedNodes {
		if !hasPlannedVictim[nodeName] {
			blocked[nodeName] = struct{}{}
		}
		if _, isBlocked := blocked[nodeName]; isBlocked {
			observation.Blocked = append(observation.Blocked, nodeName)
			continue
		}

		node := nodesByName[nodeName]
		if node == nil || engineapi.Scalar(node.Allocatable, targetResource) <= 0 {
			pending[nodeName] = struct{}{}
		}
		if _, isPending := pending[nodeName]; isPending {
			observation.Pending = append(observation.Pending, nodeName)
			continue
		}

		targetTaskCount := 0
		reused := false
		for taskID, task := range node.Tasks {
			if task == nil || engineapi.Scalar(task.Resreq, targetResource) <= 0 {
				continue
			}
			targetTaskCount++
			uid := string(task.UID)
			if uid == "" {
				uid = string(taskID)
			}
			if _, originalVictim := victimUIDsBySource[nodeName][uid]; originalVictim {
				pending[nodeName] = struct{}{}
				continue
			}
			if _, ownReplacement := replacementUIDs[uid]; ownReplacement {
				blocked[nodeName] = struct{}{}
				continue
			}
			reused = true
		}
		if _, isBlocked := blocked[nodeName]; isBlocked {
			observation.Blocked = append(observation.Blocked, nodeName)
			continue
		}
		if _, isPending := pending[nodeName]; isPending {
			observation.Pending = append(observation.Pending, nodeName)
			continue
		}
		used := engineapi.Scalar(node.Used, targetResource)
		if used > 0 && targetTaskCount == 0 {
			// The aggregate says the resource is occupied but there is no Pod
			// identity to attribute it to. Wait rather than guessing that it is
			// unrelated reuse.
			observation.Pending = append(observation.Pending, nodeName)
			continue
		}
		observation.Released = append(observation.Released, nodeName)
		if reused {
			observation.Reused = append(observation.Reused, nodeName)
		}
	}
	return observation
}

func updateActualExecuteResult(run *repackv1alpha1.RepackRun, nodes []*schedapi.NodeInfo, targetResource v1.ResourceName) {
	if run == nil || run.Status.Plan == nil || run.Status.Plan.Summary == nil || run.Status.Result == nil {
		return
	}
	run.Status.Result.FragAfterPercent = enginestatus.PercentagePoints(engineapi.MeasureResourceFragmentation(nodes, targetResource).FragmentationRate())
	nodesByName := make(map[string]*schedapi.NodeInfo, len(nodes))
	for _, node := range nodes {
		if node != nil {
			nodesByName[node.Name] = node
		}
	}
	realizedCandidates := placementexecutor.SortedUniqueNodeNames(enginestatus.RealizedFreedNodeNames(run))
	realizedCandidateSet := make(map[string]struct{}, len(realizedCandidates))
	for _, nodeName := range realizedCandidates {
		realizedCandidateSet[nodeName] = struct{}{}
	}
	actuallyFreedNodes := make([]string, 0, len(realizedCandidates))
	for _, nodeName := range placementexecutor.SortedUniqueNodeNames(run.Status.Plan.FreedNodes) {
		if _, realized := realizedCandidateSet[nodeName]; !realized {
			klog.V(4).InfoS("repack: planned node is not an actual-free candidate because its complete victim set was not removed",
				"run", run.Name, "node", nodeName, "resource", targetResource)
			continue
		}
		node := nodesByName[nodeName]
		if node == nil {
			klog.V(4).InfoS("repack: planned node not present in terminal scheduler snapshot",
				"run", run.Name, "node", nodeName, "resource", targetResource)
			continue
		}
		allocatable := engineapi.Scalar(node.Allocatable, targetResource)
		used := engineapi.Scalar(node.Used, targetResource)
		if allocatable <= 0 {
			klog.V(4).InfoS("repack: planned node no longer provides the target resource",
				"run", run.Name, "node", nodeName, "resource", targetResource,
				"allocatable", allocatable, "used", used)
			continue
		}
		if used == 0 {
			actuallyFreedNodes = append(actuallyFreedNodes, nodeName)
			klog.V(4).InfoS("repack: planned node verified free of the target resource",
				"run", run.Name, "node", nodeName, "resource", targetResource,
				"allocatable", allocatable, "used", used)
			continue
		}
		klog.V(4).InfoS("repack: planned node remains occupied by the target resource",
			"run", run.Name, "node", nodeName, "resource", targetResource,
			"allocatable", allocatable, "used", used)
	}
	run.Status.Result.FreedNodes = actuallyFreedNodes
	run.Status.Result.FreedNodeCount = int32(len(actuallyFreedNodes))
	run.Status.Result.MetricsVerified = true
	comparison := placementexecutor.CompareFreedNodeSets(run)
	klog.V(3).InfoS("repack: terminal scheduler snapshot metrics measured",
		"run", run.Name, "resource", targetResource,
		"fragAfterPercent", run.Status.Result.FragAfterPercent,
		"currentlyFreeNodeCount", run.Status.Result.FreedNodeCount,
		"movedCardCount", run.Status.Result.MovedCardCount,
		"plannedNodeCount", len(comparison.Planned),
		"notCurrentlyFreeNodeCount", len(comparison.Missing), "notCurrentlyFreeNodes", enginestatus.FormatNodeNames(comparison.Missing),
		"unexpectedFreeNodeCount", len(comparison.Unexpected))
	klog.V(4).InfoS("repack: terminal scheduler snapshot node sets",
		"run", run.Name, "resource", targetResource,
		"plannedNodes", comparison.Planned, "currentlyFreeNodes", comparison.Actual,
		"notCurrentlyFreeNodes", comparison.Missing, "unexpectedFreeNodes", comparison.Unexpected)
}
