package cronjob

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
	batchv1 "volcano.sh/apis/pkg/apis/batch/v1alpha1"
)

func TestCompatibility(t *testing.T) {
	// 创建一个自定义资源实例
	myCR := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-obj",
			Namespace: "default",
		},
	}

	// 验证可以获取 Key
	key, err := cache.MetaNamespaceKeyFunc(myCR)
	if err != nil {
		t.Fatalf("Failed: %v", err)
	}

	expected := "default/test-obj"
	if key != expected {
		t.Fatalf("Expected %q, got %q", expected, key)
	}
}
