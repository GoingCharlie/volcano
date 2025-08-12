package mutate

import (
	"encoding/json"
	"fmt"
	"strconv"

	admissionv1 "k8s.io/api/admission/v1"
	whv1 "k8s.io/api/admissionregistration/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	"k8s.io/utils/ptr"
	"volcano.sh/apis/pkg/apis/batch/v1alpha1"
	"volcano.sh/volcano/pkg/controllers/job/plugins/distributed-framework/mpi"
	"volcano.sh/volcano/pkg/controllers/job/plugins/distributed-framework/pytorch"
	"volcano.sh/volcano/pkg/controllers/job/plugins/distributed-framework/tensorflow"
	"volcano.sh/volcano/pkg/webhooks/router"
	"volcano.sh/volcano/pkg/webhooks/schema"
	"volcano.sh/volcano/pkg/webhooks/util"
)

const (
	// DefaultQueue constant stores the name of the queue as "default"
	DefaultQueue                      = "default"
	DefaultConcurrencyPolicy          = v1alpha1.AllowConcurrent
	DefaultSuccessfulJobsHistoryLimit = int32(3)
	DefaultFailedJobsHistoryLimit     = int32(1)
	// DefaultMaxRetry is the default number of retries.
	DefaultMaxRetry = 3

	defaultMaxRetry int32 = 3
)

func init() {
	router.RegisterAdmission(service)
}

var service = &router.AdmissionService{
	Path: "/cronjobs/mutate",
	Func: CronJobs,

	Config: config,

	MutatingConfig: &whv1.MutatingWebhookConfiguration{
		Webhooks: []whv1.MutatingWebhook{{
			Name: "mutatecronjob.volcano.sh",
			Rules: []whv1.RuleWithOperations{
				{
					Operations: []whv1.OperationType{whv1.Create},
					Rule: whv1.Rule{
						APIGroups:   []string{"batch.volcano.sh"},
						APIVersions: []string{"v1alpha1"},
						Resources:   []string{"cronjobs"},
					},
				},
			},
		}},
	},
}

var config = &router.AdmissionServiceConfig{}

type patchOperation struct {
	Op    string      `json:"op"`
	Path  string      `json:"path"`
	Value interface{} `json:"value,omitempty"`
}

