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

// Package placement owns replacement-Pod placement decisions and terminal
// result evaluation. Kubernetes writes and workqueue scheduling remain Engine
// orchestration concerns.
package placement

import (
	"sort"
	"time"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
	state "volcano.sh/volcano/pkg/controllers/repack/state"

	schedapi "volcano.sh/volcano/pkg/scheduler/api"
)

func Candidates(run *repackv1alpha1.RepackRun) []*repackv1alpha1.PodRelocationStatus {
	if run == nil {
		return nil
	}
	result := make([]*repackv1alpha1.PodRelocationStatus, 0)
	for index := range run.Status.Relocations {
		relocation := &run.Status.Relocations[index]
		if relocation.Placement.ReplacementPodName == "" || relocation.Placement.ReplacementPodUID == "" || relocation.Placement.SelectedNodeName != "" {
			continue
		}
		if relocation.Placement.Phase == repackv1alpha1.PodPlacementWaitingForNodeSelection {
			result = append(result, relocation)
		}
	}
	sort.Slice(result, func(left, right int) bool {
		return IdentityForRelocation(result[left]).Less(IdentityForRelocation(result[right]))
	})
	return result
}

func Complete(run *repackv1alpha1.RepackRun) bool {
	if run == nil || len(run.Status.Relocations) == 0 {
		return false
	}
	accepted := 0
	for index := range run.Status.Relocations {
		relocation := &run.Status.Relocations[index]
		if !evictionAllowsPlacement(relocation.Eviction.Phase) {
			continue
		}
		accepted++
		switch relocation.Placement.Phase {
		case repackv1alpha1.PodPlacementPlaced, repackv1alpha1.PodPlacementTimedOut:
		default:
			return false
		}
	}
	return accepted > 0
}

func evictionAllowsPlacement(phase repackv1alpha1.PodEvictionPhase) bool {
	return phase == repackv1alpha1.PodEvictionAccepted ||
		phase == repackv1alpha1.PodEvictionIndirectlyRemoved
}

type FreedNodeComparison struct {
	Planned    []string
	Actual     []string
	Missing    []string
	Unexpected []string
	Equal      bool
}

// NodeReleaseObservation separates the causal outcome of this repack from the
// instantaneous occupancy in the terminal scheduler snapshot. Released nodes
// had their complete planned victim set relocated. Reused nodes are a subset of
// Released that already carry unrelated target-resource Pods. Pending nodes
// need informer/cache convergence; Blocked nodes have a definite plan failure.
type NodeReleaseObservation struct {
	Released []string
	Reused   []string
	Pending  []string
	Blocked  []string
}

type TerminalDecision struct {
	Succeeded     bool
	Reason        string
	Nodes         FreedNodeComparison
	CurrentlyFree []string
	Reused        []string
	Pending       []string
	Blocked       []string
}

func EvaluateTerminal(
	run *repackv1alpha1.RepackRun,
	resultSnapshotUnavailable bool,
	observation NodeReleaseObservation,
) TerminalDecision {
	_, alternativeNodePlacements, timedOut := outcomeCounts(run)
	var planned, currentlyFree []string
	if run != nil && run.Status.Plan != nil {
		planned = run.Status.Plan.FreedNodes
	}
	if run != nil && run.Status.Result != nil {
		currentlyFree = run.Status.Result.FreedNodes
	}
	nodes := CompareNodeSets(planned, observation.Released)
	decision := TerminalDecision{
		Nodes:         nodes,
		CurrentlyFree: SortedUniqueNodeNames(currentlyFree),
		Reused:        SortedUniqueNodeNames(observation.Reused),
		Pending:       SortedUniqueNodeNames(observation.Pending),
		Blocked:       SortedUniqueNodeNames(observation.Blocked),
	}
	switch {
	case timedOut > 0:
		decision.Reason = state.ReasonPlacementTimedOut
	case resultSnapshotUnavailable || run == nil || run.Status.Result == nil || !run.Status.Result.MetricsVerified:
		decision.Reason = state.ReasonResultVerificationFailed
	case len(decision.Blocked) > 0:
		decision.Reason = state.ReasonBenefitNotRealized
	case len(decision.Pending) > 0:
		decision.Reason = state.ReasonResultVerificationFailed
	case !nodes.Equal:
		decision.Reason = state.ReasonBenefitNotRealized
	case alternativeNodePlacements > 0:
		decision.Succeeded = true
		decision.Reason = state.ReasonExecutionCompletedWithAlternativePlacement
	default:
		decision.Succeeded = true
		decision.Reason = state.ReasonExecutionCompleted
	}
	return decision
}

