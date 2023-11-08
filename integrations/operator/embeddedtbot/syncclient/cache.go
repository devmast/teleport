package syncclient

import (
	"bytes"
	"context"
	"sync"

	"github.com/gravitational/trace"
)

type Cache struct {
	// mutex protects cachedCert and cachedClient
	mutex        sync.Mutex
	cachedCert   []byte
	cachedClient *SyncClient

	// clientBuilder is used for testing purposes. Outside of tests, its value should always be buildClient.
	clientBuilder func(ctx context.Context) (*SyncClient, error)
	certGetter    func() ([]byte, error)
}

func NewCache(clientBuilder func(ctx context.Context) (*SyncClient, error), certGetter func() ([]byte, error)) *Cache {
	return &Cache{
		clientBuilder: clientBuilder,
		certGetter:    certGetter,
	}
}

func (c *Cache) Get(ctx context.Context) (*SyncClient, func(), error) {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	// This is where caching happens. We don't know when tbot renews the certificates, so we need to check
	// if the current certificate stored in memory changed since last time. If it did not and we already built a
	// working client, then we hit the cache. Else we build a new client, replace the cached client with the new one,
	// and fire a separate goroutine to close the previous client.
	cert, err := c.certGetter()
	if err != nil {
		return nil, nil, trace.Wrap(err)
	}
	if cert == nil || len(cert) == 0 {
		return nil, nil, trace.CompareFailed("no certificate in tbot's memory, cannot compare")
	}

	if c.cachedClient != nil && bytes.Equal(cert, c.cachedCert) {
		return c.cachedClient, c.cachedClient.LockClient(), nil
	}

	oldClient := c.cachedClient
	freshClient, err := c.clientBuilder(ctx)

	if err != nil {
		return nil, nil, trace.Wrap(err)
	}

	c.cachedCert = cert
	c.cachedClient = freshClient

	if oldClient != nil {
		go oldClient.RetireClient()
	}

	return c.cachedClient, c.cachedClient.LockClient(), nil
}
