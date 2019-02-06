package engine

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/windmilleng/tilt/internal/testutils/output"

	"github.com/pkg/errors"
	"github.com/windmilleng/tilt/internal/ospath"

	"github.com/stretchr/testify/assert"
	"github.com/windmilleng/tilt/internal/model"
	"github.com/windmilleng/tilt/internal/store"
	"github.com/windmilleng/tilt/internal/watch"
)

func TestWatchManager_IgnoredLocalDirectories(t *testing.T) {
	f := newWMFixture(t)
	defer f.TearDown()

	target := model.DockerComposeTarget{Name: "foo"}.WithIgnoredLocalDirectories([]string{"bar"})
	target.Mounts = []model.Mount{{LocalPath: "."}}
	f.SetManifestTarget(target)

	f.ChangeFile(t, "bar/baz")

	actions := f.Stop(t)

	assert.NotContains(t, targetFilesChangedActionsToPaths(actions), "bar/baz")
}

func TestWatchManager_Dockerignore(t *testing.T) {
	f := newWMFixture(t)
	defer f.TearDown()

	target := model.DockerComposeTarget{Name: "foo"}.WithDockerignores([]model.Dockerignore{{LocalPath: ".", Contents: "bar"}})
	target.Mounts = []model.Mount{{LocalPath: "."}}
	f.SetManifestTarget(target)

	f.ChangeFile(t, "bar/baz")

	actions := f.Stop(t)

	assert.NotContains(t, targetFilesChangedActionsToPaths(actions), "bar/baz")
}

func TestWatchManager_Gitignore(t *testing.T) {
	f := newWMFixture(t)
	defer f.TearDown()

	wd, err := os.Getwd()
	if !assert.NoError(t, err) {
		return
	}

	target := model.DockerComposeTarget{Name: "foo"}.WithRepos([]model.LocalGitRepo{{LocalPath: wd, GitignoreContents: "bar"}})
	target.Mounts = []model.Mount{{LocalPath: "."}}
	f.SetManifestTarget(target)

	f.ChangeFile(t, "bar")

	actions := f.Stop(t)

	assert.NotContains(t, targetFilesChangedActionsToPaths(actions), "bar")
}

func TestWatchManager_WatchesReappliedOnDockerComposeMountsChange(t *testing.T) {
	f := newWMFixture(t)
	defer f.TearDown()

	target := model.DockerComposeTarget{Name: "foo"}
	target.Mounts = []model.Mount{{LocalPath: "."}}
	f.SetManifestTarget(target.WithIgnoredLocalDirectories([]string{"bar"}))
	f.SetManifestTarget(target)

	f.ChangeFile(t, "bar")
	f.ChangeFile(t, "stop")

	actions := f.Stop(t)

	// not asserting exact contents because we can end up with duplicates since the old watch loop isn't stopped
	// until after the new watch loop is started
	assert.Contains(t, targetFilesChangedActionsToPaths(actions), "bar")
}

func TestWatchManager_WatchesReappliedOnDockerIgnoreChange(t *testing.T) {
	f := newWMFixture(t)
	defer f.TearDown()

	target := model.DockerComposeTarget{Name: "foo"}
	target.Mounts = []model.Mount{{LocalPath: "."}}
	f.SetManifestTarget(target.WithDockerignores([]model.Dockerignore{{LocalPath: ".", Contents: "bar"}}))
	f.SetManifestTarget(target)

	f.ChangeFile(t, "bar")
	f.ChangeFile(t, "stop")

	actions := f.Stop(t)

	// not asserting exact contents because we can end up with duplicates since the old watch loop isn't stopped
	// until after the new watch loop is started
	assert.Contains(t, targetFilesChangedActionsToPaths(actions), "bar")
}

func TestWatchManager_WatchesReappliedOnGitIgnoreChange(t *testing.T) {
	f := newWMFixture(t)
	defer f.TearDown()

	wd, err := os.Getwd()
	if !assert.NoError(t, err) {
		return
	}

	target := model.DockerComposeTarget{Name: "foo"}
	target.Mounts = []model.Mount{{LocalPath: "."}}
	f.SetManifestTarget(target.WithRepos([]model.LocalGitRepo{{LocalPath: wd, GitignoreContents: "bar"}}))
	f.SetManifestTarget(target)

	f.ChangeFile(t, "bar")
	f.ChangeFile(t, "stop")

	actions := f.Stop(t)

	// not asserting exact contents because we can end up with duplicates since the old watch loop isn't stopped
	// until after the new watch loop is started
	assert.Contains(t, targetFilesChangedActionsToPaths(actions), "bar")
}

