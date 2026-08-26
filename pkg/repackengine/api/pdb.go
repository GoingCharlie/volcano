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

package api

import (
	schedapi "volcano.sh/volcano/pkg/scheduler/api"
)

// PDBConsultant answers "may this task be disrupted right now?" at planning
// time, according to the PDB disruption controller's computed status. The
// engine Session exposes it to plugins; the pdbaware plugin vetoes victims
// whose PodGroup's PDB leaves no disruption allowance, so a PDB-blocked Pod is
// never planned as a victim (the Eviction API remains the final arbiter at
// Execute time, with the wave retry as the backstop).
type PDBConsultant interface {
	// Evictable reports whether task may currently be disrupted under its
	// PodGroup's PDB. ok=false means the PodGroup has no effective PDB (or the
	// allowance cannot be determined), which never vetoes a move.
	Evictable(task *schedapi.TaskInfo) (evictable bool, ok bool)
}
