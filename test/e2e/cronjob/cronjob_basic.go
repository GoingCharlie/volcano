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
	failCommand := []string{"sh", "-c", "echo 'CronJob executed at' $(date); exit 1"}
	// It("Create and schedule basic cronjob", func() {
	// 	ctx := e2eutil.InitTestContext(e2eutil.Options{})
	// 	defer e2eutil.CleanupTestContext(ctx)

	// 	cronJobName := "test-cronjob"
	// 	cronJob := createTestCronjob(cronJobName, ctx.Namespace, "* * * * *", v1alpha1.AllowConcurrent, normalCommand)

	// 	createdCronJob, err := createCronJob(ctx, ctx.Namespace, cronJob)
	// 	Expect(err).NotTo(HaveOccurred())
	// 	Expect(createdCronJob.Name).Should(Equal(cronJobName))

	// 	Eventually(func() bool {
	// 		cj, err := getCronJob(ctx, ctx.Namespace, cronJobName)
	// 		if err != nil {
	// 			return false
	// 		}
	// 		return cj.Status.LastScheduleTime != nil
	// 	}, 3*time.Minute, 10*time.Second).Should(BeTrue())

	// 	By("Ensuring one job created")
	// 	Eventually(func() int {
	// 		jobs, err := getJobList(ctx, createdCronJob)
	// 		if err != nil {
	// 			return 0
	// 		}
	// 		return len(jobs.Items)
	// 	}, 3*time.Minute, 10*time.Second).Should(BeNumerically(">=", 1))

	// 	By("CronJobsScheduledAnnotation annotation exists on job")
	// 	jobs, _ := getJobList(ctx, createdCronJob)
	// 	timeAnnotation := jobs.Items[0].Annotations[v1alpha1.CronJobScheduledTimestampAnnotation]
	// 	_, err = time.Parse(time.RFC3339, timeAnnotation)
	// 	Expect(err).NotTo(HaveOccurred(), "Failed to parse cronjob-scheduled-timestamp annotation")

	// 	By("Cleaning up test resources")
	// 	err = deleteCronJob(ctx, ctx.Namespace, cronJobName)
	// 	Expect(err).NotTo(HaveOccurred(), "Failed to delete CronJob")
	// })

	// It("CronJob with suspend functionality", func() {
	// 	ctx := e2eutil.InitTestContext(e2eutil.Options{})
	// 	defer e2eutil.CleanupTestContext(ctx)

	// 	suspend := true
	// 	cronJobName := "suspended-cronjob"
	// 	cronJob := createTestCronjob(cronJobName, ctx.Namespace, "*/1 * * * *", v1alpha1.AllowConcurrent, normalCommand)
	// 	cronJob.Spec.Suspend = &suspend
	// 	createdCronJob, err := createCronJob(ctx, ctx.Namespace, cronJob)
	// 	Expect(err).NotTo(HaveOccurred())
	// 	Consistently(func() int {
	// 		jobs, err := getJobList(ctx, createdCronJob)
	// 		if err != nil {
	// 			return -1
	// 		}
	// 		return len(jobs.Items)
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
	// 		active, err := getJobActiveNum(ctx, createdCronJob)
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
	// 		jobs, err := getJobList(ctx, createdCronJob)
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
	// 		active, err := getJobActiveNum(ctx, createdCronJob)
	// 		if err != nil {
	// 			Fail(fmt.Sprintf("Failed to list jobs: %v", err))
	// 		}
	// 		return active
	// 	}, 5*time.Minute, 10*time.Second).Should(Equal(1))

	// 	By("Cleaning up test resources")
	// 	err = deleteCronJob(ctx, ctx.Namespace, cronJobName)
	// 	Expect(err).NotTo(HaveOccurred(), "Failed to delete CronJob")
	// })

	// It("CronJob with replace concurrency", func() {
	// 	ctx := e2eutil.InitTestContext(e2eutil.Options{})
	// 	defer e2eutil.CleanupTestContext(ctx)

	// 	By("create cronJob with ReplaceConcurrent policy")
	// 	cronJobName := "replace-cronjob"
	// 	cronJob := createTestCronjob(cronJobName, ctx.Namespace, "* * * * *", v1alpha1.ReplaceConcurrent, sleepCommand)
	// 	createdCronJob, err := createCronJob(ctx, ctx.Namespace, cronJob)
	// 	Expect(err).NotTo(HaveOccurred())
	// 	Expect(createdCronJob.Name).Should(Equal(cronJobName))

	// 	By("Waiting for first job to start running")
	// 	var firstJob *v1alpha1.Job
	// 	Eventually(func() bool {
	// 		jobs, err := getJobList(ctx, createdCronJob)
	// 		if err != nil || len(jobs.Items) == 0 {
	// 			return false
	// 		}
	// 		firstJob = &jobs.Items[0]
	// 		return !isJobFinished(firstJob)
	// 	}, 3*time.Minute, 5*time.Second).Should(BeTrue())

	// 	firstJobName := firstJob.Name
	// 	fmt.Printf("First job started: %s\n", firstJobName)

	// 	By("Waiting for second job to replace the first one")
	// 	Eventually(func() bool {
	// 		jobs, err := getJobList(ctx, createdCronJob)
	// 		if err != nil {
	// 			return false
	// 		}
	// 		for _, job := range jobs.Items {
	// 			if job.Name != firstJobName {
	// 				fmt.Printf("New job found: %s (replacing %s)\n", job.Name, firstJobName)
	// 				return true
	// 			}
	// 		}
	// 		return false
	// 	}, 3*time.Minute, 5*time.Second).Should(BeTrue())

	// 	By("Ensuring first job is being terminated or terminated")
	// 	Eventually(func() bool {
	// 		jobs, err := getJobList(ctx, createdCronJob)
	// 		if err != nil {
	// 			return false
	// 		}

	// 		for _, job := range jobs.Items {
	// 			if job.Name == firstJobName {
	// 				if job.Status.Terminating > 0 || isJobFinished(&job) {
	// 					fmt.Printf("First job %s is terminating or finished\n", firstJobName)
	// 					return true
	// 				}
	// 				fmt.Printf("First job %s still running: Active=%d, Terminating=%d\n",
	// 					firstJobName, job.Status.Running, job.Status.Terminating)
	// 				return false
	// 			}
	// 		}
	// 		fmt.Printf("First job %s has been removed\n", firstJobName)
	// 		return true
	// 	}, 2*time.Minute, 5*time.Second).Should(BeTrue())

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
	// 		active, err := getJobActiveNum(ctx, createdCronJob)
	// 		if err != nil {
	// 			Fail(fmt.Sprintf("Failed to list jobs: %v", err))
	// 		}
	// 		return active
	// 	}, 5*time.Minute, 10*time.Second).Should(Equal(1))

	// 	By("Cleaning up test resources")
	// 	err = deleteCronJob(ctx, ctx.Namespace, cronJobName)
	// 	Expect(err).NotTo(HaveOccurred(), "Failed to delete CronJob")
	// })

	// It("many missed schedule", func() {
	// 	ctx := e2eutil.InitTestContext(e2eutil.Options{})
	// 	defer e2eutil.CleanupTestContext(ctx)

	// 	cronJobName := "many-miss-cronjob"
	// 	cronJob := createTestCronjob(cronJobName, ctx.Namespace, "* * * * *", v1alpha1.AllowConcurrent, normalCommand)
	// 	cronJob.CreationTimestamp = metav1.Time{Time: time.Now().Add(-2 * time.Hour)}
	// 	createdCronJob, err := createCronJob(ctx, ctx.Namespace, cronJob)
	// 	Expect(err).NotTo(HaveOccurred())
	// 	Expect(createdCronJob.Name).Should(Equal(cronJobName))

	// 	By("Waiting for first job to be scheduled")
	// 	Eventually(func() bool {
	// 		cj, err := getCronJob(ctx, ctx.Namespace, cronJobName)
	// 		if err != nil {
	// 			return false
	// 		}
	// 		return cj.Status.LastScheduleTime != nil
	// 	}, 3*time.Minute, 10*time.Second).Should(BeTrue())

	// 	By("Waiting for first job to start")
	// 	Eventually(func() int {
	// 		jobs, err := getJobList(ctx, createdCronJob)
	// 		if err != nil {
	// 			return 0
	// 		}
	// 		return len(jobs.Items)
	// 	}, 3*time.Minute, 10*time.Second).Should(BeNumerically(">=", 1))

	// 	By("Cleaning up test resources")
	// 	err = deleteCronJob(ctx, ctx.Namespace, cronJobName)
	// 	Expect(err).NotTo(HaveOccurred(), "Failed to delete CronJob")
	// })
	It("remove finished job from cronjob active", func() {
		ctx := e2eutil.InitTestContext(e2eutil.Options{})
		defer e2eutil.CleanupTestContext(ctx)

		cronJobName := "process-finished-job-cronjob"
		cronJob := createTestCronjob(cronJobName, ctx.Namespace, "* * * * *", v1alpha1.AllowConcurrent, normalCommand)
		createdCronJob, err := createCronJob(ctx, ctx.Namespace, cronJob)
		Expect(err).NotTo(HaveOccurred())
		Expect(createdCronJob.Name).Should(Equal(cronJobName))

		By("Waiting for first job to be finished")
		var firstFinishedJob v1alpha1.Job
		Eventually(func() bool {
			jobs, err := getJobList(ctx, createdCronJob)
			if err != nil || len(jobs.Items) == 0 {
				return false
			}
			if isJobFinished(&jobs.Items[0]) {
				firstFinishedJob = jobs.Items[0]
				return true
			}
			return false
		}, 3*time.Minute, 10*time.Second).Should(BeTrue())

		By("Ensuring finished job not in cronjob active")
		Eventually(func() bool {
			cronJob, err := getCronJob(ctx, ctx.Namespace, cronJobName)
			if err != nil {
				return false
			}
			activeJobs := cronJob.Status.Active
			for i := range activeJobs {
				if activeJobs[i].UID == firstFinishedJob.UID {
					return false
				}
			}
			return true
		}, 3*time.Minute, 10*time.Second).Should(BeTrue())

		By("Cleaning up test resources")
		err = deleteCronJob(ctx, ctx.Namespace, cronJobName)
		Expect(err).NotTo(HaveOccurred(), "Failed to delete CronJob")
	})

	// It("delete finished and suc jobs with limit", func() {
	// 	ctx := e2eutil.InitTestContext(e2eutil.Options{})
	// 	defer e2eutil.CleanupTestContext(ctx)

	// 	cronJobName := "suc-limit-cronjob"
	// 	cronJob := createTestCronjob(cronJobName, ctx.Namespace, "* * * * *", v1alpha1.AllowConcurrent, normalCommand)
	// 	var failLimit int32 = 1
	// 	cronJob.Spec.SuccessfulJobsHistoryLimit = &failLimit
	// 	createdCronJob, err := createCronJob(ctx, ctx.Namespace, cronJob)
	// 	Expect(err).NotTo(HaveOccurred())
	// 	Expect(createdCronJob.Name).Should(Equal(cronJobName))

	// 	By("Waiting for a job suc finished")
	// 	var finishedJob v1alpha1.Job
	// 	Eventually(func() bool {
	// 		jobs, err := getJobList(ctx, createdCronJob)
	// 		if err != nil {
	// 			return false
	// 		}
	// 		for _, job := range jobs.Items {
	// 			if job.Status.State.Phase == v1alpha1.Completed {
	// 				finishedJob = job
	// 				return true
	// 			}
	// 		}
	// 		return false
	// 	}, 3*time.Minute, 10*time.Second).Should(BeTrue())

	// 	By("Ensuring finished job does not exist")
	// 	Eventually(func() bool {
	// 		jobs, err := getJobList(ctx, createdCronJob)
	// 		if err != nil {
	// 			return false
	// 		}
	// 		for _, job := range jobs.Items {
	// 			if job.Name == finishedJob.Name {
	// 				return false
	// 			}
	// 		}
	// 		return true
	// 	}, 3*time.Minute, 10*time.Second).Should(BeTrue())

	// 	By("Ensuring only one suc finished job exists")
	// 	Consistently(func() int {
	// 		count, err := getJobSucNum(ctx, createdCronJob)
	// 		if err != nil {
	// 			Fail(fmt.Sprintf("Failed to get jobs: %v", err))
	// 		}
	// 		return count
	// 	}, 3*time.Minute, 10*time.Second).Should(Equal(1))

	// 	By("Cleaning up test resources")
	// 	err = deleteCronJob(ctx, ctx.Namespace, cronJobName)
	// 	Expect(err).NotTo(HaveOccurred(), "Failed to delete CronJob")
	// })

	It("delete finished and fail jobs with limit", func() {
		ctx := e2eutil.InitTestContext(e2eutil.Options{})
		defer e2eutil.CleanupTestContext(ctx)

		cronJobName := "suc-limit-cronjob"
		cronJob := createTestCronjob(cronJobName, ctx.Namespace, "* * * * *", v1alpha1.AllowConcurrent, failCommand)
		var failLimit int32 = 1
		cronJob.Spec.FailedJobsHistoryLimit = &failLimit
		createdCronJob, err := createCronJob(ctx, ctx.Namespace, cronJob)
		Expect(err).NotTo(HaveOccurred())
		Expect(createdCronJob.Name).Should(Equal(cronJobName))

		By("Waiting for a job fail finished")
		var finishedJob v1alpha1.Job
		Eventually(func() bool {
			jobs, err := getJobList(ctx, createdCronJob)
			if err != nil {
				return false
			}
			for _, job := range jobs.Items {
				if job.Status.State.Phase == v1alpha1.Failed {
					finishedJob = job
					return true
				}
			}
			return false
		}, 3*time.Minute, 10*time.Second).Should(BeTrue())

		By("Ensuring finished job does not exist")
		Eventually(func() bool {
			jobs, err := getJobList(ctx, createdCronJob)
			if err != nil {
				return false
			}
			for _, job := range jobs.Items {
				if job.Name == finishedJob.Name {
					return false
				}
			}
			return true
		}, 3*time.Minute, 10*time.Second).Should(BeTrue())

		By("Ensuring only one fail finished job exists")
		Consistently(func() int {
			count, err := getJobFailNum(ctx, createdCronJob)
			if err != nil {
				Fail(fmt.Sprintf("Failed to get jobs: %v", err))
			}
			return count
		}, 3*time.Minute, 10*time.Second).Should(Equal(1))

		By("Cleaning up test resources")
		err = deleteCronJob(ctx, ctx.Namespace, cronJobName)
		Expect(err).NotTo(HaveOccurred(), "Failed to delete CronJob")
	})
	// It("cronjob API operations", func() {
	// 	ctx := e2eutil.InitTestContext(e2eutil.Options{})
	// 	defer e2eutil.CleanupTestContext(ctx)

	// 	cronJobName := "api-cronjob"

	// 	cronJob := createTestCronjob(cronJobName, ctx.Namespace, "* * * * *", v1alpha1.AllowConcurrent, normalCommand)

	// 	cronJob.Labels = map[string]string{
	// 		"mark-api-label": "unique-e2e-test-label",
	// 		"test-scenario":  "api-operations",
	// 	}

	// 	cronJob.Annotations = map[string]string{
	// 		"initial-annotation": "true",
	// 		"created-by":         "e2e-test",
	// 	}

	// 	By("create - create CronJob")
	// 	createdCronJob, err := createCronJob(ctx, ctx.Namespace, cronJob)
	// 	Expect(err).NotTo(HaveOccurred())
	// 	Expect(createdCronJob.Name).Should(Equal(cronJobName))
	// 	Expect(createdCronJob.Labels["mark-api-label"]).Should(Equal("unique-e2e-test-label"))
	// 	Expect(createdCronJob.Annotations["initial-annotation"]).Should(Equal("true"))

	// 	By("get - get single CronJob")
	// 	getcronJob, err := getCronJob(ctx, ctx.Namespace, cronJobName)
	// 	Expect(err).NotTo(HaveOccurred())
	// 	Expect(getcronJob.Name).Should(Equal(cronJobName))
	// 	Expect(getcronJob.UID).Should(Equal(createdCronJob.UID))
	// 	Expect(getcronJob.Spec.Schedule).Should(Equal("* * * * *"))
	// 	Expect(getcronJob.Spec.ConcurrencyPolicy).Should(Equal(v1alpha1.AllowConcurrent))

	// 	By("list - current ns")
	// 	listCronJob, err := ctx.Vcclient.BatchV1alpha1().CronJobs(ctx.Namespace).List(
	// 		context.TODO(), metav1.ListOptions{LabelSelector: "mark-api-label=unique-e2e-test-label"})
	// 	Expect(err).NotTo(HaveOccurred())
	// 	Expect(len(listCronJob.Items)).Should(Equal(1))
	// 	Expect(listCronJob.Items[0].Name).Should(Equal(cronJobName))

	// 	By("list all namespaces - all ns")
	// 	listCronJobAll, err := ctx.Vcclient.BatchV1alpha1().CronJobs("").List(
	// 		context.TODO(), metav1.ListOptions{LabelSelector: "mark-api-label=unique-e2e-test-label"})
	// 	Expect(err).NotTo(HaveOccurred())
	// 	Expect(len(listCronJobAll.Items)).Should(BeNumerically(">=", 1))

	// 	found := false
	// 	for _, cj := range listCronJobAll.Items {
	// 		if cj.Name == cronJobName && cj.Namespace == ctx.Namespace {
	// 			found = true
	// 			break
	// 		}
	// 	}
	// 	Expect(found).Should(BeTrue(), "Should find cronjob in all namespaces list")

	// 	By("update - update CronJob")
	// 	currentCronJob, err := getCronJob(ctx, ctx.Namespace, cronJobName)
	// 	Expect(err).NotTo(HaveOccurred())

	// 	if currentCronJob.Annotations == nil {
	// 		currentCronJob.Annotations = make(map[string]string)
	// 	}
	// 	currentCronJob.Annotations["updated-annotation"] = "true"
	// 	currentCronJob.Annotations["update-timestamp"] = time.Now().Format(time.RFC3339)
	// 	currentCronJob.Labels["updated-label"] = "true"

	// 	updateCronJob, err := ctx.Vcclient.BatchV1alpha1().CronJobs(ctx.Namespace).Update(
	// 		context.TODO(), currentCronJob, metav1.UpdateOptions{})
	// 	Expect(err).NotTo(HaveOccurred())
	// 	Expect(updateCronJob.Annotations["updated-annotation"]).Should(Equal("true"))
	// 	Expect(updateCronJob.Labels["updated-label"]).Should(Equal("true"))

	// 	By("patch - patch CronJob")
	// 	patchData := []byte(`{
	//     "metadata": {
	//         "annotations": {
	//             "patched-annotation": "true"
	//         },
	//         "labels": {
	//             "patched-label": "true"
	//         }
	//     },
	//     "spec": {
	//         "suspend": true
	//     }
	// }`)

	// 	patchedCronJob, err := ctx.Vcclient.BatchV1alpha1().CronJobs(ctx.Namespace).Patch(
	// 		context.TODO(),
	// 		cronJobName,
	// 		types.MergePatchType,
	// 		patchData,
	// 		metav1.PatchOptions{})
	// 	Expect(err).NotTo(HaveOccurred())
	// 	Expect(patchedCronJob.Annotations["patched-annotation"]).Should(Equal("true"))
	// 	Expect(patchedCronJob.Labels["patched-label"]).Should(Equal("true"))
	// 	Expect(patchedCronJob.Spec.Suspend).ShouldNot(BeNil())
	// 	Expect(*patchedCronJob.Spec.Suspend).Should(BeTrue())

	// 	By("watch - watch CronJob changes")
	// 	watchCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	// 	defer cancel()

	// 	watcher, err := ctx.Vcclient.BatchV1alpha1().CronJobs(ctx.Namespace).Watch(
	// 		watchCtx,
	// 		metav1.ListOptions{
	// 			LabelSelector:   "mark-api-label=unique-e2e-test-label",
	// 			ResourceVersion: patchedCronJob.ResourceVersion,
	// 		})
	// 	Expect(err).NotTo(HaveOccurred())
	// 	defer watcher.Stop()

	// 	go func() {
	// 		time.Sleep(2 * time.Second)
	// 		current, err := getCronJob(ctx, ctx.Namespace, cronJobName)
	// 		if err == nil {
	// 			current.Labels["watch-test"] = "triggered"
	// 			_, err := ctx.Vcclient.BatchV1alpha1().CronJobs(ctx.Namespace).Update(
	// 				context.TODO(), current, metav1.UpdateOptions{})
	// 			if err != nil {
	// 				Fail(fmt.Sprintf("Watch test update failed: %v", err))
	// 			}
	// 		}
	// 	}()

	// 	select {
	// 	case event, ok := <-watcher.ResultChan():
	// 		Expect(ok).Should(BeTrue(), "Watch channel should be open")
	// 		Expect(event.Type).Should(Equal(watch.Modified))
	// 		if cronJob, ok := event.Object.(*v1alpha1.CronJob); ok {
	// 			Expect(cronJob.Labels["watch-test"]).Should(Equal("triggered"))
	// 		}
	// 	case <-watchCtx.Done():
	// 		klog.Infof("Watch context timeout reached")
	// 	}

	// 	By("delete - delete CronJob")
	// 	err = deleteCronJob(ctx, ctx.Namespace, cronJobName)
	// 	Expect(err).NotTo(HaveOccurred(), "Failed to delete CronJob")

	// 	By("verify deletion")
	// 	Eventually(func() bool {
	// 		_, err := getCronJob(ctx, ctx.Namespace, cronJobName)
	// 		return errors.IsNotFound(err)
	// 	}, 30*time.Second, 2*time.Second).Should(BeTrue(), "CronJob should be deleted")
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

