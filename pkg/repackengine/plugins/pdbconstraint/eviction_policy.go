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

package pdbconstraint

import (
	"encoding/json"
	"fmt"

	schedapi "volcano.sh/volcano/pkg/scheduler/api"
)

const (
	evictionPolicyAnnotationKey = "volcano.sh/eviction-policy"
	evictionPolicyAPIVersion    = "scheduling.volcano.sh/v1alpha1"

	modelServingNameLabel      = "modelserving.volcano.sh/name"
	modelServingGroupNameLabel = "modelserving.volcano.sh/group-name"
	modelServingRoleLabel      = "modelserving.volcano.sh/role"

	podGroupZeroDisruptionReason = "eviction_policy_podgroup_zero_disruption"
	subgroupZeroDisruptionReason = "eviction_policy_subgroup_zero_disruption"
	invalidEvictionPolicyReason  = "invalid_eviction_policy"
)

type evictionPolicy struct {
	APIVersion string                    `json:"apiVersion"`
	PodGroup   *evictionBudget           `json:"podGroup,omitempty"`
	Subgroups  map[string]evictionBudget `json:"subgroups,omitempty"`
}

type evictionBudget struct {
	// A pointer distinguishes an explicit zero from an omitted required field.
	DisruptionsAllowed *int32 `json:"disruptionsAllowed"`
}

// compiledEvictionPolicy retains every declared subgroup, including those with
// a positive allowance. An absent subgroup means no subgroup-level constraint;
// the Pod remains subject to the PodGroup-level rule.
type compiledEvictionPolicy struct {
	podGroupBlocked bool
	subgroups       map[string]bool
	validationError error
}

type evictionPolicyBlockInfo struct {
	reason   string
	subgroup string
}

func compileEvictionPolicy(raw string) (*compiledEvictionPolicy, error) {
	var policy evictionPolicy
	if err := json.Unmarshal([]byte(raw), &policy); err != nil {
		return nil, fmt.Errorf("decode annotation: %w", err)
	}
	if policy.APIVersion != evictionPolicyAPIVersion {
		return nil, fmt.Errorf("unsupported apiVersion %q", policy.APIVersion)
	}

	compiled := &compiledEvictionPolicy{
		subgroups: make(map[string]bool, len(policy.Subgroups)),
	}
	if policy.PodGroup != nil {
		allowed, err := validateEvictionBudget(*policy.PodGroup)
		if err != nil {
			return nil, fmt.Errorf("podGroup: %w", err)
		}
		compiled.podGroupBlocked = allowed == 0
	}
	for subgroup, budget := range policy.Subgroups {
		if subgroup == "" {
			return nil, fmt.Errorf("subgroup name must not be empty")
		}
		allowed, err := validateEvictionBudget(budget)
		if err != nil {
			return nil, fmt.Errorf("subgroup %q: %w", subgroup, err)
		}
		compiled.subgroups[subgroup] = allowed == 0
	}
	return compiled, nil
}

func validateEvictionBudget(budget evictionBudget) (int32, error) {
	if budget.DisruptionsAllowed == nil {
		return 0, fmt.Errorf("disruptionsAllowed is required")
	}
	if *budget.DisruptionsAllowed < 0 {
		return 0, fmt.Errorf("disruptionsAllowed must not be negative")
	}
	return *budget.DisruptionsAllowed, nil
}

func blockingEvictionPolicy(task *schedapi.TaskInfo, policy *compiledEvictionPolicy) (evictionPolicyBlockInfo, bool) {
	if task == nil || task.Pod == nil || policy == nil {
		return evictionPolicyBlockInfo{}, false
	}
	if policy.validationError != nil {
		return evictionPolicyBlockInfo{reason: invalidEvictionPolicyReason}, true
	}
	if policy.podGroupBlocked {
		return evictionPolicyBlockInfo{reason: podGroupZeroDisruptionReason}, true
	}
	if len(policy.subgroups) == 0 {
		return evictionPolicyBlockInfo{}, false
	}

	subgroup := task.Pod.Labels[modelServingRoleLabel]
	if subgroup == "" {
		// Non-ModelServing producers may use Volcano's existing task role as
		// their subgroup name. ModelServing's built-in role label takes priority.
		subgroup = task.TaskRole
	}
	blocked, found := policy.subgroups[subgroup]
	if subgroup == "" || !found {
		return evictionPolicyBlockInfo{}, false
	}
	if blocked {
		return evictionPolicyBlockInfo{reason: subgroupZeroDisruptionReason, subgroup: subgroup}, true
	}
	return evictionPolicyBlockInfo{}, false
}
