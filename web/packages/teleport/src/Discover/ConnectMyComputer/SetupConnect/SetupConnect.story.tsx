/**
 * Copyright 2023 Gravitational, Inc
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *      http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

import React, { useEffect } from 'react';

import {
  OverrideUserAgent,
  UserAgent,
} from 'shared/components/OverrideUserAgent';

import { ContextProvider } from 'teleport';
import cfg from 'teleport/config';
import { UserContext } from 'teleport/User/UserContext';
import { createTeleportContext } from 'teleport/mocks/contexts';
import { makeDefaultUserPreferences } from 'teleport/services/userPreferences/userPreferences';

import { SetupConnect } from './SetupConnect';

const { worker, rest } = window.msw;

export default {
  title: 'Teleport/Discover/ConnectMyComputer/SetupConnect',
  decorators: [
    Story => {
      worker.resetHandlers();
      return <Story />;
    },
  ],
};

const pollingWorker = () => {
  worker.use(
    rest.get(cfg.api.nodesPath, (req, res, ctx) => res(ctx.delay('infinite')))
  );
};

export const macOS = () => {
  pollingWorker();
  return (
    <OverrideUserAgent userAgent={UserAgent.macOS}>
      <Provider>
        <SetupConnect prevStep={() => {}} />
      </Provider>
    </OverrideUserAgent>
  );
};

export const Linux = () => {
  pollingWorker();
  return (
    <OverrideUserAgent userAgent={UserAgent.Linux}>
      <Provider>
        <SetupConnect prevStep={() => {}} />
      </Provider>
    </OverrideUserAgent>
  );
};

export const Polling = () => {
  pollingWorker();

  return (
    <Provider>
      <SetupConnect prevStep={() => {}} />
    </Provider>
  );
};

export const PollingSuccess = () => {
  worker.use(
    rest.get(cfg.api.nodesPath, (req, res, ctx) => {
      return res(ctx.json({ items: [{ id: '1234', hostname: 'foo' }] }));
    })
  );
  worker.use(
    rest.get(cfg.api.nodesPath, (req, res, ctx) => {
      return res.once(ctx.json({ items: [] }));
    })
  );

  return (
    <Provider>
      <SetupConnect prevStep={() => {}} />
    </Provider>
  );
};

// TODO: Polling Error
// TODO: Shorten the interval and timeouts.
// TODO: Hints.

const Provider = ({ children }) => {
  const ctx = createTeleportContext();
  ctx.storeUser.state.cluster.proxyVersion = '14.1.0';

  const preferences = makeDefaultUserPreferences();
  const updatePreferences = () => Promise.resolve();
  const getClusterPinnedResources = () => Promise.resolve([]);
  const updateClusterPinnedResources = () => Promise.resolve();

  return (
    <UserContext.Provider
      value={{
        preferences,
        updatePreferences,
        getClusterPinnedResources,
        updateClusterPinnedResources,
      }}
    >
      <ContextProvider ctx={ctx}>{children}</ContextProvider>
    </UserContext.Provider>
  );
};
