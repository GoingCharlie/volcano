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

package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"

	"github.com/fsnotify/fsnotify"
	"k8s.io/klog/v2"

	"volcano.sh/volcano/pkg/filewatcher"
	engineconf "volcano.sh/volcano/pkg/repackengine/conf"
	engineframework "volcano.sh/volcano/pkg/repackengine/framework"
	"volcano.sh/volcano/pkg/scheduler"
	schedconf "volcano.sh/volcano/pkg/scheduler/conf"
)

// runtimeConfiguration is an immutable, coherent view of both configuration
// files. A reconcile captures one pointer and keeps using it for the whole
// cycle, while a reload publishes a new pointer for later reconciles.
type runtimeConfiguration struct {
	actions        []string
	plugins        []engineframework.PluginOption
	tiers          []schedconf.Tier
	configurations []schedconf.Configuration
	generation     uint64
}

type configFileWatcher struct {
	source  string
	path    string
	watcher filewatcher.FileWatcher
}

func configuredPluginNames(options []engineframework.PluginOption) []string {
	names := make([]string, 0, len(options))
	for _, option := range options {
		names = append(names, option.Name)
	}
	return names
}

// loadConf performs the required initial load. Runtime watcher callbacks use
// reloadConf directly so they can distinguish a semantic change from a no-op.
func (e *Engine) loadConf() error {
	_, err := e.reloadConf()
	return err
}

// reloadConf loads and validates both configuration files before publishing
// either one. A bad or transiently unreadable update therefore leaves the
// complete last-known-good configuration active.
func (e *Engine) reloadConf() (bool, error) {
	e.reloadConfigMutex.Lock()
	defer e.reloadConfigMutex.Unlock()

	next, err := e.buildRuntimeConfiguration()
	if err != nil {
		return false, err
	}

	e.runtimeConfigMutex.Lock()
	defer e.runtimeConfigMutex.Unlock()
	if sameRuntimeConfiguration(e.runtimeConfig, next) {
		return false, nil
	}
	if e.runtimeConfig != nil {
		next.generation = e.runtimeConfig.generation + 1
	} else {
		next.generation = 1
	}
	e.runtimeConfig = next
	return true, nil
}

func (e *Engine) buildRuntimeConfiguration() (*runtimeConfiguration, error) {
	if e.config.SchedulerConf == "" {
		return nil, fmt.Errorf("scheduler-conf is required")
	}
	raw, err := os.ReadFile(e.config.SchedulerConf)
	if err != nil {
		return nil, fmt.Errorf("read scheduler-conf: %w", err)
	}
	_, tiers, configurations, _, err := scheduler.UnmarshalSchedulerConf(string(raw))
	if err != nil {
		return nil, fmt.Errorf("decode scheduler-conf: %w", err)
	}

	// e.config is the immutable command-line/programmatic baseline. Rebuild from
	// it on every reload so removing a file value restores the baseline instead
	// of retaining a value from the previous file version.
	candidate := e.config
	candidate.Actions = append([]string(nil), e.config.Actions...)
	candidate.Plugins = append([]engineframework.PluginOption(nil), e.config.Plugins...)
	if e.config.RepackConf != "" {
		repackRaw, err := os.ReadFile(e.config.RepackConf)
		if err != nil {
			return nil, fmt.Errorf("read repack-conf: %w", err)
		}
		repackConfig, err := engineconf.Decode(repackRaw)
		if err != nil {
			return nil, fmt.Errorf("decode repack-conf: %w", err)
		}
		engineconf.ApplyFile(&candidate, repackConfig, e.actionsExplicit, e.pluginsExplicit)
	}
	if err := engineconf.ValidatePipeline(candidate.Actions, candidate.Plugins); err != nil {
		return nil, err
	}
	return &runtimeConfiguration{
		actions:        append([]string(nil), candidate.Actions...),
		plugins:        append([]engineframework.PluginOption(nil), candidate.Plugins...),
		tiers:          tiers,
		configurations: configurations,
	}, nil
}

func sameRuntimeConfiguration(current, next *runtimeConfiguration) bool {
	if current == nil || next == nil {
		return current == next
	}
	return reflect.DeepEqual(current.actions, next.actions) &&
		reflect.DeepEqual(current.plugins, next.plugins) &&
		reflect.DeepEqual(current.tiers, next.tiers) &&
		reflect.DeepEqual(current.configurations, next.configurations)
}

// currentRuntimeConfiguration returns an immutable pointer. Published
// configurations are never modified, so the read lock is only needed while
// copying the pointer.
func (e *Engine) currentRuntimeConfiguration() *runtimeConfiguration {
	e.runtimeConfigMutex.RLock()
	configuration := e.runtimeConfig
	e.runtimeConfigMutex.RUnlock()
	if configuration != nil {
		return configuration
	}
	// Production calls loadConf before reconciling. This fallback keeps direct
	// Engine unit tests usable without weakening the startup requirement.
	return &runtimeConfiguration{
		actions: append([]string(nil), e.config.Actions...),
		plugins: append([]engineframework.PluginOption(nil), e.config.Plugins...),
	}
}

func newConfigFileWatchers(config Config) ([]configFileWatcher, error) {
	sources := []struct {
		name string
		path string
	}{
		{name: "scheduler", path: config.SchedulerConf},
		{name: "repack", path: config.RepackConf},
	}
	watchers := make([]configFileWatcher, 0, len(sources))
	for _, source := range sources {
		if source.path == "" {
			continue
		}
		watcher, err := filewatcher.NewFileWatcher(filepath.Dir(source.path))
		if err != nil {
			for _, created := range watchers {
				created.watcher.Close()
			}
			return nil, fmt.Errorf("create %s config file watcher for %q: %w", source.name, source.path, err)
		}
		watchers = append(watchers, configFileWatcher{source: source.name, path: source.path, watcher: watcher})
	}
	return watchers, nil
}

func (e *Engine) watchConfig(ctx context.Context, watched configFileWatcher) {
	if watched.watcher == nil {
		return
	}
	for {
		select {
		case event, ok := <-watched.watcher.Events():
			if !ok {
				return
			}
			klog.V(4).InfoS("repack: config filesystem event", "source", watched.source,
				"path", watched.path, "event", event)
			if event.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename) == 0 {
				continue
			}
			changed, err := e.reloadConf()
			if err != nil {
				current := e.currentRuntimeConfiguration()
				klog.ErrorS(err, "repack: config reload rejected; keeping last-known-good configuration",
					"source", watched.source, "path", watched.path, "generation", current.generation)
				continue
			}
			if !changed {
				continue
			}
			current := e.currentRuntimeConfiguration()
			klog.InfoS("repack: configuration reloaded", "source", watched.source,
				"generation", current.generation, "actions", current.actions,
				"plugins", configuredPluginNames(current.plugins))
		case err, ok := <-watched.watcher.Errors():
			if !ok {
				return
			}
			klog.ErrorS(err, "repack: config watcher error", "source", watched.source, "path", watched.path)
		case <-ctx.Done():
			return
		}
	}
}

func (e *Engine) closeConfigWatchers() {
	for _, watched := range e.configWatchers {
		if watched.watcher != nil {
			watched.watcher.Close()
		}
	}
}
