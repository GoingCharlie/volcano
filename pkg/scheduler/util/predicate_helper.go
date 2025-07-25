/*
Copyright 2021 The Volcano Authors.

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
	"fmt"
	"sync"
	"sync/atomic"

	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	"volcano.sh/volcano/pkg/scheduler/api"
)

type PredicateHelper interface {
	PredicateNodes(task *api.TaskInfo, nodes []*api.NodeInfo, fn api.PredicateFn, enableErrorCache bool) ([]*api.NodeInfo, *api.FitErrors)
}

type predicateHelper struct {
	taskPredicateErrorCache map[string]map[string]error
}

// PredicateNodes returns the specified number of nodes that fit a task
func (ph *predicateHelper) PredicateNodes(task *api.TaskInfo, nodes []*api.NodeInfo, fn api.PredicateFn, enableErrorCache bool) ([]*api.NodeInfo, *api.FitErrors) {
	var errorLock sync.RWMutex
	fe := api.NewFitErrors()

	// don't enable error cache if task's TaskRole is empty, because different pods with empty TaskRole will all
	// have the same taskGroupID, and one pod predicate failed, all other pods will also be failed
	// see issue: https://github.com/volcano-sh/volcano/issues/3527
	if len(task.TaskRole) == 0 {
		enableErrorCache = false
	}

	allNodes := len(nodes)
	if allNodes == 0 {
		return make([]*api.NodeInfo, 0), fe
	}
	numNodesToFind := CalculateNumOfFeasibleNodesToFind(int32(allNodes))

	//allocate enough space to avoid growing it
	predicateNodes := make([]*api.NodeInfo, numNodesToFind)

	numFoundNodes := int32(0)
	processedNodes := int32(0)

	taskGroupid := taskGroupID(task)
	nodeErrorCache, taskFailedBefore := ph.taskPredicateErrorCache[taskGroupid]
	if nodeErrorCache == nil {
		nodeErrorCache = map[string]error{}
	}

	//create a context with cancellation
	ctx, cancel := context.WithCancel(context.Background())

	checkNode := func(index int) {
		// Check the nodes starting from where is left off in the previous scheduling cycle,
		// to make sure all nodes have the same chance of being examined across pods.
		node := nodes[(lastProcessedNodeIndex+index)%allNodes]
		atomic.AddInt32(&processedNodes, 1)
		klog.V(4).Infof("Considering Task <%v/%v> on node <%v>: <%v> vs. <%v>",
			task.Namespace, task.Name, node.Name, task.Resreq, node.Idle)

		// Check if the task had "predicate" failure before.
		// And then check if the task failed to predict on this node before.
		if enableErrorCache && taskFailedBefore {
			errorLock.RLock()
			errC, ok := nodeErrorCache[node.Name]
			errorLock.RUnlock()

			if ok {
				errorLock.Lock()
				fe.SetNodeError(node.Name, errC)
				errorLock.Unlock()
				return
			}
		}

		// TODO (k82cn): Enable eCache for performance improvement.
		if err := fn(task, node); err != nil {
			klog.V(3).Infof("Predicates failed: %v", err)
			errorLock.Lock()
			nodeErrorCache[node.Name] = err
			ph.taskPredicateErrorCache[taskGroupid] = nodeErrorCache
			fe.SetNodeError(node.Name, err)
			errorLock.Unlock()
			return
		}

		//check if the number of found nodes is more than the numNodesTofind
		length := atomic.AddInt32(&numFoundNodes, 1)
		if length > numNodesToFind {
			cancel()
			atomic.AddInt32(&numFoundNodes, -1)
		} else {
			predicateNodes[length-1] = node
		}
	}

	//workqueue.ParallelizeUntil(context.TODO(), 16, len(nodes), checkNode)
	workqueue.ParallelizeUntil(ctx, 16, allNodes, checkNode)

	//processedNodes := int(numFoundNodes) + len(filteredNodesStatuses) + len(failedPredicateMap)
	lastProcessedNodeIndex = (lastProcessedNodeIndex + int(processedNodes)) % allNodes
	predicateNodes = predicateNodes[:numFoundNodes]
	return predicateNodes, fe
}

func taskGroupID(task *api.TaskInfo) string {
	return fmt.Sprintf("%s/%s", task.Job, task.TaskRole)
}

func NewPredicateHelper() PredicateHelper {
	return &predicateHelper{taskPredicateErrorCache: map[string]map[string]error{}}
}
