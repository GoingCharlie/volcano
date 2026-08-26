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

// Package pdbaware vetoes planning victims whose PodGroup's PDB leaves no
// disruption allowance, so a PDB-blocked Pod is never planned as a victim.
// The Eviction API remains the final PDB arbiter at Execute time (the wave
// retry is the backstop); this plugin only prevents obviously-blocked Pods
// from entering a plan in the first place.
package pdbaware

import (
	"fmt"

	schedapi "volcano.sh/volcano/pkg/scheduler/api"

	"volcano.sh/volcano/pkg/repackengine/framework"
)

const Name = "pdbaware"

// Reserved arguments. The first version is strictly veto-only: a Pod whose
// matched PDB has DisruptionsAllowed==0 is never movable. maxBlockedInPlan is
// reserved (must be 0 for now) for a future "tolerate up to N PDB-blocked
// victims per plan and rely on the Execute-time wave retry" mode.
const (
	// maxBlockedInPlanKey, when > 0 in a future version, would allow up to N
	// PDB-blocked victims to remain in the plan so a node is not entirely
	// abandoned because one of its Pods is PDB-protected.
	maxBlockedInPlanKey = "maxBlockedInPlan"
)

func init() {
	framework.RegisterPlugin(Name, framework.PluginRegistration{
		Factory: func(framework.Arguments) framework.Plugin { return &pdbAwarePlugin{} },
		Validator: func(args framework.Arguments) error {
			// Reserved for a future tolerant mode; only 0 (strict) is accepted now.
			value, err := args.NonNegativeInt(maxBlockedInPlanKey, 0)
			if err != nil {
				return err
			}
			if value != 0 {
				return fmt.Errorf(
					"pdbaware %s is reserved for a future tolerant mode and must be 0 (strict) for now", maxBlockedInPlanKey)
			}
			return nil
		},
	})
}

type pdbAwarePlugin struct{}

func (*pdbAwarePlugin) Name() string { return Name }

func (*pdbAwarePlugin) OnSessionOpen(ssn *framework.Session) {
	// Resolve the consultant lazily inside the callback: the engine wires the
	// PDB source AFTER OpenSession runs the plugins' OnSessionOpen, so capturing
	// ssn.PDBConsultant() here would freeze the noop default.
	ssn.AddMovableFn(func(task *schedapi.TaskInfo) bool {
		evictable, ok := ssn.PDBConsultant().Evictable(task)
		if !ok {
			return true // no effective PDB → no veto
		}
		return evictable
	})
}

func (*pdbAwarePlugin) OnSessionClose(*framework.Session) {}
