//go:build linux

// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"
)

const (
	// systemInfoSyncDebounce is how long the syncer waits after the last
	// change under an actor's files before it pushes, so a burst of writes is
	// pushed once.
	systemInfoSyncDebounce = 200 * time.Millisecond
	// systemInfoRescanInterval is how often the syncer rescans every actor's
	// files whatever the watcher reports. It is the whole refresh when
	// fsnotify is unavailable, and it catches an event the watcher dropped.
	systemInfoRescanInterval = 30 * time.Second
	// systemInfoPushTimeout bounds one sync's pushes into a guest.
	systemInfoPushTimeout = 10 * time.Second
)

// systemInfoSyncer copies the system-info files atelet changes into the
// running CopyFile guests. One fsnotify watcher serves every actor on the
// worker, because each watcher costs an inotify instance and a node has few.
//
// It never takes the actor's lifecycle lock: a checkpoint holds that lock
// while it stops the actor's sync. The zero value is ready to use.
type systemInfoSyncer struct {
	// debounce, rescan and newWatcher override the defaults in tests.
	debounce   time.Duration
	rescan     time.Duration
	newWatcher func() (*fsnotify.Watcher, error)

	mu      sync.Mutex
	started bool
	// watcher is nil when fsnotify is unavailable.
	watcher *fsnotify.Watcher
	// actors is keyed by actor UID.
	actors map[string]*syncedActor
}

// syncedActor is one running guest the syncer pushes to.
type syncedActor struct {
	syncer *systemInfoSyncer
	uid    string
	root   string
	copier fileCopier

	stopped atomic.Bool
	// ctx ends when the actor is stopped, cancelling a push in flight.
	ctx    context.Context
	cancel context.CancelFunc

	timerMu sync.Mutex
	timer   *time.Timer

	// mu is held across a sync, so stopping waits for one in flight.
	mu sync.Mutex
	// hashes are the contents the guest has.
	hashes systemInfoHashes
	// watched are the directories under root added to the watcher.
	watched []string
}

// Start pushes changes to the files under root into the guest of actor uid
// through c, until Stop. hashes are the contents the guest already has; nil
// means unknown, so every file is pushed. It scans once straight away, for
// changes made since those were read.
func (s *systemInfoSyncer) Start(uid, root string, c fileCopier, hashes systemInfoHashes) {
	s.Stop(uid)
	if hashes == nil {
		hashes = systemInfoHashes{}
	}
	ctx, cancel := context.WithCancel(context.Background())
	a := &syncedActor{syncer: s, uid: uid, root: root, copier: c, hashes: hashes, ctx: ctx, cancel: cancel}
	s.mu.Lock()
	s.startLocked()
	s.actors[uid] = a
	s.mu.Unlock()
	a.schedule(0)
}

// Stop stops pushing to actor uid's guest. It cancels a push in flight and
// returns once it has ended, so the guest can be paused or torn down.
// Stopping an actor that isn't synced does nothing.
func (s *systemInfoSyncer) Stop(uid string) {
	s.mu.Lock()
	a := s.actors[uid]
	delete(s.actors, uid)
	w := s.watcher
	s.mu.Unlock()
	if a == nil {
		return
	}
	a.stopped.Store(true)
	a.cancel()
	a.timerMu.Lock()
	if a.timer != nil {
		a.timer.Stop()
	}
	a.timerMu.Unlock()

	a.mu.Lock()
	defer a.mu.Unlock()
	if w != nil {
		for _, dir := range a.watched {
			// The directory may already be gone, which drops its watch.
			_ = w.Remove(dir)
		}
	}
}

// startLocked creates the watcher and starts the loop serving it, once.
func (s *systemInfoSyncer) startLocked() {
	if s.started {
		return
	}
	s.started = true
	s.actors = map[string]*syncedActor{}
	newWatcher := s.newWatcher
	if newWatcher == nil {
		newWatcher = fsnotify.NewWatcher
	}
	w, err := newWatcher()
	if err != nil {
		slog.Warn("Could not create fsnotify watcher for system-info files; refreshing by rescanning",
			slog.Duration("interval", s.rescanInterval()), slog.Any("err", err))
	} else {
		s.watcher = w
	}
	go s.loop(w)
}

