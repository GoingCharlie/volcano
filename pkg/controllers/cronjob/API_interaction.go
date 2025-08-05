package cronjob

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	batchv1 "volcano.sh/apis/pkg/apis/batch/v1alpha1"
	vcclientset "volcano.sh/apis/pkg/client/clientset/versioned"
	volcanoclient "volcano.sh/apis/pkg/client/clientset/versioned/fake"
	batchv1alpha1 "volcano.sh/apis/pkg/client/clientset/versioned/typed/batch/v1alpha1"
	fakebatchv1alpha1 "volcano.sh/apis/pkg/client/clientset/versioned/typed/batch/v1alpha1/fake"
)

type testFakeClient struct {
	*volcanoclient.Clientset
}

func (c *testFakeClient) BatchV1alpha1() batchv1alpha1.BatchV1alpha1Interface {
	realFake := c.Clientset.BatchV1alpha1().(*fakebatchv1alpha1.FakeBatchV1alpha1)
	return &testBatchV1alpha1{
		FakeBatchV1alpha1: realFake,
	}
}

type testBatchV1alpha1 struct {
	*fakebatchv1alpha1.FakeBatchV1alpha1
}

func (c *testBatchV1alpha1) Jobs(namespace string) batchv1alpha1.JobInterface {
	realJobInterface := c.FakeBatchV1alpha1.Jobs(namespace)
	return &testJobInterface{
		realJobInterface,
	}
}

type testJobInterface struct {
	batchv1alpha1.JobInterface
}

func (c *testJobInterface) Create(ctx context.Context, job *batchv1.Job, opts metav1.CreateOptions) (*batchv1.Job, error) {
	job.TypeMeta = metav1.TypeMeta{
		Kind:       "Job",
		APIVersion: "batch.volcano.sh/v1alpha1",
	}
	return c.JobInterface.Create(ctx, job, opts)
}

func getCronJobClient(vcClient vcclientset.Interface, namespace string, name string) (*batchv1.CronJob, error) {
	cronJob, err := vcClient.BatchV1alpha1().CronJobs(namespace).Get(context.TODO(),
		name,
		metav1.GetOptions{})
	return cronJob, err
}
func getJobClient(vcClient vcclientset.Interface, namespace string, name string) (*batchv1.Job, error) {
	job, err := vcClient.BatchV1alpha1().Jobs(namespace).Get(context.TODO(),
		name,
		metav1.GetOptions{})
	return job, err
}
func deleteJobClient(vcClient vcclientset.Interface, namespace string, name string) error {
	err := vcClient.BatchV1alpha1().Jobs(namespace).Delete(context.TODO(),
		name,
		metav1.DeleteOptions{})
	return err
}
func createJobClient(vcClient vcclientset.Interface, namespace string, job *batchv1.Job) (*batchv1.Job, error) {
	newJob, err := vcClient.BatchV1alpha1().Jobs(namespace).Create(context.TODO(),
		job,
		metav1.CreateOptions{})
	return newJob, err
}
