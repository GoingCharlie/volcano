package cronjob

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"volcano.sh/apis/pkg/apis/batch/v1alpha1"
	e2eutil "volcano.sh/volcano/test/e2e/util"
)

var _ = Describe("CronJob E2E Test", func() {
	// sleepCommand := []string{"sh", "-c", "echo 'CronJob executed at' $(date); sleep 180"}
	normalCommand := []string{"sh", "-c", "echo 'CronJob executed at' $(date)"}
	It("Create and schedule basic cronjob", func() {
		ctx := e2eutil.InitTestContext(e2eutil.Options{})
		defer e2eutil.CleanupTestContext(ctx)

		cronJobName := "test-cronjob"
		cronJob := createTestCronjob(cronJobName, ctx.Namespace, "* * * * *", v1alpha1.AllowConcurrent, normalCommand)

		createdCronJob, err := createCronJob(ctx, ctx.Namespace, cronJob)
		Expect(err).NotTo(HaveOccurred())
		Expect(createdCronJob.Name).Should(Equal(cronJobName))

		Eventually(func() bool {
			cj, err := getCronJob(ctx, ctx.Namespace, cronJobName)
			if err != nil {
				return false
			}
			return cj.Status.LastScheduleTime != nil
		}, 3*time.Minute, 10*time.Second).Should(BeTrue())

		Eventually(func() int {
			jobs, err := getJobList(ctx, cronJob)
			if err != nil {
				return 0
			}
			return len(jobs.Items)
		}, 3*time.Minute, 10*time.Second).Should(BeNumerically(">=", 1))

		By("Cleaning up test resources")
		err = deleteCronJob(ctx, ctx.Namespace, cronJobName)
		Expect(err).NotTo(HaveOccurred(), "Failed to delete CronJob")
	})

	// It("CronJob with suspend functionality", func() {
	// 	ctx := e2eutil.InitTestContext(e2eutil.Options{})
	// 	defer e2eutil.CleanupTestContext(ctx)

	// 	suspend := true
	// 	cronJobName := "suspended-cronjob"
	// 	cronJob := createTestCronjob(cronJobName, ctx.Namespace, "*/1 * * * *", v1alpha1.AllowConcurrent, normalCommand)
	// 	cronJob.Spec.Suspend = &suspend
	// 	_, err := createCronJob(ctx, ctx.Namespace, cronJob)
	// 	Expect(err).NotTo(HaveOccurred())
	// 	Consistently(func() int {
	// 		jobs, err := getJobList(ctx, cronJob)
	// 		if err != nil {
	// 			return -1
	// 		}
	// 		count := 0
	// 		for _, job := range jobs.Items {
	// 			if job.Labels["cronjob"] == cronJobName {
	// 				count++
	// 			}
	// 		}
	// 		return count
	// 	}, 3*time.Minute, 10*time.Second).Should(Equal(0))
	// 	By("Cleaning up test resources")
	// 	err = deleteCronJob(ctx, ctx.Namespace, cronJobName)
	// 	Expect(err).NotTo(HaveOccurred(), "Failed to delete CronJob")
	// })

	// It("CronJob with allow concurrency", func() {
	// 	ctx := e2eutil.InitTestContext(e2eutil.Options{})
	// 	defer e2eutil.CleanupTestContext(ctx)

	// 	By("create cronJob")
	// 	cronJobName := "allow-cronjob"
	// 	cronJob := createTestCronjob(cronJobName, ctx.Namespace, "* * * * *", v1alpha1.AllowConcurrent, sleepCommand)
	// 	createdCronJob, err := createCronJob(ctx, ctx.Namespace, cronJob)
	// 	Expect(err).NotTo(HaveOccurred())
	// 	Expect(createdCronJob.Name).Should(Equal(cronJobName))

	// 	By("Ensuring more than one job in active")
	// 	Eventually(func() int {
	// 		active, err := getActiveNum(ctx, ctx.Namespace, cronJobName)
	// 		if err != nil {
	// 			Fail(fmt.Sprintf("Failed to get active jobs: %v", err))
	// 		}
	// 		return active
	// 	}, 5*time.Minute, 10*time.Second).Should(BeNumerically(">=", 2))

	// 	By("Ensuring at least two jobs in job lister")
	// 	Eventually(func() int {
	// 		active, err := getJobActiveNum(ctx, cronJob)
	// 		if err != nil {
	// 			Fail(fmt.Sprintf("Failed to list jobs: %v", err))
	// 		}
	// 		return active
	// 	}, 5*time.Minute, 10*time.Second).Should(BeNumerically(">=", 2))

	// 	By("Cleaning up test resources")
	// 	err = deleteCronJob(ctx, ctx.Namespace, cronJobName)
	// 	Expect(err).NotTo(HaveOccurred(), "Failed to delete CronJob")
	// })

	// It("CronJob with forbid concurrency", func() {
	// 	ctx := e2eutil.InitTestContext(e2eutil.Options{})
	// 	defer e2eutil.CleanupTestContext(ctx)

	// 	By("create cronJob with ForbidConcurrent policy")
	// 	cronJobName := "forbid-cronjob"
	// 	cronJob := createTestCronjob(cronJobName, ctx.Namespace, "* * * * *", v1alpha1.ForbidConcurrent, sleepCommand)
	// 	createdCronJob, err := createCronJob(ctx, ctx.Namespace, cronJob)
	// 	Expect(err).NotTo(HaveOccurred())
	// 	Expect(createdCronJob.Name).Should(Equal(cronJobName))

	// 	By("Waiting for first job to start")
	// 	Eventually(func() bool {
	// 		jobs, err := getJobList(ctx, cronJob)
	// 		if err != nil || len(jobs.Items) == 0 {
	// 			return false
	// 		}
	// 		return true
	// 	}, 3*time.Minute, 5*time.Second).Should(BeTrue())

	// 	By("Ensuring only one job is active at any time")
	// 	Consistently(func() int {
	// 		active, err := getActiveNum(ctx, ctx.Namespace, cronJobName)
	// 		if err != nil {
	// 			Fail(fmt.Sprintf("Failed to get active jobs: %v", err))
	// 		}
	// 		return active
	// 	}, 5*time.Minute, 10*time.Second).Should(Equal(1))

	// 	By("Ensuring only one job is running")
	// 	Consistently(func() int {
	// 		active, err := getJobActiveNum(ctx, cronJob)
	// 		if err != nil {
	// 			Fail(fmt.Sprintf("Failed to list jobs: %v", err))
	// 		}
	// 		return active
	// 	}, 5*time.Minute, 10*time.Second).Should(Equal(1))

	// 	By("Cleaning up test resources")
	// 	err = deleteCronJob(ctx, ctx.Namespace, cronJobName)
	// 	Expect(err).NotTo(HaveOccurred(), "Failed to delete CronJob")
	// })
})

