package provider

import (
	"errors"
	"sync"
)

type CloudGroupLoader func() ([]Group, error)

var cloudGroupSource struct {
	sync.RWMutex
	load CloudGroupLoader
}

func RegisterCloudGroupLoader(load CloudGroupLoader) {
	cloudGroupSource.Lock()
	defer cloudGroupSource.Unlock()
	if load == nil {
		panic("provider: nil cloud group loader")
	}
	if cloudGroupSource.load != nil {
		panic("provider: cloud group loader registered twice")
	}
	cloudGroupSource.load = load
}

func loadCloudGroups() ([]Group, error) {
	cloudGroupSource.RLock()
	load := cloudGroupSource.load
	cloudGroupSource.RUnlock()
	if load == nil {
		return nil, errors.New("cloud group store is not configured")
	}
	gs, err := load()
	if err != nil {
		return nil, err
	}
	for _, g := range gs {
		if g.ID == "" || g.OwnerUserID == "" && g.HouseChannelID == "" || g.OwnerUserID != "" && g.HouseChannelID != "" {
			return nil, errors.New("cloud group store returned an unowned group")
		}
	}
	return gs, nil
}
