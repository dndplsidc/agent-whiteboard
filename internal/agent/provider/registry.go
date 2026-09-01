package provider

import (
	"errors"
	"sort"

	"github.com/dndplsidc/agent-whiteboard/internal/common"
)

// Registry is an immutable, closed mapping from provider names to their
// adapters. It is the provider-neutral dispatch boundary used by the broker.
type Registry struct {
	drivers map[Name]Driver
}

func NewRegistry(drivers map[Name]Driver) (Registry, error) {
	if len(drivers) == 0 {
		return Registry{}, errors.New("provider registry is empty")
	}
	owned := make(map[Name]Driver, len(drivers))
	for name, driver := range drivers {
		if !name.Valid() || common.IsNil(driver) {
			return Registry{}, errors.New("invalid provider registry entry")
		}
		owned[name] = driver
	}
	return Registry{drivers: owned}, nil
}

func (registry Registry) Lookup(name Name) Driver {
	if !name.Valid() {
		return nil
	}
	return registry.drivers[name]
}

func (registry Registry) Names() []Name {
	names := make([]Name, 0, len(registry.drivers))
	for name := range registry.drivers {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		order := func(name Name) int {
			switch name {
			case NamePi:
				return 0
			case NameCodex:
				return 1
			default:
				return 2
			}
		}
		return order(names[i]) < order(names[j])
	})
	return names
}
