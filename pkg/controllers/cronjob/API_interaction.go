package cronjob

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	batchv1 "volcano.sh/apis/pkg/apis/batch/v1alpha1"
	vcclientset "volcano.sh/apis/pkg/client/clientset/versioned"
)

func getCronJobApi(vcClient vcclientset.Interface, namespace string, name string) (*batchv1.CronJob, error) {
	cronJob, err := vcClient.BatchV1alpha1().CronJobs(namespace).Get(context.TODO(),
		name,
		metav1.GetOptions{})
	return cronJob, err
}
func getJobApi(vcClient vcclientset.Interface, namespace string, name string) (*batchv1.Job, error) {
	job, err := vcClient.BatchV1alpha1().Jobs(namespace).Get(context.TODO(),
		name,
		metav1.GetOptions{})
	return job, err
}
func deleteJobApi(vcClient vcclientset.Interface, namespace string, name string) error {
	err := vcClient.BatchV1alpha1().Jobs(namespace).Delete(context.TODO(),
		name,
		metav1.DeleteOptions{})
	return err
}
func createJobApi(vcClient vcclientset.Interface, namespace string, job *batchv1.Job) (*batchv1.Job, error) {
	newJob, err := vcClient.BatchV1alpha1().Jobs(namespace).Create(context.TODO(),
		job,
		metav1.CreateOptions{})
	return newJob, err
}
