package syncclient

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/gravitational/teleport/lib/tbot/config"
	"github.com/gravitational/teleport/lib/tbot/identity"
	"github.com/stretchr/testify/require"
)

type mockClientBuilder struct {
	counter atomic.Int32
}

func (m *mockClientBuilder) buildClient(_ context.Context) (*SyncClient, error) {
	m.counter.Add(1)
	return NewSyncClient(nil), nil
}

func (m *mockClientBuilder) countClientBuild() int {
	count := m.counter.Load()
	count32 := int(count)
	return count32
}

func TestBot_GetClient(t *testing.T) {
	ctx := context.Background()

	cert1 := []byte("cert1")
	cert2 := []byte("cert2")

	tests := []struct {
		name                 string
		currentCert          []byte
		cachedCert           []byte
		cachedClient         *SyncClient
		expectNewClientBuild require.BoolAssertionFunc
		assertError          require.ErrorAssertionFunc
	}{
		{
			name:                 "no cert yet",
			currentCert:          nil,
			cachedCert:           nil,
			cachedClient:         nil,
			expectNewClientBuild: require.False,
			assertError:          require.Error,
		},
		{
			name:                 "cert but no cache",
			currentCert:          cert1,
			cachedCert:           nil,
			cachedClient:         nil,
			expectNewClientBuild: require.True,
			assertError:          require.NoError,
		},
		{
			name:                 "cert and fresh cache",
			currentCert:          cert1,
			cachedCert:           cert1,
			cachedClient:         NewSyncClient(nil),
			expectNewClientBuild: require.False,
			assertError:          require.NoError,
		},
		{
			name:                 "cert and stale cache",
			currentCert:          cert2,
			cachedCert:           cert1,
			cachedClient:         NewSyncClient(nil),
			expectNewClientBuild: require.True,
			assertError:          require.NoError,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := mockClientBuilder{}
			destination := &config.DestinationMemory{}
			require.NoError(t, destination.CheckAndSetDefaults())
			require.NoError(t, destination.Write(ctx, identity.TLSCertKey, tt.currentCert))
			c := Cache{
				cachedCert:    tt.cachedCert,
				cachedClient:  tt.cachedClient,
				clientBuilder: mock.buildClient,
				certGetter: func() ([]byte, error) {
					return tt.currentCert, nil
				},
			}
			_, _, err := c.Get(ctx)
			tt.assertError(t, err)
			tt.expectNewClientBuild(t, mock.countClientBuild() != 0)
		})
	}
}
