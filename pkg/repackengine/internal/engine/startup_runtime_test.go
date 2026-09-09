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
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"k8s.io/client-go/rest"

	schedoptions "volcano.sh/volcano/cmd/scheduler/app/options"

	_ "volcano.sh/volcano/pkg/repackengine/actions/repack"
	engineconf "volcano.sh/volcano/pkg/repackengine/conf"
	_ "volcano.sh/volcano/pkg/repackengine/plugins/binpack"
	_ "volcano.sh/volcano/pkg/repackengine/plugins/gangdisruption"
	_ "volcano.sh/volcano/pkg/repackengine/plugins/networktopologyaware"
	_ "volcano.sh/volcano/pkg/repackengine/plugins/nodeconsolidation"
	_ "volcano.sh/volcano/pkg/repackengine/plugins/pdbconstraint"
	_ "volcano.sh/volcano/pkg/repackengine/plugins/repackbudget"
	_ "volcano.sh/volcano/pkg/repackengine/plugins/workloaddisruption"
	_ "volcano.sh/volcano/pkg/repackengine/plugins/workloadscope"
	_ "volcano.sh/volcano/pkg/scheduler/actions"
)

// fakeConfig is a non-nil rest.Config that never connects — client construction
// is lazy, so NewEngine can be built offline (no cluster required).
func fakeConfig() *rest.Config { return &rest.Config{Host: "https://127.0.0.1:6443"} }

// NewEngine applies its defaults so an operator can start the engine with an
// empty Config and still get a working engine.
func TestNewEngineAppliesDefaults(t *testing.T) {
	e, err := NewEngine(fakeConfig(), Config{})
	if err != nil {
		t.Fatalf("NewEngine() error = %v", err)
	}
	if e == nil {
		t.Fatal("NewEngine() returned nil engine")
	}
	if len(e.config.Plugins) == 0 {
		t.Error("default Plugins should be non-empty")
	}
	wantPlugins := []string{"workloadscope", "pdbconstraint", "repackbudget", "nodeconsolidation", "networktopologyaware", "workloaddisruption", "gangdisruption", "binpack"}
	if got := configuredPluginNames(e.config.Plugins); !reflect.DeepEqual(got, wantPlugins) {
		t.Errorf("default Plugins=%v, want %v", got, wantPlugins)
	}
	if e.config.ExecutionTimeout != 10*time.Minute {
		t.Errorf("default ExecutionTimeout = %v, want 10m", e.config.ExecutionTimeout)
	}
}

func TestLoadConfRejectsInvalidPluginWeight(t *testing.T) {
	directory := t.TempDir()
	schedulerConf := filepath.Join(directory, "scheduler.conf")
	repackConf := filepath.Join(directory, "repack.conf")
	if err := os.WriteFile(schedulerConf, []byte(`actions: "enqueue"`), 0o600); err != nil {
		t.Fatalf("write scheduler config: %v", err)
	}
	if err := os.WriteFile(repackConf, []byte(`
actions: "repack"
plugins:
  - name: workloadscope
  - name: repackbudget
  - name: workloaddisruption
    arguments:
      movedPodsWeight: 0.1
`), 0o600); err != nil {
		t.Fatalf("write repack config: %v", err)
	}

	engine := &Engine{config: Config{
		SchedulerConf: schedulerConf,
		RepackConf:    repackConf,
		Plugins:       engineconf.DefaultPluginOptions(),
	}}
	err := engine.loadConf()
	if err == nil || !strings.Contains(err.Error(), "movedPodsWeight") {
		t.Fatalf("loadConf error=%v, want fail-fast invalid weight rejection", err)
	}
}

