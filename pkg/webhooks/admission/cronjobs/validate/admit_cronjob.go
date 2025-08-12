package validate

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
	admissionv1 "k8s.io/api/admission/v1"
	whv1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/capabilities"
	"volcano.sh/apis/pkg/apis/batch/v1alpha1"
	schedulingv1beta1 "volcano.sh/apis/pkg/apis/scheduling/v1beta1"
	"volcano.sh/volcano/pkg/controllers/job/plugins"
	controllerMpi "volcano.sh/volcano/pkg/controllers/job/plugins/distributed-framework/mpi"
	"volcano.sh/volcano/pkg/webhooks/router"
	"volcano.sh/volcano/pkg/webhooks/schema"
	"volcano.sh/volcano/pkg/webhooks/util"
)

func init() {
	capabilities.Initialize(capabilities.Capabilities{
		AllowPrivileged: true,
		PrivilegedSources: capabilities.PrivilegedSources{
			HostNetworkSources: []string{},
			HostPIDSources:     []string{},
			HostIPCSources:     []string{},
		},
	})
	router.RegisterAdmission(service)
}

var service = &router.AdmissionService{
	Path: "/cronjobs/validate",
	Func: AdmitCronjobs,

	Config: config,

	ValidatingConfig: &whv1.ValidatingWebhookConfiguration{
		Webhooks: []whv1.ValidatingWebhook{{
			Name: "validatecronjob.volcano.sh",
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

func AdmitCronjobs(ar admissionv1.AdmissionReview) *admissionv1.AdmissionResponse {
	klog.V(3).Infof("admitting cronjobs -- %s", ar.Request.Operation)

	cronjob, err := schema.DecodeCronJob(ar.Request.Object, ar.Request.Resource)
	if err != nil {
		return util.ToAdmissionResponse(err)
	}
	var msg string
	reviewResponse := admissionv1.AdmissionResponse{}
	reviewResponse.Allowed = true

	switch ar.Request.Operation {
	case admissionv1.Create:
		msg = validateCronJobCreate(cronjob, &reviewResponse)
	case admissionv1.Update:
		err = validateCronJobUpdate(cronjob)
		if err != nil {
			return util.ToAdmissionResponse(err)
		}
	default:
		err := fmt.Errorf("expect operation to be 'CREATE' or 'UPDATE'")
		return util.ToAdmissionResponse(err)
	}

	if !reviewResponse.Allowed {
		reviewResponse.Result = &metav1.Status{Message: strings.TrimSpace(msg)}
	}
	return &reviewResponse
}
func validateCronJobCreate(cronjob *v1alpha1.CronJob, reviewResponse *admissionv1.AdmissionResponse) string {
	msg := validateCronjobSpec(&cronjob.Spec, cronjob.Namespace)
	msg += validateCronJobName(cronjob.Name)
	if msg != "" {
		reviewResponse.Allowed = false
	}
	return msg
}
func validateCronJobUpdate(new *v1alpha1.CronJob) error {
	msg := validateCronjobSpec(&new.Spec, new.Namespace)
	msg += validateCronJobName(new.Name)
	return errors.New(msg)
}
func validateCronjobSpec(spec *v1alpha1.CronJobSpec, nameSpace string) string {
	var msg string
	if len(spec.Schedule) == 0 {
		msg += " schedule is required, but got empty"
	} else {
		if strings.Contains(spec.Schedule, "TZ") {
			msg += " schedule should not contain TZ or CRON_TZ, TZ should only be set in the timeZone field"
		} else {
			if _, err := cron.ParseStandard(spec.Schedule); err != nil {
				msg += "schedule is not a valid cron expression"
			}
		}
	}

	var validTimeZoneCharacters = regexp.MustCompile(`^[A-Za-z\.\-_0-9+]{1,14}$`)
	if spec.TimeZone != nil {
		if len(*spec.TimeZone) == 0 {
			msg += " timeZone must be nil or non-empty string, got empty string"
		} else {
			for _, part := range strings.Split(*spec.TimeZone, "/") {
				if part == "." || part == ".." || strings.HasPrefix(part, "-") || !validTimeZoneCharacters.MatchString(part) {
					msg += " unknown timeZone"
				}
			}
			if strings.EqualFold(*spec.TimeZone, "Local") {
				msg += " timeZone can't be Local, and must be defined in https://www.iana.org/time-zones"
			} else {
				if _, err := time.LoadLocation(*spec.TimeZone); err != nil {
					msg += " invalid timeZone"
				}
			}
		}
	}
	if spec.StartingDeadlineSeconds != nil && *spec.StartingDeadlineSeconds < 0 {
		msg += " startingDeadlineSeconds can not be negative"
	}
	if spec.SuccessfulJobsHistoryLimit != nil && *spec.SuccessfulJobsHistoryLimit < 0 {
		msg += " successfulJobsHistoryLimit can not be negative"
	}
	if spec.FailedJobsHistoryLimit != nil && *spec.FailedJobsHistoryLimit < 0 {
		msg += " failedJobsHistoryLimit can not be negative"
	}

	switch spec.ConcurrencyPolicy {
	case "":
		msg += " ConcurrencyPolicy must be nil or Allow/Forbid/Replace, got empty string"
	case v1alpha1.AllowConcurrent, v1alpha1.ForbidConcurrent, v1alpha1.ReplaceConcurrent:
		break
	default:
		msg += " ConcurrencyPolicy must be nil or Allow/Forbid/Replace"
	}
	msg += validateJobSpec(&spec.JobTemplate.Spec, nameSpace)
	return msg
}
func validateJobSpec(spec *v1alpha1.JobSpec, nameSpace string) string {
	var msg string
	taskNames := map[string]string{}
	var totalReplicas int32

	if spec.MinAvailable < 0 {
		return "'minAvailable' must be >= 0."
	}

	if spec.MaxRetry < 0 {
		return "'maxRetry' cannot be less than zero."
	}

	if spec.TTLSecondsAfterFinished != nil && *spec.TTLSecondsAfterFinished < 0 {
		return "'ttlSecondsAfterFinished' cannot be less than zero."
	}

	if len(spec.Tasks) == 0 {
		return "No task specified in job spec"
	}

	if _, ok := spec.Plugins[controllerMpi.MPIPluginName]; ok {
		mp := controllerMpi.NewInstance(spec.Plugins[controllerMpi.MPIPluginName])
		masterIndex := getTaskIndexUnderJob(mp.GetMasterName(), spec)
		workerIndex := getTaskIndexUnderJob(mp.GetWorkerName(), spec)
		if masterIndex == -1 {
			return "The specified mpi master task was not found"
		}
		if workerIndex == -1 {
			return "The specified mpi worker task was not found"
		}
	}

	hasDependenciesBetweenTasks := false
	for index, task := range spec.Tasks {
		if task.DependsOn != nil {
			hasDependenciesBetweenTasks = true
		}

		if task.Replicas < 0 {
			msg += fmt.Sprintf(" 'replicas' < 0 in task: %s;", task.Name)
		}

		if task.MinAvailable != nil {
			if *task.MinAvailable < 0 {
				msg += fmt.Sprintf(" 'minAvailable' < 0 in task: %s;", task.Name)
			} else if *task.MinAvailable > task.Replicas {
				msg += fmt.Sprintf(" 'minAvailable' is greater than 'replicas' in task: %s;", task.Name)
			}
		}

		// count replicas
		totalReplicas += task.Replicas

		// validate task name
		if errMsgs := validation.IsDNS1123Label(task.Name); len(errMsgs) > 0 {
			msg += fmt.Sprintf(" %v;", errMsgs)
		}

		// duplicate task name
		if _, found := taskNames[task.Name]; found {
			msg += fmt.Sprintf(" duplicated task name %s;", task.Name)
			break
		} else {
			taskNames[task.Name] = task.Name
		}

		if err := validatePolicies(task.Policies, field.NewPath("spec.tasks.policies")); err != nil {
			msg += err.Error() + fmt.Sprintf(" valid events are %v, valid actions are %v;",
				getValidEvents(), getValidActions())
		}
		msg += validateTaskTemplate(task, nameSpace, index)
	}

	if totalReplicas < spec.MinAvailable {
		msg += " job 'minAvailable' should not be greater than total replicas in tasks;"
	}

	if err := validatePolicies(spec.Policies, field.NewPath("spec.policies")); err != nil {
		msg = msg + err.Error() + fmt.Sprintf(" valid events are %v, valid actions are %v;",
			getValidEvents(), getValidActions())
	}

	// invalid job plugins
	if len(spec.Plugins) != 0 {
		for name := range spec.Plugins {
			if _, found := plugins.GetPluginBuilder(name); !found {
				msg += fmt.Sprintf(" unable to find job plugin: %s;", name)
			}
		}
	}

	if err := validateIO(spec.Volumes); err != nil {
		msg += err.Error()
	}

	queue, err := config.QueueLister.Get(spec.Queue)
	if err != nil {
		msg += fmt.Sprintf(" unable to find job queue: %v;", err)
	} else {
		if queue.Status.State != schedulingv1beta1.QueueStateOpen {
			msg += fmt.Sprintf(" can only submit job to queue with state `Open`, "+
				"queue `%s` status is `%s`;", queue.Name, queue.Status.State)
		}

		// validate hierarchical queue
		if queue.Name == "root" {
			msg += " can not submit job to root queue;"
		} else {
			queueList, err := config.QueueLister.List(labels.Everything())
			if err != nil {
				msg += fmt.Sprintf("failed to get list queues: %v;", err)
			}
			childQueues := make([]*schedulingv1beta1.Queue, 0)
			for _, childQueue := range queueList {
				if childQueue.Spec.Parent == queue.Name {
					childQueues = append(childQueues, childQueue)
				}
			}
			if len(childQueues) > 0 {
				msg += fmt.Sprintf(" can only submit job to leaf queue, "+"queue `%s` has %d child queues;", queue.Name, len(childQueues))
			}
		}
	}

	if hasDependenciesBetweenTasks {
		_, isDag := topoSort(spec)
		if !isDag {
			msg += " job has dependencies between tasks, but doesn't form a directed acyclic graph(DAG);"
		}
	}
	return msg
}
