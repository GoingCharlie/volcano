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

package repack

import (
	"context"
	"fmt"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"gopkg.in/yaml.v2"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"

	engineconf "volcano.sh/volcano/pkg/repackengine/conf"
	e2eutil "volcano.sh/volcano/test/e2e/util"
)

const (
	repackConfigMapName       = "integration-repack-configmap"
	schedulerConfigMapName    = "integration-scheduler-configmap"
	repackConfigKey           = "repack-engine.conf"
	taintTolerationEnableKey  = "predicate.TaintTolerationEnable"
	repackEnginePodSelector   = "app=volcano-repack-engine"
	schedulerPodSelector      = "app=volcano-scheduler"
	configurationLogTailLines = int64(5000)
)

type configurationReloadCheckpoint struct {
	podName      string
	podUID       types.UID
	restartCount int32
	reloadCount  int
}

// configReloadCase guarantees that a changed ConfigMap is restored even when a
// spec fails. Both the forward change and the restoration wait until the engine
// reports that it published the new coherent configuration snapshot.
type configReloadCase struct {
	ctx          *e2eutil.TestContext
	configMap    *e2eutil.ConfigMapCase
	source       string
	needsRestore bool
}

func newConfigReloadCase(ctx *e2eutil.TestContext, configMapName, source string, refreshSelectors ...string) *configReloadCase {
	testCase := &configReloadCase{
		ctx:       ctx,
		configMap: e2eutil.NewConfigMapCase(repackSystemNamespace, configMapName).WithRefreshPodSelectors(refreshSelectors...),
		source:    source,
	}
	DeferCleanup(testCase.restore)
	return testCase
}

func (c *configReloadCase) change(modifier func(map[string]string) (bool, map[string]string)) {
	checkpoint := newConfigurationReloadCheckpoint(c.ctx, c.source)
	// Set this before ChangeBy so a failure after the API update still restores
	// the original data from ConfigMapCase's undo state.
	c.needsRestore = true
	Expect(c.configMap.ChangeBy(modifier)).To(Succeed())
	waitForConfigurationReload(c.ctx, c.source, checkpoint)
}

func (c *configReloadCase) restore() {
	if !c.needsRestore {
		return
	}
	checkpoint := newConfigurationReloadCheckpoint(c.ctx, c.source)
	Expect(c.configMap.UndoChanged()).To(Succeed())
	waitForConfigurationReload(c.ctx, c.source, checkpoint)
	c.needsRestore = false
}

func newConfigurationReloadCheckpoint(ctx *e2eutil.TestContext, source string) configurationReloadCheckpoint {
	pod, err := currentRepackEnginePod(ctx)
	Expect(err).NotTo(HaveOccurred())
	reloads, err := configurationReloadLogCount(ctx, pod.Name, source)
	Expect(err).NotTo(HaveOccurred())
	return configurationReloadCheckpoint{
		podName:      pod.Name,
		podUID:       pod.UID,
		restartCount: podRestartCount(pod),
		reloadCount:  reloads,
	}
}

func waitForConfigurationReload(ctx *e2eutil.TestContext, source string, checkpoint configurationReloadCheckpoint) {
	Eventually(func() bool {
		pod, err := currentRepackEnginePod(ctx)
		if err != nil || pod.Name != checkpoint.podName || pod.UID != checkpoint.podUID || podRestartCount(pod) != checkpoint.restartCount {
			return false
		}
		reloads, err := configurationReloadLogCount(ctx, pod.Name, source)
		return err == nil && reloads > checkpoint.reloadCount
	}, repackTimeout, repackPoll).Should(BeTrue(),
		"engine must load the %s configuration without restarting Pod %s", source, checkpoint.podName)

	pod, err := currentRepackEnginePod(ctx)
	Expect(err).NotTo(HaveOccurred())
	Expect(pod.Name).To(Equal(checkpoint.podName), "configuration reload must not replace the engine Pod")
	Expect(pod.UID).To(Equal(checkpoint.podUID), "configuration reload must not replace the engine Pod")
	Expect(podRestartCount(pod)).To(Equal(checkpoint.restartCount), "configuration reload must not restart the engine container")
}