func TestReloadConfPublishesBothFilesAtomically(t *testing.T) {
	directory := t.TempDir()
	schedulerConf := filepath.Join(directory, "scheduler.conf")
	repackConf := filepath.Join(directory, "repack.conf")
	writeTestConfig(t, schedulerConf, `
actions: "enqueue"
tiers:
  - plugins:
      - name: predicates
`)
	writeTestConfig(t, repackConf, `
actions: "repack"
plugins:
  - name: nodeconsolidation
`)
	base := Config{SchedulerConf: schedulerConf, RepackConf: repackConf}
	engineconf.ApplyDefaults(&base)
	engine := &Engine{config: base}

	changed, err := engine.reloadConf()
	if err != nil || !changed {
		t.Fatalf("initial reloadConf() changed=%t error=%v, want a published configuration", changed, err)
	}
	before := engine.currentRuntimeConfiguration()
	if before.generation != 1 || len(before.tiers) != 1 || before.tiers[0].Plugins[0].Name != "predicates" {
		t.Fatalf("initial scheduler configuration=%+v generation=%d", before.tiers, before.generation)
	}

	// Change the scheduler file at the same time as the Repack file becomes
	// invalid. Neither half may be published.
	writeTestConfig(t, schedulerConf, `
actions: "enqueue"
tiers:
  - plugins:
      - name: nodeorder
`)
	writeTestConfig(t, repackConf, `
actions: "repack"
plugins:
  - name: nodeconsolidation
  - name: workloaddisruption
    arguments:
      movedPodsWeight: 0.1
`)
	changed, err = engine.reloadConf()
	if err == nil || changed {
		t.Fatalf("invalid reloadConf() changed=%t error=%v, want rejection", changed, err)
	}
	afterRejected := engine.currentRuntimeConfiguration()
	if afterRejected != before {
		t.Fatal("rejected reload replaced the last-known-good configuration pointer")
	}
	if afterRejected.tiers[0].Plugins[0].Name != "predicates" {
		t.Fatalf("scheduler half was partially published: %+v", afterRejected.tiers)
	}

	writeTestConfig(t, repackConf, `
actions: "repack"
plugins:
  - name: nodeconsolidation
  - name: binpack
`)
	changed, err = engine.reloadConf()
	if err != nil || !changed {
		t.Fatalf("repaired reloadConf() changed=%t error=%v", changed, err)
	}
	afterRepair := engine.currentRuntimeConfiguration()
	if afterRepair.generation != 2 || afterRepair.tiers[0].Plugins[0].Name != "nodeorder" {
		t.Fatalf("repaired scheduler configuration=%+v generation=%d", afterRepair.tiers, afterRepair.generation)
	}
	if got := configuredPluginNames(afterRepair.plugins); !reflect.DeepEqual(got, []string{"nodeconsolidation", "binpack"}) {
		t.Fatalf("repaired Repack plugins=%v", got)
	}

	changed, err = engine.reloadConf()
	if err != nil || changed {
		t.Fatalf("no-op reloadConf() changed=%t error=%v, want unchanged", changed, err)
	}
	if current := engine.currentRuntimeConfiguration(); current != afterRepair || current.generation != 2 {
		t.Fatalf("no-op reload replaced configuration: pointerChanged=%t generation=%d", current != afterRepair, current.generation)
	}
}

func TestReloadConfRestoresBaselineWhenFilePluginsAreRemoved(t *testing.T) {
	directory := t.TempDir()
	schedulerConf := filepath.Join(directory, "scheduler.conf")
	repackConf := filepath.Join(directory, "repack.conf")
	writeTestConfig(t, schedulerConf, `actions: "enqueue"`)
	writeTestConfig(t, repackConf, `
actions: "repack"
plugins:
  - name: nodeconsolidation
  - name: binpack
`)
	base := Config{SchedulerConf: schedulerConf, RepackConf: repackConf}
	engineconf.ApplyDefaults(&base)
	engine := &Engine{config: base}
	if _, err := engine.reloadConf(); err != nil {
		t.Fatalf("initial reloadConf(): %v", err)
	}

	writeTestConfig(t, repackConf, `actions: "repack"`)
	if changed, err := engine.reloadConf(); err != nil || !changed {
		t.Fatalf("reload after removing plugins changed=%t error=%v", changed, err)
	}
	want := configuredPluginNames(engineconf.DefaultPluginOptions())
	if got := configuredPluginNames(engine.currentRuntimeConfiguration().plugins); !reflect.DeepEqual(got, want) {
		t.Fatalf("plugins after removing file value=%v, want baseline defaults %v", got, want)
	}
}

func TestReloadConfPreservesExplicitOverrides(t *testing.T) {
	directory := t.TempDir()
	schedulerConf := filepath.Join(directory, "scheduler.conf")
	repackConf := filepath.Join(directory, "repack.conf")
	writeTestConfig(t, schedulerConf, `actions: "enqueue"`)
	writeTestConfig(t, repackConf, `
actions: "ignored-action"
plugins:
  - name: binpack
`)
	base := Config{
		SchedulerConf: schedulerConf,
		RepackConf:    repackConf,
		Actions:       []string{"repack"},
		Plugins:       engineconf.PluginOptions([]string{"nodeconsolidation"}),
	}
	engine := &Engine{config: base, actionsExplicit: true, pluginsExplicit: true}
	if changed, err := engine.reloadConf(); err != nil || !changed {
		t.Fatalf("reloadConf() changed=%t error=%v", changed, err)
	}
	configuration := engine.currentRuntimeConfiguration()
	if !reflect.DeepEqual(configuration.actions, []string{"repack"}) {
		t.Fatalf("explicit actions overridden by file: %v", configuration.actions)
	}
	if got := configuredPluginNames(configuration.plugins); !reflect.DeepEqual(got, []string{"nodeconsolidation"}) {
		t.Fatalf("explicit plugins overridden by file: %v", got)
	}
}