// Jobs mutate jobs.
func CronJobs(ar admissionv1.AdmissionReview) *admissionv1.AdmissionResponse {
	klog.V(3).Infof("mutating cronjobs")

	cronjob, err := schema.DecodeCronJob(ar.Request.Object, ar.Request.Resource)
	if err != nil {
		return util.ToAdmissionResponse(err)
	}

	var patchBytes []byte
	switch ar.Request.Operation {
	case admissionv1.Create, admissionv1.Update:
		patchBytes, _ = createPatch(cronjob)
	default:
		err = fmt.Errorf("expect operation to be 'CREATE' ")
		return util.ToAdmissionResponse(err)
	}

	klog.V(3).Infof("AdmissionResponse: patch=%v", string(patchBytes))
	reviewResponse := admissionv1.AdmissionResponse{
		Allowed: true,
		Patch:   patchBytes,
	}
	if len(patchBytes) > 0 {
		pt := admissionv1.PatchTypeJSONPatch
		reviewResponse.PatchType = &pt
	}

	return &reviewResponse
}
func createPatch(cronjob *v1alpha1.CronJob) ([]byte, error) {
	var patch []patchOperation
	patchConcurrencyPolicy := patchConcurrencyPolicy(cronjob)
	if patchConcurrencyPolicy != nil {
		patch = append(patch, *patchConcurrencyPolicy)
	}
	patchSuccessfulJobsHistoryLimit := patchSuccessfulJobsHistoryLimit(cronjob)
	if patchSuccessfulJobsHistoryLimit != nil {
		patch = append(patch, *patchSuccessfulJobsHistoryLimit)
	}
	patchFailedJobsHistoryLimit := patchFailedJobsHistoryLimit(cronjob)
	if patchFailedJobsHistoryLimit != nil {
		patch = append(patch, *patchFailedJobsHistoryLimit)
	}
	patchSuspend := patchSuspend(cronjob)
	if patchSuspend != nil {
		patch = append(patch, *patchSuspend)
	}
	patches := createJobPatch(cronjob)
	patch = append(patch, patches...)
	return json.Marshal(patch)
}
func patchConcurrencyPolicy(cronjob *v1alpha1.CronJob) *patchOperation {
	//Add default concurrencyPolicy if not specified.
	if cronjob.Spec.ConcurrencyPolicy == "" {
		return &patchOperation{Op: "add", Path: "/spec/concurrencyPolicy", Value: DefaultConcurrencyPolicy}
	}
	return nil
}
func patchSuccessfulJobsHistoryLimit(cronjob *v1alpha1.CronJob) *patchOperation {
	//Add default successfulJobsHistoryLimit if not specified.
	if cronjob.Spec.SuccessfulJobsHistoryLimit == nil {
		return &patchOperation{Op: "add", Path: "/spec/successfulJobsHistoryLimit", Value: ptr.To(DefaultSuccessfulJobsHistoryLimit)}
	}
	return nil
}
func patchFailedJobsHistoryLimit(cronjob *v1alpha1.CronJob) *patchOperation {
	//Add default failedJobsHistoryLimit if not specified.
	if cronjob.Spec.FailedJobsHistoryLimit == nil {
		return &patchOperation{Op: "add", Path: "/spec/failedJobsHistoryLimit", Value: ptr.To(DefaultFailedJobsHistoryLimit)}
	}
	return nil
}
func patchSuspend(cronjob *v1alpha1.CronJob) *patchOperation {
	//Add default suspend if not specified.
	if cronjob.Spec.Suspend == nil {
		return &patchOperation{Op: "add", Path: "/spec/suspend", Value: new(bool)}
	}
	return nil
}
func createJobPatch(cronjob *v1alpha1.CronJob) []patchOperation {
	var patch []patchOperation

	pathQueue := patchDefaultQueue(cronjob)
	if pathQueue != nil {
		patch = append(patch, *pathQueue)
	}
	// pathScheduler := patchDefaultScheduler(cronjob)
	// if pathScheduler != nil {
	// 	patch = append(patch, *pathScheduler)
	// }
	pathMaxRetry := patchDefaultMaxRetry(cronjob)
	if pathMaxRetry != nil {
		patch = append(patch, *pathMaxRetry)
	}
	pathSpec := mutateSpec(cronjob.Spec.JobTemplate.Spec.Tasks, "/spec/jobTemplate/spec/tasks")
	if pathSpec != nil {
		patch = append(patch, *pathSpec)
	}
	pathMinAvailable := patchDefaultMinAvailable(cronjob)
	if pathMinAvailable != nil {
		patch = append(patch, *pathMinAvailable)
	}
	// Add default plugins for some distributed-framework plugin cases
	patchPlugins := patchDefaultPlugins(cronjob)
	if patchPlugins != nil {
		patch = append(patch, *patchPlugins)
	}
	return patch
}

func patchDefaultQueue(cronjob *v1alpha1.CronJob) *patchOperation {
	//Add default queue if not specified.
	if cronjob.Spec.JobTemplate.Spec.Queue == "" {
		return &patchOperation{Op: "add", Path: "/spec/jobTemplate/spec/queue", Value: DefaultQueue}
	}
	return nil
}

// func patchDefaultScheduler(cronjob *v1alpha1.CronJob) *patchOperation {
// 	// Add default scheduler name if not specified.
// 	if cronjob.Spec.JobTemplate.Spec.SchedulerName == "" {
// 		return &patchOperation{Op: "add", Path: "/spec/jobTemplate/spec/schedulerName", Value: commonutil.GenerateSchedulerName(config.SchedulerNames)}
// 	}
// 	return nil
// }