func CompareFreedNodeSets(run *repackv1alpha1.RepackRun) FreedNodeComparison {
	var planned, actual []string
	if run != nil && run.Status.Plan != nil {
		planned = run.Status.Plan.FreedNodes
	}
	if run != nil && run.Status.Result != nil {
		actual = run.Status.Result.FreedNodes
	}
	return CompareNodeSets(planned, actual)
}

func CompareNodeSets(planned, actual []string) FreedNodeComparison {
	result := FreedNodeComparison{Planned: SortedUniqueNodeNames(planned), Actual: SortedUniqueNodeNames(actual)}
	plannedSet := make(map[string]struct{}, len(result.Planned))
	actualSet := make(map[string]struct{}, len(result.Actual))
	for _, nodeName := range result.Planned {
		plannedSet[nodeName] = struct{}{}
	}
	for _, nodeName := range result.Actual {
		actualSet[nodeName] = struct{}{}
	}
	for _, nodeName := range result.Planned {
		if _, found := actualSet[nodeName]; !found {
			result.Missing = append(result.Missing, nodeName)
		}
	}
	for _, nodeName := range result.Actual {
		if _, found := plannedSet[nodeName]; !found {
			result.Unexpected = append(result.Unexpected, nodeName)
		}
	}
	result.Equal = len(result.Missing) == 0 && len(result.Unexpected) == 0
	return result
}

func SortedUniqueNodeNames(nodeNames []string) []string {
	unique := make(map[string]struct{}, len(nodeNames))
	for _, nodeName := range nodeNames {
		if nodeName != "" {
			unique[nodeName] = struct{}{}
		}
	}
	result := make([]string, 0, len(unique))
	for nodeName := range unique {
		result = append(result, nodeName)
	}
	sort.Strings(result)
	return result
}

func ObservationDeadlinePassed(run *repackv1alpha1.RepackRun, now time.Time) bool {
	return run != nil && run.Status.ExecutionDeadline != nil &&
		!now.Before(run.Status.ExecutionDeadline.Time)
}

func NodeReleaseVerificationPending(run *repackv1alpha1.RepackRun, observation NodeReleaseObservation, now time.Time) bool {
	return len(observation.Blocked) == 0 && len(observation.Pending) > 0 && !ObservationDeadlinePassed(run, now)
}

func BindingsVisible(nodes []*schedapi.NodeInfo, relocations []repackv1alpha1.PodRelocationStatus) bool {
	expected := make(map[string]string)
	for index := range relocations {
		relocation := &relocations[index]
		if relocation.Placement.Phase == repackv1alpha1.PodPlacementPlaced {
			if relocation.Placement.ReplacementPodUID == "" || relocation.Placement.ActualNodeName == "" {
				return false
			}
			expected[string(relocation.Placement.ReplacementPodUID)] = relocation.Placement.ActualNodeName
		}
	}
	if len(expected) == 0 {
		return true
	}
	for _, node := range nodes {
		if node == nil {
			continue
		}
		for _, task := range node.Tasks {
			if task == nil {
				continue
			}
			expectedNode, found := expected[string(task.UID)]
			if found && expectedNode == node.Name {
				delete(expected, string(task.UID))
			}
		}
	}
	return len(expected) == 0
}

func MarkBenefitUnverified(run *repackv1alpha1.RepackRun) {
	if run == nil || run.Status.Plan == nil || run.Status.Plan.Summary == nil {
		return
	}
	if run.Status.Result == nil {
		run.Status.Result = &repackv1alpha1.RepackResult{}
	}
	run.Status.Result.FragAfterPercent = run.Status.Plan.Summary.FragBeforePercent
	run.Status.Result.FreedNodeCount = 0
	run.Status.Result.FreedNodes = nil
	run.Status.Result.MetricsVerified = false
}

func outcomeCounts(run *repackv1alpha1.RepackRun) (selectedNodePlacements, alternativeNodePlacements, timedOut int) {
	if run == nil {
		return 0, 0, 0
	}
	for index := range run.Status.Relocations {
		relocation := &run.Status.Relocations[index]
		switch relocation.Placement.Phase {
		case repackv1alpha1.PodPlacementPlaced:
			if relocation.Placement.SelectedNodeName != "" && relocation.Placement.ActualNodeName == relocation.Placement.SelectedNodeName {
				selectedNodePlacements++
			} else {
				alternativeNodePlacements++
			}
		case repackv1alpha1.PodPlacementTimedOut:
			timedOut++
		}
	}
	return
}