func TestSchedulerAndRepackWatchersReloadConfiguration(t *testing.T) {
	directory := t.TempDir()
	schedulerConf := filepath.Join(directory, "scheduler.conf")
	repackConf := filepath.Join(directory, "repack.conf")
	writeTestConfig(t, schedulerConf, `
actions: "enqueue"
tiers:
  - plugins:
      - name: predicates
`)
	writeTestConfig(t, repackConf, `
actions: "repack"
plugins:
  - name: nodeconsolidation
`)
	base := Config{SchedulerConf: schedulerConf, RepackConf: repackConf}
	engineconf.ApplyDefaults(&base)
	engine := &Engine{config: base}
	if _, err := engine.reloadConf(); err != nil {
		t.Fatalf("initial reloadConf(): %v", err)
	}

	schedulerWatcher := newFakeConfigWatcher()
	repackWatcher := newFakeConfigWatcher()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		schedulerWatcher.Close()
		repackWatcher.Close()
	})
	go engine.watchConfig(ctx, configFileWatcher{source: "scheduler", path: schedulerConf, watcher: schedulerWatcher})
	go engine.watchConfig(ctx, configFileWatcher{source: "repack", path: repackConf, watcher: repackWatcher})

	writeTestConfig(t, schedulerConf, `
actions: "enqueue"
tiers:
  - plugins:
      - name: nodeorder
`)
	schedulerWatcher.events <- fsnotify.Event{Name: schedulerConf, Op: fsnotify.Write}
	waitForConfigGeneration(t, engine, 2)
	if got := engine.currentRuntimeConfiguration().tiers[0].Plugins[0].Name; got != "nodeorder" {
		t.Fatalf("scheduler watcher loaded plugin %q, want nodeorder", got)
	}

	writeTestConfig(t, repackConf, `
actions: "repack"
plugins:
  - name: nodeconsolidation
  - name: binpack
`)
	repackWatcher.events <- fsnotify.Event{Name: repackConf, Op: fsnotify.Create}
	waitForConfigGeneration(t, engine, 3)
	if got := configuredPluginNames(engine.currentRuntimeConfiguration().plugins); !reflect.DeepEqual(got, []string{"nodeconsolidation", "binpack"}) {
		t.Fatalf("Repack watcher loaded plugins %v", got)
	}
}

func TestNewConfigFileWatchersWatchesBothMountDirectories(t *testing.T) {
	schedulerDirectory := t.TempDir()
	repackDirectory := t.TempDir()
	watchers, err := newConfigFileWatchers(Config{
		SchedulerConf: filepath.Join(schedulerDirectory, "scheduler.conf"),
		RepackConf:    filepath.Join(repackDirectory, "repack.conf"),
	})
	if err != nil {
		t.Fatalf("newConfigFileWatchers(): %v", err)
	}
	defer func() {
		for _, watched := range watchers {
			watched.watcher.Close()
		}
	}()
	if len(watchers) != 2 {
		t.Fatalf("watcher count=%d, want scheduler and Repack watchers", len(watchers))
	}
	if watchers[0].source != "scheduler" || watchers[1].source != "repack" {
		t.Fatalf("watcher sources=%q,%q, want scheduler,repack", watchers[0].source, watchers[1].source)
	}
}

type fakeConfigWatcher struct {
	events    chan fsnotify.Event
	errors    chan error
	closeOnce sync.Once
}

func newFakeConfigWatcher() *fakeConfigWatcher {
	return &fakeConfigWatcher{events: make(chan fsnotify.Event, 1), errors: make(chan error, 1)}
}

func (w *fakeConfigWatcher) Events() chan fsnotify.Event { return w.events }
func (w *fakeConfigWatcher) Errors() chan error          { return w.errors }
func (w *fakeConfigWatcher) Close() {
	w.closeOnce.Do(func() {
		close(w.events)
		close(w.errors)
	})
}

func writeTestConfig(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write %s: %v", filepath.Base(path), err)
	}
}

func waitForConfigGeneration(t *testing.T, engine *Engine, generation uint64) {
	t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-deadline.C:
			t.Fatalf("timed out waiting for configuration generation %d; current=%d", generation, engine.currentRuntimeConfiguration().generation)
		case <-ticker.C:
			if engine.currentRuntimeConfiguration().generation >= generation {
				return
			}
		}
	}
}

// The repack-engine binary never runs the scheduler's flag setup, so the global
// options.ServerOpts is nil. NewEngine must initialize it before building the
// scheduler cache, or reused scheduler code nil-derefs at startup.
func TestNewEngineInitializesServerOptsWhenNil(t *testing.T) {
	orig := schedoptions.ServerOpts
	t.Cleanup(func() { schedoptions.ServerOpts = orig })
	schedoptions.ServerOpts = nil

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("NewEngine panicked with a nil ServerOpts: %v", r)
		}
	}()

	if _, err := NewEngine(fakeConfig(), Config{}); err != nil {
		t.Fatalf("NewEngine() error = %v", err)
	}
	if schedoptions.ServerOpts == nil {
		t.Fatal("NewEngine did not initialize the global scheduler ServerOpts")
	}
}
