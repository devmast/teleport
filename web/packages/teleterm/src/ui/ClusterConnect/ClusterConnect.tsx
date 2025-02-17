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

import React, { useState } from 'react';

import Dialog from 'design/Dialog';

import { useAppContext } from 'teleterm/ui/appContextProvider';
import { DialogClusterConnect } from 'teleterm/ui/services/modals';
import { RootClusterUri } from 'teleterm/ui/uri';

import { ClusterAdd } from './ClusterAdd';
import { ClusterLogin } from './ClusterLogin';

export function ClusterConnect(props: { dialog: DialogClusterConnect }) {
  const [createdClusterUri, setCreatedClusterUri] = useState<
    RootClusterUri | undefined
  >();
  const { clustersService } = useAppContext();
  const clusterUri = props.dialog.clusterUri || createdClusterUri;

  function handleClusterAdd(clusterUri: RootClusterUri): void {
    const cluster = clustersService.findCluster(clusterUri);
    if (cluster?.connected) {
      props.dialog.onSuccess(clusterUri);
    } else {
      setCreatedClusterUri(clusterUri);
    }
  }

  return (
    <Dialog
      dialogCss={() => ({
        maxWidth: '480px',
        width: '100%',
        padding: '0',
      })}
      disableEscapeKeyDown={false}
      onClose={props.dialog.onCancel}
      open={true}
    >
      {!clusterUri ? (
        <ClusterAdd
          onCancel={props.dialog.onCancel}
          onSuccess={handleClusterAdd}
          prefill={{ clusterAddress: props.dialog.prefill?.clusterAddress }}
        />
      ) : (
        <ClusterLogin
          reason={props.dialog.reason}
          clusterUri={clusterUri}
          prefill={{ username: props.dialog.prefill?.username }}
          onCancel={props.dialog.onCancel}
          onSuccess={() => props.dialog.onSuccess(clusterUri)}
        />
      )}
    </Dialog>
  );
}
