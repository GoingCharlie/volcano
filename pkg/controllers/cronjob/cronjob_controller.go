package cronjob

import (
	v1 "k8s.io/api/core/v1"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/kubernetes"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	vcclientset "volcano.sh/apis/pkg/client/clientset/versioned"
	vcscheme "volcano.sh/apis/pkg/client/clientset/versioned/scheme"
	vcinformer "volcano.sh/apis/pkg/client/informers/externalversions"
	batchinformer "volcano.sh/apis/pkg/client/informers/externalversions/batch/v1alpha1"
	batchlister "volcano.sh/apis/pkg/client/listers/batch/v1alpha1"
	jobcache "volcano.sh/volcano/pkg/controllers/cache"
	"volcano.sh/volcano/pkg/controllers/framework"
	"volcano.sh/volcano/pkg/features"
)

func init() {
	framework.RegisterController(&cronjobcontroller{})
}

type cronjobcontroller struct {
	kubeClient kubernetes.Interface
	vcClient   vcclientset.Interface

	jobInformer     batchinformer.JobInformer
	cronJobInformer batchinformer.CronJobInformer

	vcInformerFactory vcinformer.SharedInformerFactory

	cronJobList batchlister.CronJobLister
	cronJobSync func() bool
	// A store of jobs
	jobLister batchlister.JobLister
	jobSynced func() bool

	queueList     []workqueue.TypedRateLimitingInterface[any]
	cache         jobcache.Cache
	recorder      record.EventRecorder
	workers       uint32
	maxRequeueNum int
}

func (cc *cronjobcontroller) Name() string { return "cronjob-controller" }

func (cc *cronjobcontroller) Initialize(opt *framework.ControllerOption) error {
	cc.kubeClient = opt.KubeClient
	cc.vcClient = opt.VolcanoClient

	workers := opt.WorkerNum
	// Initialize event client
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(klog.Infof)
	eventBroadcaster.StartRecordingToSink(&corev1.EventSinkImpl{Interface: cc.kubeClient.CoreV1().Events("")})
	recorder := eventBroadcaster.NewRecorder(vcscheme.Scheme, v1.EventSource{Component: "vc-controller-manager"})
	cc.cache = jobcache.New()
	cc.recorder = recorder
	cc.workers = workers
	cc.maxRequeueNum = opt.MaxRequeueNum
	if cc.maxRequeueNum < 0 {
		cc.maxRequeueNum = -1
	}
	var i uint32
	for i = 0; i < workers; i++ {
		cc.queueList[i] = workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[any]())
	}

	factory := opt.VCSharedInformerFactory
	cc.vcInformerFactory = factory
	if utilfeature.DefaultFeatureGate.Enabled(features.VolcanoJobSupport) {
		cc.jobInformer = factory.Batch().V1alpha1().Jobs()
		cc.jobInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc:    cc.addJob,
			UpdateFunc: cc.updateJob,
			DeleteFunc: cc.deleteJob,
		})
		cc.jobLister = cc.jobInformer.Lister()
		cc.jobSynced = cc.jobInformer.Informer().HasSynced

		cc.cronJobInformer = factory.Batch().V1alpha1().CronJobs()
		cc.cronJobList = cc.cronJobInformer.Lister()
		cc.cronJobSync = cc.cronJobInformer.Informer().HasSynced
		cc.cronJobInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				cc.enqueueController(obj)
			},
			UpdateFunc: func(oldObj interface{}, newObj interface{}) {
				cc.updateCronJob(oldObj, newObj)
			},
			DeleteFunc: func(obj interface{}) {
				cc.enqueueController(obj)
			},
		})
	}
	return nil
}
func (cc *cronjobcontroller) Run(stopCh <-chan struct{}) {}