type wmFixture struct {
	ctx              context.Context
	cancel           func()
	actions          chan store.Action
	store            *store.Store
	wm               *WatchManager
	fakeMultiWatcher *fakeMultiWatcher
	fakeTimerMaker   fakeTimerMaker
}

func newWMFixture(t *testing.T) *wmFixture {
	actions := make(chan store.Action, 20)
	reducer := store.Reducer(func(ctx context.Context, s *store.EngineState, action store.Action) {
		select {
		case actions <- action:
		case <-time.After(100 * time.Millisecond):
			panic("would block when writing to action chan. perhaps too many actions or too small a buffer")
		}
	})
	st := store.NewStore(reducer, false)
	timerMaker := makeFakeTimerMaker(t)
	fakeMultiWatcher := newFakeMultiWatcher()
	wm := NewWatchManager(fakeMultiWatcher.newSub, timerMaker.maker())

	ctx, cancel := context.WithCancel(output.CtxForTest())
	go func() {
		err := st.Loop(ctx)
		if err != nil && err != context.Canceled {
			panic(fmt.Sprintf("st.Loop exited with error: %+v", err))
		}
	}()

	return &wmFixture{
		ctx:              ctx,
		cancel:           cancel,
		actions:          actions,
		store:            st,
		wm:               wm,
		fakeMultiWatcher: fakeMultiWatcher,
		fakeTimerMaker:   timerMaker,
	}
}

func (f *wmFixture) TearDown() {
	f.cancel()
}

func (f *wmFixture) ChangeFile(t *testing.T, path string) {
	select {
	case f.fakeMultiWatcher.events <- watch.FileEvent{Path: path}:
	default:
		t.Fatal("emitting a FileEvent would block. Perhaps there are too many events or the buffer size is too small.")
	}
}

func (f *wmFixture) ReadActionsUntil(lastFile string) ([]targetFilesChangedAction, error) {
	wd, err := os.Getwd()
	if err != nil {
		return nil, errors.Wrap(err, "error getting wd")
	}
	if relPath, ok := ospath.Child(wd, lastFile); ok {
		lastFile = relPath
	}

	var actions []targetFilesChangedAction
	for {
		select {
		case a := <-f.actions:
			tfca, ok := a.(targetFilesChangedAction)
			if !ok {
				return nil, fmt.Errorf("expected action of type %T, got action of type %T: %v", targetFilesChangedAction{}, a, a)
			}

			// 1. unpack to one file per action, for deterministic inspection
			// 2. make paths relative to cwd
			for _, absPath := range tfca.files {
				p := absPath
				if relPath, ok := ospath.Child(wd, absPath); ok {
					p = relPath
				}
				actions = append(actions, targetFilesChangedAction{tfca.targetID, []string{p}})
				if p == lastFile {
					return actions, nil
				}
			}
		case <-time.After(1 * time.Second):
			return nil, fmt.Errorf("timed out waiting for actions. received so far: %v", actions)
		}
	}
}

func (f *wmFixture) Stop(t *testing.T) []targetFilesChangedAction {
	f.ChangeFile(t, "stop")

	actions, err := f.ReadActionsUntil("stop")
	if err != nil {
		fmt.Printf("This is due to something funky in the test itself, not the code being tested.\n")
		t.Fatal(err)
	}
	return actions
}

func (f *wmFixture) SetManifestTarget(target model.DockerComposeTarget) {
	m := model.Manifest{Name: "foo"}.WithDeployTarget(target)
	mt := store.ManifestTarget{Manifest: m}
	state := f.store.LockMutableStateForTesting()
	state.UpsertManifestTarget(&mt)
	state.WatchMounts = true
	f.store.UnlockMutableState()
	f.wm.OnChange(f.ctx, f.store)
}

func targetFilesChangedActionsToPaths(actions []targetFilesChangedAction) []string {
	var paths []string
	for _, a := range actions {
		for _, p := range a.files {
			paths = append(paths, p)
		}
	}
	return paths
}
