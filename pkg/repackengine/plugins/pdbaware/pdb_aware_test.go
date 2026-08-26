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

package pdbaware

import (
	"testing"

	schedapi "volcano.sh/volcano/pkg/scheduler/api"

	"volcano.sh/volcano/pkg/repackengine/framework"
)

// fakePDBConsultant lets a test decide each task's evictability.
type fakePDBConsultant struct {
	evictable func(*schedapi.TaskInfo) (bool, bool)
}

func (f fakePDBConsultant) Evictable(t *schedapi.TaskInfo) (bool, bool) {
	return f.evictable(t)
}

func movable(ssn *framework.Session, name string) bool {
	return ssn.Movable()(&schedapi.TaskInfo{Job: "ns/pg", Name: name})
}

func TestPDBAwareVetoesPDBBlockedVictim(t *testing.T) {
	ssn := framework.OpenSession(framework.SessionConfig{}, framework.PluginOptions(Name))
	defer framework.CloseSession(ssn)
	ssn.SetPDBConsultant(fakePDBConsultant{
		evictable: func(*schedapi.TaskInfo) (bool, bool) { return false, true },
	})
	if movable(ssn, "blocked") {
		t.Fatal("a task whose matched PDB allows zero disruptions must be immovable")
	}
}

func TestPDBAwareAllowsPDBWithAllowance(t *testing.T) {
	ssn := framework.OpenSession(framework.SessionConfig{}, framework.PluginOptions(Name))
	defer framework.CloseSession(ssn)
	ssn.SetPDBConsultant(fakePDBConsultant{
		evictable: func(*schedapi.TaskInfo) (bool, bool) { return true, true },
	})
	if !movable(ssn, "allowed") {
		t.Fatal("a task whose PDB still allows disruptions must be movable")
	}
}

func TestPDBAwareAllowsNoEffectivePDB(t *testing.T) {
	ssn := framework.OpenSession(framework.SessionConfig{}, framework.PluginOptions(Name))
	defer framework.CloseSession(ssn)
	// ok=false means no effective PDB → never vetoes.
	ssn.SetPDBConsultant(fakePDBConsultant{
		evictable: func(*schedapi.TaskInfo) (bool, bool) { return false, false },
	})
	if !movable(ssn, "no-pdb") {
		t.Fatal("a task without an effective PDB must remain movable")
	}
}

func TestPDBAwareIsOptionalWithoutConsultant(t *testing.T) {
	// No SetPDBConsultant → the session keeps the noop consultant, so planning
	// behavior is unchanged (nothing is vetoed on PDB grounds).
	ssn := framework.OpenSession(framework.SessionConfig{}, framework.PluginOptions(Name))
	defer framework.CloseSession(ssn)
	if !movable(ssn, "anything") {
		t.Fatal("without a wired PDB consultant the pdbaware plugin must not veto")
	}
}

func TestPDBAwareMaxBlockedInPlanReservedStrictOnly(t *testing.T) {
	if err := framework.ValidatePluginArguments(Name, framework.Arguments{maxBlockedInPlanKey: 0}); err != nil {
		t.Fatalf("maxBlockedInPlan=0 (strict) must be accepted: %v", err)
	}
	if err := framework.ValidatePluginArguments(Name, framework.Arguments{maxBlockedInPlanKey: 2}); err == nil {
		t.Fatal("maxBlockedInPlan>0 is reserved for a future tolerant mode and must be rejected for now")
	}
}