func isJobFinished(job *v1alpha1.Job) bool {
	if job.Status.State.Phase == v1alpha1.Completed || job.Status.State.Phase == v1alpha1.Failed || job.Status.State.Phase == v1alpha1.Terminated {
		return true
	}
	return false
}
func createTestCronjob(name, nameSpace, schedule string, concurrency v1alpha1.ConcurrencyPolicy, command []string) *v1alpha1.CronJob {
	return &v1alpha1.CronJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: nameSpace,
		},
		Spec: v1alpha1.CronJobSpec{
			Schedule:          schedule,
			ConcurrencyPolicy: concurrency,
			JobTemplate: v1alpha1.JobTemplateSpec{
				Spec: v1alpha1.JobSpec{
					MinAvailable: 1,
					Tasks: []v1alpha1.TaskSpec{
						{
							Name:     "test-task",
							Replicas: 1,
							Template: corev1.PodTemplateSpec{
								Spec: corev1.PodSpec{
									RestartPolicy: corev1.RestartPolicyNever,
									Containers: []corev1.Container{
										{
											Name:    "test-container",
											Image:   "busybox:1.24",
											Command: command,
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}
}
func getJobActiveNum(ctx *e2eutil.TestContext, cronJob *v1alpha1.CronJob) (int, error) {
	jobs, err := getJobList(ctx, cronJob)
	if err != nil {
		return -1, err
	}
	activeCount := 0
	for _, job := range jobs.Items {
		if !isJobFinished(&job) {
			activeCount++
		}
	}
	return activeCount, nil
}
func getActiveNum(ctx *e2eutil.TestContext, ns, name string) (int, error) {
	cronjob, err := getCronJob(ctx, ns, name)
	if err != nil {
		return -1, err
	}
	return len(cronjob.Status.Active), nil
}

func createCronJob(ctx *e2eutil.TestContext, ns string, cronJob *v1alpha1.CronJob) (*v1alpha1.CronJob, error) {
	return ctx.Vcclient.BatchV1alpha1().CronJobs(ns).Create(
		context.TODO(), cronJob, metav1.CreateOptions{})
}
func getCronJob(ctx *e2eutil.TestContext, ns, name string) (*v1alpha1.CronJob, error) {
	return ctx.Vcclient.BatchV1alpha1().CronJobs(ns).Get(
		context.TODO(), name, metav1.GetOptions{})
}
func deleteCronJob(ctx *e2eutil.TestContext, ns, name string) error {
	return ctx.Vcclient.BatchV1alpha1().CronJobs(ns).Delete(
		context.TODO(), name, metav1.DeleteOptions{})
}
func getJobList(ctx *e2eutil.TestContext, cronJob *v1alpha1.CronJob) (*v1alpha1.JobList, error) {
	print("ns+name:", cronJob.Namespace, cronJob.Name, "\n")
	jobList, err := ctx.Vcclient.BatchV1alpha1().Jobs(ctx.Namespace).List(
		context.TODO(), metav1.ListOptions{})
	count := 0
	if err != nil {
		return nil, fmt.Errorf("failed to list jobs: %w", err)
	}

	var filteredJobs []v1alpha1.Job
	for _, job := range jobList.Items {
		count++
		controllerRef := metav1.GetControllerOf(&job)
		if controllerRef == nil {
			continue
		}

		if controllerRef.Kind == "CronJob" &&
			controllerRef.APIVersion == v1alpha1.SchemeGroupVersion.String() &&
			controllerRef.Name == cronJob.Name &&
			controllerRef.UID == cronJob.UID {
			filteredJobs = append(filteredJobs, job)
		}
	}
	print(count, "\n")
	return &v1alpha1.JobList{
		Items: filteredJobs,
	}, nil
}
