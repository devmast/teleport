// Copyright 2023 Gravitational, Inc
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package daemon

import (
	"context"
	"strings"
	"sync"

	"github.com/gravitational/trace"

	"github.com/gravitational/teleport/api/types"
	"github.com/gravitational/teleport/api/utils/retryutils"
	api "github.com/gravitational/teleport/gen/proto/go/teleport/lib/teleterm/v1"
	"github.com/gravitational/teleport/lib/defaults"
	"github.com/gravitational/teleport/lib/teleterm/clusters"
	"github.com/gravitational/teleport/lib/utils"
)

// UpdateHeadlessAuthenticationState updates a headless authentication state.
func (s *Service) UpdateHeadlessAuthenticationState(ctx context.Context, clusterURI, headlessID string, state api.HeadlessAuthenticationState) error {
	cluster, _, err := s.ResolveCluster(clusterURI)
	if err != nil {
		return trace.Wrap(err)
	}

	if err := cluster.UpdateHeadlessAuthenticationState(ctx, headlessID, types.HeadlessAuthenticationState(state)); err != nil {
		return trace.Wrap(err)
	}

	return nil
}

// StartHeadlessHandlers starts a headless watcher for the given cluster URI.
//
// If waitInit is true, this method will wait for the watcher to connect to the
// Auth Server and receive an OpInit event to indicate that the watcher is fully
// initialized and ready to catch headless events.
func (s *Service) StartHeadlessWatcher(uri string, waitInit bool) error {
	s.headlessWatcherClosersMu.Lock()
	defer s.headlessWatcherClosersMu.Unlock()

	cluster, _, err := s.ResolveCluster(uri)
	if err != nil {
		return trace.Wrap(err)
	}

	err = s.startHeadlessWatcher(cluster, waitInit)
	return trace.Wrap(err)
}

// StartHeadlessWatchers starts headless watchers for all connected clusters.
func (s *Service) StartHeadlessWatchers() error {
	s.headlessWatcherClosersMu.Lock()
	defer s.headlessWatcherClosersMu.Unlock()

	clusters, err := s.cfg.Storage.ReadAll()
	if err != nil {
		return trace.Wrap(err)
	}

	for _, c := range clusters {
		if c.Connected() {
			// Don't wait for the headless watcher to initialize as this could slow down startup.
			if err := s.startHeadlessWatcher(c, false /* waitInit */); err != nil {
				return trace.Wrap(err)
			}
		}
	}

	return nil
}