func currentRepackEnginePod(ctx *e2eutil.TestContext) (*v1.Pod, error) {
	pods, err := ctx.Kubeclient.CoreV1().Pods(repackSystemNamespace).List(context.TODO(), metav1.ListOptions{
		LabelSelector: repackEnginePodSelector,
	})
	if err != nil {
		return nil, err
	}
	var running []*v1.Pod
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.DeletionTimestamp == nil && pod.Status.Phase == v1.PodRunning {
			running = append(running, pod)
		}
	}
	if len(running) != 1 {
		return nil, fmt.Errorf("expected one running repack engine Pod, got %d", len(running))
	}
	return running[0], nil
}

func podRestartCount(pod *v1.Pod) int32 {
	var count int32
	for _, status := range pod.Status.InitContainerStatuses {
		count += status.RestartCount
	}
	for _, status := range pod.Status.ContainerStatuses {
		count += status.RestartCount
	}
	return count
}

func configurationReloadLogCount(ctx *e2eutil.TestContext, podName, source string) (int, error) {
	tailLines := configurationLogTailLines
	raw, err := ctx.Kubeclient.CoreV1().Pods(repackSystemNamespace).GetLogs(
		podName, &v1.PodLogOptions{TailLines: &tailLines}).DoRaw(context.TODO())
	if err != nil {
		return 0, err
	}
	count := 0
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.Contains(line, "configuration reloaded") && strings.Contains(line, `source="`+source+`"`) {
			count++
		}
	}
	return count, nil
}

func removeRepackBudget(data map[string]string) (bool, map[string]string) {
	original, ok := data[repackConfigKey]
	Expect(ok).To(BeTrue(), "%s must contain %s", repackConfigMapName, repackConfigKey)
	configuration, err := engineconf.Decode([]byte(original))
	Expect(err).NotTo(HaveOccurred())

	plugins := configuration.Plugins[:0]
	removed := false
	for _, plugin := range configuration.Plugins {
		if plugin.Name == "repackbudget" {
			removed = true
			continue
		}
		plugins = append(plugins, plugin)
	}
	Expect(removed).To(BeTrue(), "the E2E repack configuration must enable repackbudget")
	configuration.Plugins = plugins
	updated, err := yaml.Marshal(configuration)
	Expect(err).NotTo(HaveOccurred())

	data[repackConfigKey] = string(updated)
	return true, map[string]string{repackConfigKey: original}
}

func disableSchedulerTaintToleration(data map[string]string) (bool, map[string]string) {
	return e2eutil.ModifySchedulerConfig(data, func(configuration *e2eutil.SchedulerConfiguration) bool {
		found := false
		changed := false
		for tierIndex := range configuration.Tiers {
			for pluginIndex := range configuration.Tiers[tierIndex].Plugins {
				plugin := &configuration.Tiers[tierIndex].Plugins[pluginIndex]
				if plugin.Name != "predicates" {
					continue
				}
				found = true
				if plugin.Arguments == nil {
					plugin.Arguments = make(map[string]string)
				}
				if enabled, configured := plugin.Arguments[taintTolerationEnableKey]; !configured || enabled != "false" {
					plugin.Arguments[taintTolerationEnableKey] = "false"
					changed = true
				}
			}
		}
		Expect(found).To(BeTrue(), "the E2E scheduler configuration must contain the predicates plugin")
		Expect(changed).To(BeTrue(), "the E2E scheduler configuration must initially enforce taint toleration")
		return changed
	})
}