func patchDefaultMaxRetry(cronjob *v1alpha1.CronJob) *patchOperation {
	// Add default maxRetry if maxRetry is zero.
	if cronjob.Spec.JobTemplate.Spec.MaxRetry == 0 {
		return &patchOperation{Op: "add", Path: "/spec/jobTemplate/spec/maxRetry", Value: DefaultMaxRetry}
	}
	return nil
}

func patchDefaultMinAvailable(cronjob *v1alpha1.CronJob) *patchOperation {
	// Add default minAvailable if minAvailable is zero.
	if cronjob.Spec.JobTemplate.Spec.MinAvailable == 0 {
		var jobMinAvailable int32
		for _, task := range cronjob.Spec.JobTemplate.Spec.Tasks {
			if task.MinAvailable != nil {
				jobMinAvailable += *task.MinAvailable
			} else {
				jobMinAvailable += task.Replicas
			}
		}

		return &patchOperation{Op: "add", Path: "/spec/jobTemplate/spec/minAvailable", Value: jobMinAvailable}
	}
	return nil
}

func mutateSpec(tasks []v1alpha1.TaskSpec, basePath string) *patchOperation {
	// TODO: Enable this configuration when dependOn supports coexistence with the gang plugin
	// if _, ok := job.Spec.Plugins[mpi.MpiPluginName]; ok {
	// 	mpi.AddDependsOn(job)
	// }
	patched := false
	for index := range tasks {
		// add default task name
		taskName := tasks[index].Name
		if len(taskName) == 0 {
			patched = true
			tasks[index].Name = v1alpha1.DefaultTaskSpec + strconv.Itoa(index)
		}

		if tasks[index].Template.Spec.HostNetwork && tasks[index].Template.Spec.DNSPolicy == "" {
			patched = true
			tasks[index].Template.Spec.DNSPolicy = v1.DNSClusterFirstWithHostNet
		}

		if tasks[index].MinAvailable == nil {
			patched = true
			minAvailable := tasks[index].Replicas
			tasks[index].MinAvailable = &minAvailable
		}

		if tasks[index].MaxRetry == 0 {
			patched = true
			tasks[index].MaxRetry = defaultMaxRetry
		}
	}
	if !patched {
		return nil
	}
	return &patchOperation{
		Op:    "replace",
		Path:  basePath,
		Value: tasks,
	}
}

func patchDefaultPlugins(cronjob *v1alpha1.CronJob) *patchOperation {
	if cronjob.Spec.JobTemplate.Spec.Plugins == nil {
		return nil
	}
	plugins := map[string][]string{}
	for k, v := range cronjob.Spec.JobTemplate.Spec.Plugins {
		plugins[k] = v
	}

	// Because the tensorflow-plugin, mpi-plugin and pytorch-plugin depend on svc-plugin.
	// If the svc-plugin is not defined, we should add it.
	_, hasTf := cronjob.Spec.JobTemplate.Spec.Plugins[tensorflow.TFPluginName]
	_, hasMPI := cronjob.Spec.JobTemplate.Spec.Plugins[mpi.MPIPluginName]
	_, hasPytorch := cronjob.Spec.JobTemplate.Spec.Plugins[pytorch.PytorchPluginName]
	if hasTf || hasMPI || hasPytorch {
		if _, ok := plugins["svc"]; !ok {
			plugins["svc"] = []string{}
		}
	}

	if _, ok := cronjob.Spec.JobTemplate.Spec.Plugins["mpi"]; ok {
		if _, ok := plugins["ssh"]; !ok {
			plugins["ssh"] = []string{}
		}
	}

	return &patchOperation{
		Op:    "replace",
		Path:  "/spec/jobTemplate/spec/plugins",
		Value: plugins,
	}
}