// startHeadlessWatcher starts a headless watcher for the given cluster.
//
// If waitInit is true, this method will wait for the watcher to connect to the
// Auth Server and receive an OpInit event to indicate that the watcher is fully
// initialized and ready to catch headless events.
func (s *Service) startHeadlessWatcher(cluster *clusters.Cluster, waitInit bool) error {
	// If there is already a watcher for this cluster, close and replace it.
	// This may occur after relogin, for example.
	if err := s.stopHeadlessWatcher(cluster.URI.String()); err != nil && !trace.IsNotFound(err) {
		return trace.Wrap(err)
	}

	maxBackoffDuration := defaults.MaxWatcherBackoff
	retry, err := retryutils.NewLinear(retryutils.LinearConfig{
		First:  utils.FullJitter(maxBackoffDuration / 10),
		Step:   maxBackoffDuration / 5,
		Max:    maxBackoffDuration,
		Jitter: retryutils.NewHalfJitter(),
		Clock:  s.cfg.Clock,
	})
	if err != nil {
		return trace.Wrap(err)
	}

	watchCtx, watchCancel := context.WithCancel(s.closeContext)
	s.headlessWatcherClosers[cluster.URI.String()] = watchCancel

	log := s.cfg.Log.WithField("cluster", cluster.URI.String())

	pendingRequests := make(map[string]context.CancelFunc)
	pendingRequestsMu := sync.Mutex{}

	cancelPendingRequest := func(name string) {
		pendingRequestsMu.Lock()
		defer pendingRequestsMu.Unlock()
		if cancel, ok := pendingRequests[name]; ok {
			cancel()
		}
	}

	addPendingRequest := func(name string, cancel context.CancelFunc) {
		pendingRequestsMu.Lock()
		defer pendingRequestsMu.Unlock()
		pendingRequests[name] = cancel
	}

	pendingWatcherInitialized := make(chan struct{})
	pendingWatcherInitializedOnce := sync.Once{}

	watch := func() error {
		pendingWatcher, closePendingWatcher, err := cluster.WatchPendingHeadlessAuthentications(watchCtx)
		if err != nil {
			return trace.Wrap(err)
		}
		defer closePendingWatcher()

		resolutionWatcher, closeResolutionWatcher, err := cluster.WatchHeadlessAuthentications(watchCtx)
		if err != nil {
			return trace.Wrap(err)
		}
		defer closeResolutionWatcher()

		// Wait for the pending watcher to finish initializing. the resolution watcher is not as critical,
		// so we skip waiting for it.

		select {
		case event := <-pendingWatcher.Events():
			if event.Type != types.OpInit {
				return trace.BadParameter("expected init event, got %v instead", event.Type)
			}
			pendingWatcherInitializedOnce.Do(func() { close(pendingWatcherInitialized) })
		case <-pendingWatcher.Done():
			return trace.Wrap(pendingWatcher.Error())
		case <-watchCtx.Done():
			return trace.Wrap(watchCtx.Err())
		}

		retry.Reset()

		for {
			select {
			case event := <-pendingWatcher.Events():
				// Ignore non-put events.
				if event.Type != types.OpPut {
					continue
				}

				ha, ok := event.Resource.(*types.HeadlessAuthentication)
				if !ok {
					return trace.Errorf("headless watcher returned an unexpected resource type %T", event.Resource)
				}

				// headless authentication requests will timeout after 3 minutes, so we can close the
				// Electron modal once this time is up.
				sendCtx, cancelSend := context.WithTimeout(s.closeContext, defaults.HeadlessLoginTimeout)

				// Add the pending request to the map so it is canceled early upon resolution.
				addPendingRequest(ha.GetName(), cancelSend)

				// Notify the Electron App of the pending headless authentication to handle resolution.
				// We do this in a goroutine so the watch loop can continue and cancel resolved requests.
				go func() {
					defer cancelSend()
					if err := s.sendPendingHeadlessAuthentication(sendCtx, ha, cluster.URI.String()); err != nil {
						if !strings.Contains(err.Error(), context.Canceled.Error()) && !strings.Contains(err.Error(), context.DeadlineExceeded.Error()) {
							log.WithError(err).Debug("sendPendingHeadlessAuthentication resulted in unexpected error.")
						}
					}
				}()
			case event := <-resolutionWatcher.Events():
				// Watch for pending headless authentications to be approved, denied, or deleted (canceled/timeout).
				switch event.Type {
				case types.OpPut:
					ha, ok := event.Resource.(*types.HeadlessAuthentication)
					if !ok {
						return trace.Errorf("headless watcher returned an unexpected resource type %T", event.Resource)
					}

					switch ha.State {
					case types.HeadlessAuthenticationState_HEADLESS_AUTHENTICATION_STATE_APPROVED, types.HeadlessAuthenticationState_HEADLESS_AUTHENTICATION_STATE_DENIED:
						cancelPendingRequest(ha.GetName())
					}
				case types.OpDelete:
					cancelPendingRequest(event.Resource.GetName())
				}
			case <-pendingWatcher.Done():
				return trace.Wrap(pendingWatcher.Error(), "pending watcher error")
			case <-resolutionWatcher.Done():
				return trace.Wrap(resolutionWatcher.Error(), "resolution watcher error")
			case <-watchCtx.Done():
				return nil
			}
		}
	}

	log.Debugf("Starting headless watch loop.")
	go func() {
		defer func() {
			s.headlessWatcherClosersMu.Lock()
			defer s.headlessWatcherClosersMu.Unlock()

			select {
			case <-watchCtx.Done():
				// watcher was canceled by an outside call to stopHeadlessWatcher.
			default:
				// watcher closed due to error or cluster disconnect.
				if err := s.stopHeadlessWatcher(cluster.URI.String()); err != nil {
					log.WithError(err).Debug("Failed to remove headless watcher.")
				}
			}
		}()

		for {
			if !cluster.Connected() {
				log.Debugf("Not connected to cluster. Returning from headless watch loop.")
				return
			}

			err := watch()
			if trace.IsNotImplemented(err) {
				// Don't retry watch if we are connecting to an old Auth Server.
				log.WithError(err).Debug("Headless watcher not supported.")
				return
			}

			startedWaiting := s.cfg.Clock.Now()
			select {
			case t := <-retry.After():
				log.WithError(err).Debugf("Restarting watch on error after waiting %v.", t.Sub(startedWaiting))
				retry.Inc()
			case <-watchCtx.Done():
				log.WithError(watchCtx.Err()).Debugf("Context closed with err. Returning from headless watch loop.")
				return
			}
		}
	}()

	if waitInit {
		select {
		case <-pendingWatcherInitialized:
		case <-watchCtx.Done():
			return trace.Wrap(watchCtx.Err())
		}
	}

	return nil
}

// sendPendingHeadlessAuthentication notifies the Electron App of a pending headless authentication.
func (s *Service) sendPendingHeadlessAuthentication(ctx context.Context, ha *types.HeadlessAuthentication, clusterURI string) error {
	req := &api.SendPendingHeadlessAuthenticationRequest{
		RootClusterUri:                 clusterURI,
		HeadlessAuthenticationId:       ha.GetName(),
		HeadlessAuthenticationClientIp: ha.ClientIpAddress,
	}

	if err := s.importantModalSemaphore.Acquire(ctx); err != nil {
		return trace.Wrap(err)
	}
	defer s.importantModalSemaphore.Release()

	_, err := s.tshdEventsClient.SendPendingHeadlessAuthentication(ctx, req)
	return trace.Wrap(err)
}

// StopHeadlessWatcher stops the headless watcher for the given cluster URI.
func (s *Service) StopHeadlessWatcher(uri string) error {
	s.headlessWatcherClosersMu.Lock()
	defer s.headlessWatcherClosersMu.Unlock()

	return trace.Wrap(s.stopHeadlessWatcher(uri))
}

// StopHeadlessWatchers stops all headless watchers.
func (s *Service) StopHeadlessWatchers() {
	s.headlessWatcherClosersMu.Lock()
	defer s.headlessWatcherClosersMu.Unlock()

	for uri := range s.headlessWatcherClosers {
		if err := s.stopHeadlessWatcher(uri); err != nil {
			s.cfg.Log.WithField("cluster", uri).WithError(err).Debug("Encountered unexpected error closing headless watcher")
		}
	}
}

func (s *Service) stopHeadlessWatcher(uri string) error {
	if _, ok := s.headlessWatcherClosers[uri]; !ok {
		return trace.NotFound("no headless watcher for cluster %v", uri)
	}

	s.headlessWatcherClosers[uri]()
	delete(s.headlessWatcherClosers, uri)
	return nil
}