func (s *systemInfoSyncer) debounceDelay() time.Duration {
	if s.debounce > 0 {
		return s.debounce
	}
	return systemInfoSyncDebounce
}

func (s *systemInfoSyncer) rescanInterval() time.Duration {
	if s.rescan > 0 {
		return s.rescan
	}
	return systemInfoRescanInterval
}

// loop schedules syncs for the worker's lifetime: debounced on a change under
// an actor's files, and for every actor on each rescan and watcher error.
func (s *systemInfoSyncer) loop(w *fsnotify.Watcher) {
	var events <-chan fsnotify.Event
	var errs <-chan error
	if w != nil {
		events, errs = w.Events, w.Errors
	}
	ticker := time.NewTicker(s.rescanInterval())
	defer ticker.Stop()
	for {
		select {
		case ev := <-events:
			if a := s.actorFor(ev.Name); a != nil {
				a.schedule(s.debounceDelay())
			}
		case err := <-errs:
			slog.Warn("System-info file watch error; rescanning every actor", slog.Any("err", err))
			s.scheduleAll()
		case <-ticker.C:
			s.scheduleAll()
		}
	}
}

// actorFor returns the actor whose files include path, or nil.
func (s *systemInfoSyncer) actorFor(path string) *syncedActor {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.actors {
		if path == a.root || strings.HasPrefix(path, a.root+string(filepath.Separator)) {
			return a
		}
	}
	return nil
}

func (s *systemInfoSyncer) scheduleAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.actors {
		a.schedule(0)
	}
}

// schedule runs a sync after d, replacing one already scheduled.
func (a *syncedActor) schedule(d time.Duration) {
	a.timerMu.Lock()
	defer a.timerMu.Unlock()
	if a.stopped.Load() {
		return
	}
	if a.timer == nil {
		a.timer = time.AfterFunc(d, a.sync)
		return
	}
	a.timer.Reset(d)
}

// sync pushes every file whose contents differ from the guest's, and watches
// every directory under the actor's root. A file that fails to push keeps its
// old hash, so the next sync tries it again.
func (a *syncedActor) sync() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.stopped.Load() {
		return
	}
	a.watchDirs()
	files, err := readSystemInfoFiles(a.root)
	if err != nil {
		slog.Warn("Could not read system-info files to sync", slog.String("actorUID", a.uid), slog.Any("err", err))
		return
	}
	ctx, cancel := context.WithTimeout(a.ctx, systemInfoPushTimeout)
	defer cancel()
	copied, errs := pushChanged(ctx, a.copier, files, a.hashes)
	for _, rel := range copied {
		slog.Info("Copied a changed system-info file into the guest", slog.String("actorUID", a.uid), slog.String("file", rel))
	}
	if a.ctx.Err() != nil {
		// Stopped mid-push; the actor's guest is being paused or torn down.
		return
	}
	for _, err := range errs {
		if errors.Is(err, errPushCutShort) {
			slog.Warn("System-info sync timed out; the remaining files are copied on the next sync",
				slog.String("actorUID", a.uid), slog.Any("err", err))
			continue
		}
		slog.Warn("Could not copy a changed system-info file into the guest; will retry",
			slog.String("actorUID", a.uid), slog.Any("err", err))
	}
}

// watchDirs adds every directory under the actor's root to the watcher, so a
// volume or subdirectory created after Start is watched too.
func (a *syncedActor) watchDirs() {
	a.syncer.mu.Lock()
	w := a.syncer.watcher
	a.syncer.mu.Unlock()
	if w == nil {
		return
	}
	_ = filepath.WalkDir(a.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		if err := w.Add(p); err != nil {
			slog.Warn("Could not watch a system-info directory; relying on rescans",
				slog.String("actorUID", a.uid), slog.String("dir", p), slog.Any("err", err))
			return nil
		}
		if !slices.Contains(a.watched, p) {
			a.watched = append(a.watched, p)
		}
		return nil
	})
}