func runDryRunWithResourceLimit(ctx *e2eutil.TestContext, name string, cards int64) *repackv1alpha1.RepackRun {
	run, err := newRun(name, repackv1alpha1.RepackModeDryRun).goal(npuResource).
		maxPerRun(&repackv1alpha1.MaxPerRun{
			Resources: v1.ResourceList{npuResource: *resource.NewQuantity(cards, resource.DecimalSI)},
		}).create(ctx)
	Expect(err).NotTo(HaveOccurred())
	return waitTerminal(ctx, run.Name)
}

func runDryRun(ctx *e2eutil.TestContext, name string) *repackv1alpha1.RepackRun {
	run, err := newRun(name, repackv1alpha1.RepackModeDryRun).goal(npuResource).create(ctx)
	Expect(err).NotTo(HaveOccurred())
	return waitTerminal(ctx, run.Name)
}

var _ = Describe("Repack ConfigMap hot reload", Serial, func() {
	var ctx *e2eutil.TestContext
	var nodes []string
	var tainted []string

	BeforeEach(func() {
		ctx = e2eutil.InitTestContext(e2eutil.Options{})
		nodes = npuFixture(ctx, 3)
		tainted = nil
	})
	AfterEach(func() {
		recordSpecFailureDiagnostics(ctx)
		e2eutil.CleanupTestContext(ctx)
		for _, node := range tainted {
			untaintNode(ctx, node)
		}
		for _, node := range nodes {
			clearNPU(ctx, node)
		}
	})

	It("reloads repack-engine.conf without restarting the engine", func() {
		occupyMovableVCJob(ctx, "reload-budget-a", nodes[0], 4)
		occupyMovableVCJob(ctx, "reload-budget-b", nodes[1], 2)

		blocked := runDryRunWithResourceLimit(ctx, "reload-budget-before", 1)
		Expect(blocked.Status.Phase).To(Equal(repackv1alpha1.RepackSucceeded))
		Expect(completeReason(blocked)).To(Equal("InsufficientImprovement"))
		Expect(blocked.Status.Plan.Moves).To(BeEmpty())

		configuration := newConfigReloadCase(ctx, repackConfigMapName, "repack", repackEnginePodSelector)
		configuration.change(removeRepackBudget)

		admitted := runDryRunWithResourceLimit(ctx, "reload-budget-after", 1)
		Expect(admitted.Status.Phase).To(Equal(repackv1alpha1.RepackSucceeded))
		Expect(completeReason(admitted)).To(Equal("RepackRecommended"))
		Expect(admitted.Status.Plan.Moves).To(HaveLen(1), "without repackbudget the one-card maxPerRun limit is not enforced")
	})

	It("reloads the shared scheduler configuration without restarting the engine", func() {
		// The workloads must be running before NoSchedule is applied. Their Job
		// templates remain movable, so TaintToleration is the only reason the two
		// occupied nodes cannot receive one another's gang.
		occupyMovableVCJob(ctx, "reload-predicate-a", nodes[0], 4)
		occupyMovableVCJob(ctx, "reload-predicate-b", nodes[1], 4)
		taintNode(ctx, nodes[0])
		taintNode(ctx, nodes[1])
		tainted = []string{nodes[0], nodes[1]}

		blocked := runDryRun(ctx, "reload-predicate-before")
		Expect(blocked.Status.Phase).To(Equal(repackv1alpha1.RepackSucceeded))
		Expect(completeReason(blocked)).To(Equal("InsufficientImprovement"))
		Expect(blocked.Status.Plan.Moves).To(BeEmpty())

		configuration := newConfigReloadCase(ctx, schedulerConfigMapName, "scheduler", schedulerPodSelector, repackEnginePodSelector)
		configuration.change(disableSchedulerTaintToleration)

		admitted := runDryRun(ctx, "reload-predicate-after")
		Expect(admitted.Status.Phase).To(Equal(repackv1alpha1.RepackSucceeded))
		Expect(completeReason(admitted)).To(Equal("RepackRecommended"))
		Expect(admitted.Status.Plan.Moves).NotTo(BeEmpty(), "disabling TaintToleration must make the tainted receiver feasible")
	})
})
