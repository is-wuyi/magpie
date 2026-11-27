package provider

import (
	"errors"
	"sync"
)

// CloudLoader returns every tenant-owned and house provider. Implementations
// must return stable, namespace-unique provider IDs and no subscription
// accounts. The gateway applies the caller's authorization before use.
type CloudLoader func() ([]Provider, error)

var cloudSource struct {
	sync.RWMutex
	load CloudLoader
	err  error
}

// RegisterCloudLoader installs the database-backed provider source used in
// cloud mode. It must be called before the gateway starts serving requests.
func RegisterCloudLoader(load CloudLoader) {
	cloudSource.Lock()
	defer cloudSource.Unlock()
	if load == nil {
		panic("provider: nil cloud loader")
	}
	if cloudSource.load != nil {
		panic("provider: cloud loader registered twice")
	}
	cloudSource.load = load
}

func loadCloudProviders() ([]Provider, error) {
	cloudSource.RLock()
	load := cloudSource.load
	cloudSource.RUnlock()
	if load == nil {
		return nil, errors.New("cloud provider store is not configured")
	}
	ps, err := load()
	cloudSource.Lock()
	cloudSource.err = err
	cloudSource.Unlock()
	if err != nil {
		return nil, err
	}
	out := make([]Provider, 0, len(ps))
	for _, p := range ps {
		if p.Account != nil || p.OwnerUserID == "" && p.HouseChannelID == "" {
			return nil, errors.New("cloud provider store returned an unowned or subscription provider")
		}
		out = append(out, normalize(p))
	}
	return out, nil
}

// CloudError is the last provider-store read failure. A failure never falls
// back to the local provider file or to an empty authorized catalog.
func CloudError() error {
	cloudSource.RLock()
	defer cloudSource.RUnlock()
	return cloudSource.err
}
