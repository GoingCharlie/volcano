/*
Copyright 2018 The Volcano Authors.

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

package validate

import (
	"fmt"
	"strings"

	"github.com/hashicorp/go-multierror"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	k8score "k8s.io/kubernetes/pkg/apis/core"
	k8scorev1 "k8s.io/kubernetes/pkg/apis/core/v1"
	k8scorevalid "k8s.io/kubernetes/pkg/apis/core/validation"
	batchv1alpha1 "volcano.sh/apis/pkg/apis/batch/v1alpha1"
	busv1alpha1 "volcano.sh/apis/pkg/apis/bus/v1alpha1"
)

// policyEventMap defines all policy events and whether to allow external use.
var policyEventMap = map[busv1alpha1.Event]bool{
	busv1alpha1.AnyEvent:           true,
	busv1alpha1.PodFailedEvent:     true,
	busv1alpha1.PodEvictedEvent:    true,
	busv1alpha1.PodPendingEvent:    true,
	busv1alpha1.JobUnknownEvent:    true,
	busv1alpha1.TaskCompletedEvent: true,
	busv1alpha1.TaskFailedEvent:    true,
	busv1alpha1.JobUpdatedEvent:    true,
	busv1alpha1.OutOfSyncEvent:     false,
	busv1alpha1.CommandIssuedEvent: false,
	busv1alpha1.PodRunningEvent:    false,
}

// policyActionMap defines all policy actions and whether to allow external use.
var policyActionMap = map[busv1alpha1.Action]bool{
	busv1alpha1.AbortJobAction:     true,
	busv1alpha1.RestartJobAction:   true,
	busv1alpha1.RestartTaskAction:  true,
	busv1alpha1.RestartPodAction:   true,
	busv1alpha1.TerminateJobAction: true,
	busv1alpha1.CompleteJobAction:  true,
	busv1alpha1.ResumeJobAction:    true,
	busv1alpha1.SyncJobAction:      false,
	busv1alpha1.EnqueueAction:      false,
	busv1alpha1.SyncQueueAction:    false,
	busv1alpha1.OpenQueueAction:    false,
	busv1alpha1.CloseQueueAction:   false,
}

func validatePolicies(policies []batchv1alpha1.LifecyclePolicy, fldPath *field.Path) error {
	var err error
	policyEvents := map[busv1alpha1.Event]struct{}{}
	exitCodes := map[int32]struct{}{}

	for _, policy := range policies {
		if (policy.Event != "" || len(policy.Events) != 0) && policy.ExitCode != nil {
			err = multierror.Append(err, fmt.Errorf("must not specify event and exitCode simultaneously"))
			break
		}

		if policy.Event == "" && len(policy.Events) == 0 && policy.ExitCode == nil {
			err = multierror.Append(err, fmt.Errorf("either event and exitCode should be specified"))
			break
		}

		if len(policy.Event) != 0 || len(policy.Events) != 0 {
			bFlag := false
			policyEventsList := getEventList(policy)
			for _, event := range policyEventsList {
				if allow, ok := policyEventMap[event]; !ok || !allow {
					err = multierror.Append(err, field.Invalid(fldPath, event, "invalid policy event"))
					bFlag = true
					break
				}

				if allow, ok := policyActionMap[policy.Action]; !ok || !allow {
					err = multierror.Append(err, field.Invalid(fldPath, policy.Action, "invalid policy action"))
					bFlag = true
					break
				}
				if _, found := policyEvents[event]; found {
					err = multierror.Append(err, fmt.Errorf("duplicate event %v  across different policy", event))
					bFlag = true
					break
				} else {
					policyEvents[event] = struct{}{}
				}
			}
			if bFlag {
				break
			}
		} else {
			if *policy.ExitCode == 0 {
				err = multierror.Append(err, fmt.Errorf("0 is not a valid error code"))
				break
			}
			if _, found := exitCodes[*policy.ExitCode]; found {
				err = multierror.Append(err, fmt.Errorf("duplicate exitCode %v", *policy.ExitCode))
				break
			} else {
				exitCodes[*policy.ExitCode] = struct{}{}
			}
		}
	}

	if _, found := policyEvents[busv1alpha1.AnyEvent]; found && len(policyEvents) > 1 {
		err = multierror.Append(err, fmt.Errorf("if there's * here, no other policy should be here"))
	}

	return err
}

func getEventList(policy batchv1alpha1.LifecyclePolicy) []busv1alpha1.Event {
	policyEventsList := policy.Events
	if len(policy.Event) > 0 {
		policyEventsList = append(policyEventsList, policy.Event)
	}
	uniquePolicyEventlist := removeDuplicates(policyEventsList)
	return uniquePolicyEventlist
}

func removeDuplicates(eventList []busv1alpha1.Event) []busv1alpha1.Event {
	keys := make(map[busv1alpha1.Event]bool)
	list := []busv1alpha1.Event{}
	for _, val := range eventList {
		if _, value := keys[val]; !value {
			keys[val] = true
			list = append(list, val)
		}
	}
	return list
}

func getValidEvents() []busv1alpha1.Event {
	var events []busv1alpha1.Event
	for e, allow := range policyEventMap {
		if allow {
			events = append(events, e)
		}
	}

	return events
}

func getValidActions() []busv1alpha1.Action {
	var actions []busv1alpha1.Action
	for a, allow := range policyActionMap {
		if allow {
			actions = append(actions, a)
		}
	}

	return actions
}

// validateIO validates IO configuration.
func validateIO(volumes []batchv1alpha1.VolumeSpec) error {
	volumeMap := map[string]bool{}
	for _, volume := range volumes {
		if len(volume.MountPath) == 0 {
			return fmt.Errorf(" mountPath is required;")
		}
		if _, found := volumeMap[volume.MountPath]; found {
			return fmt.Errorf(" duplicated mountPath: %s;", volume.MountPath)
		}
		if volume.VolumeClaim == nil && volume.VolumeClaimName == "" {
			return fmt.Errorf(" either VolumeClaim or VolumeClaimName must be specified;")
		}
		if len(volume.VolumeClaimName) != 0 {
			if volume.VolumeClaim != nil {
				return fmt.Errorf("conflict: If you want to use an existing PVC, just specify VolumeClaimName." +
					"If you want to create a new PVC, you do not need to specify VolumeClaimName")
			}
			if errMsgs := k8scorevalid.ValidatePersistentVolumeName(volume.VolumeClaimName, false); len(errMsgs) > 0 {
				return fmt.Errorf("invalid VolumeClaimName %s : %v", volume.VolumeClaimName, errMsgs)
			}
		}

		volumeMap[volume.MountPath] = true
	}
	return nil
}

// topoSort uses topo sort to sort job tasks based on dependsOn field
// it will return an array contains all sorted task names and a bool which indicates whether it's a valid dag
func topoSort(spec *batchv1alpha1.JobSpec) ([]string, bool) {
	graph, inDegree, taskList := makeGraph(spec)
	var taskStack []string
	for task, degree := range inDegree {
		if degree == 0 {
			taskStack = append(taskStack, task)
		}
	}

	sortedTasks := make([]string, 0)
	for len(taskStack) > 0 {
		length := len(taskStack)
		var out string
		out, taskStack = taskStack[length-1], taskStack[:length-1]
		sortedTasks = append(sortedTasks, out)
		for in, connected := range graph[out] {
			if connected {
				graph[out][in] = false
				inDegree[in]--
				if inDegree[in] == 0 {
					taskStack = append(taskStack, in)
				}
			}
		}
	}

	isDag := len(sortedTasks) == len(taskList)
	if !isDag {
		return nil, false
	}

	return sortedTasks, isDag
}

func makeGraph(spec *batchv1alpha1.JobSpec) (map[string]map[string]bool, map[string]int, []string) {
	graph := make(map[string]map[string]bool)
	inDegree := make(map[string]int)
	taskList := make([]string, 0)

	for _, task := range spec.Tasks {
		taskList = append(taskList, task.Name)
		inDegree[task.Name] = 0
		if task.DependsOn != nil {
			for _, dependOnTask := range task.DependsOn.Name {
				if graph[dependOnTask] == nil {
					graph[dependOnTask] = make(map[string]bool)
				}

				graph[dependOnTask][task.Name] = true
				inDegree[task.Name]++
			}
		}
	}

	return graph, inDegree, taskList
}
func validateTaskTemplate(task batchv1alpha1.TaskSpec, namespace string, index int) string {
	var v1PodTemplate v1.PodTemplate
	v1PodTemplate.Template = *task.Template.DeepCopy()
	k8scorev1.SetObjectDefaults_PodTemplate(&v1PodTemplate)

	var coreTemplateSpec k8score.PodTemplateSpec
	k8scorev1.Convert_v1_PodTemplateSpec_To_core_PodTemplateSpec(&v1PodTemplate.Template, &coreTemplateSpec, nil)

	corePodTemplate := k8score.PodTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      task.Name,
			Namespace: namespace,
		},
		Template: coreTemplateSpec,
	}

	opts := k8scorevalid.PodValidationOptions{}
	if allErrs := k8scorevalid.ValidatePodTemplate(&corePodTemplate, opts); len(allErrs) > 0 {
		msg := fmt.Sprintf("spec.task[%d].", index)
		for index := range allErrs {
			msg += allErrs[index].Error() + ". "
		}
		return msg
	}

	msg := validateTaskTopoPolicy(task, index)
	if msg != "" {
		return msg
	}

	return ""
}
func validateTaskTopoPolicy(task batchv1alpha1.TaskSpec, index int) string {
	if task.TopologyPolicy == "" || task.TopologyPolicy == batchv1alpha1.None {
		return ""
	}

	template := task.Template.DeepCopy()

	for id, container := range template.Spec.Containers {
		if len(container.Resources.Requests) == 0 {
			template.Spec.Containers[id].Resources.Requests = container.Resources.Limits.DeepCopy()
		}
	}

	for id, container := range template.Spec.InitContainers {
		if len(container.Resources.Requests) == 0 {
			template.Spec.InitContainers[id].Resources.Requests = container.Resources.Limits.DeepCopy()
		}
	}

	for id, container := range append(template.Spec.Containers, template.Spec.InitContainers...) {
		requestNum := guaranteedCPUs(container)
		if requestNum == 0 {
			return fmt.Sprintf("the cpu request isn't  an integer in spec.task[%d] container[%d].",
				index, id)
		}
	}

	return ""
}
func guaranteedCPUs(container v1.Container) int {
	cpuQuantity := container.Resources.Requests[v1.ResourceCPU]
	if cpuQuantity.Value()*1000 != cpuQuantity.MilliValue() {
		return 0
	}

	return int(cpuQuantity.Value())
}
func getTaskIndexUnderJob(taskName string, spec *batchv1alpha1.JobSpec) int {
	for index, task := range spec.Tasks {
		if task.Name == taskName {
			return index
		}
	}
	return -1
}
func validateCronJobName(name string) string {
	const (
		maxTotalLength    = 63
		maxSuffixLength   = 11
		maxBaseNameLength = maxTotalLength - maxSuffixLength
	)
	if errs := validation.IsQualifiedName(name); len(errs) > 0 {
		for _, err := range errs {
			if strings.Contains(err, "must be no more than") {
				return fmt.Sprintf("cronJob base name must be no more than %d characters (including %d characters reserved for job suffix)",
					maxBaseNameLength, maxSuffixLength)
			}
		}
		return fmt.Sprintf("invalid cronJob name %q: %v", name, strings.Join(errs, "; "))
	}
	if len(name) > maxBaseNameLength {
		return fmt.Sprintf("cronJob name must be no more than %d characters to accommodate job name suffix (max %d total)",
			maxBaseNameLength, maxTotalLength)
	}

	return ""
}
