/*
Copyright 2022 The Volcano Authors.

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

package util

import (
	"context"
	"time"

	"github.com/onsi/gomega"
	"gopkg.in/yaml.v2"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
)

type ConfigMapCase struct {
	NameSpace string
	Name      string // configmap.name

	startTs             time.Time // start timestamp
	undoData            map[string]string
	ocm                 *v1.ConfigMap
	refreshPodSelectors []string
}

func NewConfigMapCase(ns, name string) *ConfigMapCase {
	return &ConfigMapCase{
		NameSpace: ns,
		Name:      name,

		undoData:            make(map[string]string),
		refreshPodSelectors: []string{"app=volcano-scheduler"},
	}
}

// WithRefreshPodSelectors overrides the Pods whose annotations are touched
// after a ConfigMap update. Updating Pod metadata makes kubelet refresh mounted
// ConfigMaps promptly. The default remains app=volcano-scheduler for backwards
// compatibility with existing scheduler E2E cases.
func (c *ConfigMapCase) WithRefreshPodSelectors(selectors ...string) *ConfigMapCase {
	c.refreshPodSelectors = append([]string(nil), selectors...)
	return c
}

// ChangeBy call fn and update configmap by changed
func (c *ConfigMapCase) ChangeBy(fn func(data map[string]string) (changed bool, changedBefore map[string]string)) error {
	if c.ocm == nil {
		cm, err := KubeClient.CoreV1().ConfigMaps(c.NameSpace).Get(context.TODO(), c.Name, metav1.GetOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		c.ocm = cm
	}
	if changed, changedBefore := fn(c.ocm.Data); changed {
		time.Sleep(time.Second) // wait last configmap-change done completely
		cm, err := KubeClient.CoreV1().ConfigMaps(c.NameSpace).Update(context.TODO(), c.ocm, metav1.UpdateOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		c.ocm, c.undoData = cm, changedBefore

		gomega.Expect(c.refreshMountedConfigMaps()).To(gomega.Succeed())
		c.startTs = time.Now()
	}
	return nil
}

// UndoChanged restore configmap if exist undoData
func (c *ConfigMapCase) UndoChanged() error {
	if len(c.undoData) == 0 {
		return nil
	}
	for filename, old := range c.undoData {
		c.ocm.Data[filename] = old
	}
	atLeast := time.Second // at least 1s wait between 2 configmap-change
	if dur := time.Now().Sub(c.startTs); dur < atLeast {
		time.Sleep(atLeast - dur)
	}
	cm, err := KubeClient.CoreV1().ConfigMaps(c.NameSpace).Update(context.TODO(), c.ocm, metav1.UpdateOptions{})
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	c.ocm = cm

	gomega.Expect(c.refreshMountedConfigMaps()).To(gomega.Succeed())
	return nil
}

func (c *ConfigMapCase) refreshMountedConfigMaps() error {
	refreshTimestamp := time.Now().Format("060102150405.000")
	for _, selector := range c.refreshPodSelectors {
		pods, err := KubeClient.CoreV1().Pods(c.NameSpace).List(context.TODO(), metav1.ListOptions{LabelSelector: selector})
		if err != nil {
			return err
		}
		for i := range pods.Items {
			podName := pods.Items[i].Name
			if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
				pod, err := KubeClient.CoreV1().Pods(c.NameSpace).Get(context.TODO(), podName, metav1.GetOptions{})
				if err != nil {
					return err
				}
				if pod.Annotations == nil {
					pod.Annotations = make(map[string]string)
				}
				pod.Annotations["refreshts"] = refreshTimestamp
				_, err = KubeClient.CoreV1().Pods(c.NameSpace).Update(context.TODO(), pod, metav1.UpdateOptions{})
				return err
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

func ModifySchedulerConfig(data map[string]string, modifier func(*SchedulerConfiguration) bool) (changed bool, changedBefore map[string]string) {
	vcScheConfStr, ok := data["volcano-scheduler-ci.conf"]
	gomega.Expect(ok).To(gomega.BeTrue())

	schedulerConf := &SchedulerConfiguration{}
	err := yaml.Unmarshal([]byte(vcScheConfStr), schedulerConf)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())

	changed = modifier(schedulerConf)
	if !changed {
		return false, nil
	}

	newVCScheConfBytes, err := yaml.Marshal(schedulerConf)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())

	changedBefore = make(map[string]string)
	changedBefore["volcano-scheduler-ci.conf"] = vcScheConfStr
	data["volcano-scheduler-ci.conf"] = string(newVCScheConfBytes)
	return
}

// SchedulerConfiguration defines the configuration of scheduler.
type SchedulerConfiguration struct {
	// Actions defines the actions list of scheduler in order
	Actions string `yaml:"actions"`
	// Tiers defines plugins in different tiers
	Tiers []Tier `yaml:"tiers,omitempty"`
	// Configurations is configuration for actions
	Configurations []Configuration `yaml:"configurations,omitempty"`
}

// Tier defines plugin tier
type Tier struct {
	Plugins []PluginOption `yaml:"plugins,omitempty"`
}

func (t Tier) ContainsPlugin(name string) bool {
	return t.GetPluginIdxOf(name) >= 0
}

func (t Tier) GetPluginIdxOf(name string) int {
	for idx, p := range t.Plugins {
		if p.Name == name {
			return idx
		}
	}
	return -1
}

// Configuration is configuration of action
type Configuration struct {
	// Name is name of action
	Name string `yaml:"name"`
	// Arguments defines the different arguments that can be given to specified action
	Arguments map[string]string `yaml:"arguments,omitempty"`
}

// PluginOption defines the options of plugin
type PluginOption struct {
	// The name of Plugin
	Name string `yaml:"name"`
	// EnabledJobOrder defines whether jobOrderFn is enabled
	EnabledJobOrder *bool `yaml:"enableJobOrder,omitempty"`
	// EnabledHierachy defines whether hierarchical sharing is enabled
	EnabledHierarchy *bool `yaml:"enableHierarchy,omitempty"`
	// EnabledJobReady defines whether jobReadyFn is enabled
	EnabledJobReady *bool `yaml:"enableJobReady,omitempty"`
	// EnabledJobPipelined defines whether jobPipelinedFn is enabled
	EnabledJobPipelined *bool `yaml:"enableJobPipelined,omitempty"`
	// EnabledTaskOrder defines whether taskOrderFn is enabled
	EnabledTaskOrder *bool `yaml:"enableTaskOrder,omitempty"`
	// EnabledPreemptable defines whether preemptableFn is enabled
	EnabledPreemptable *bool `yaml:"enablePreemptable,omitempty"`
	// EnabledReclaimable defines whether reclaimableFn is enabled
	EnabledReclaimable *bool `yaml:"enableReclaimable,omitempty"`
	// EnabledQueueOrder defines whether queueOrderFn is enabled
	EnabledQueueOrder *bool `yaml:"enableQueueOrder,omitempty"`
	// EnabledPredicate defines whether predicateFn is enabled
	EnabledClusterOrder *bool `yaml:"EnabledClusterOrder,omitempty"`
	// EnableClusterOrder defines whether clusterOrderFn is enabled
	EnabledPredicate *bool `yaml:"enablePredicate,omitempty"`
	// EnabledBestNode defines whether bestNodeFn is enabled
	EnabledBestNode *bool `yaml:"enableBestNode,omitempty"`
	// EnabledNodeOrder defines whether NodeOrderFn is enabled
	EnabledNodeOrder *bool `yaml:"enableNodeOrder,omitempty"`
	// EnabledTargetJob defines whether targetJobFn is enabled
	EnabledTargetJob *bool `yaml:"enableTargetJob,omitempty"`
	// EnabledReservedNodes defines whether reservedNodesFn is enabled
	EnabledReservedNodes *bool `yaml:"enableReservedNodes,omitempty"`
	// EnabledJobEnqueued defines whether jobEnqueuedFn is enabled
	EnabledJobEnqueued *bool `yaml:"enableJobEnqueued,omitempty"`
	// EnabledVictim defines whether victimsFn is enabled
	EnabledVictim *bool `yaml:"enabledVictim,omitempty"`
	// EnabledJobStarving defines whether jobStarvingFn is enabled
	EnabledJobStarving *bool `yaml:"enableJobStarving,omitempty"`
	// Arguments defines the different arguments that can be given to different plugins
	Arguments map[string]string `yaml:"arguments,omitempty"`
}