//	func getJobActiveNum(ctx *e2eutil.TestContext, cronJob *v1alpha1.CronJob) (int, error) {
//		jobs, err := getJobList(ctx, cronJob)
//		if err != nil {
//			return -1, err
//		}
//		activeCount := 0
//		for _, job := range jobs.Items {
//			if !isJobFinished(&job) {
//				activeCount++
//			}
//		}
//		return activeCount, nil
//	}
//
//	func getJobSucNum(ctx *e2eutil.TestContext, cronJob *v1alpha1.CronJob) (int, error) {
//		jobs, err := getJobList(ctx, cronJob)
//		if err != nil {
//			return -1, err
//		}
//		sucCount := 0
//		for _, job := range jobs.Items {
//			if job.Status.State.Phase == v1alpha1.Completed {
//				sucCount++
//			}
//		}
//		return sucCount, nil
//	}
func getJobFailNum(ctx *e2eutil.TestContext, cronJob *v1alpha1.CronJob) (int, error) {
	jobs, err := getJobList(ctx, cronJob)
	if err != nil {
		return -1, err
	}
	failCount := 0
	for _, job := range jobs.Items {
		if job.Status.State.Phase == v1alpha1.Failed {
			failCount++
		}
	}
	return failCount, nil
}

// func getActiveNum(ctx *e2eutil.TestContext, ns, name string) (int, error) {
// 	cronjob, err := getCronJob(ctx, ns, name)
// 	if err != nil {
// 		return -1, err
// 	}
// 	return len(cronjob.Status.Active), nil
// }

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
	jobList, err := ctx.Vcclient.BatchV1alpha1().Jobs(ctx.Namespace).List(
		context.TODO(), metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list jobs: %w", err)
	}

	var filteredJobs []v1alpha1.Job
	for _, job := range jobList.Items {
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
	return &v1alpha1.JobList{
		Items: filteredJobs,
	}, nil
}
