//                           _       _
// __      _____  __ ___   ___  __ _| |_ ___
// \ \ /\ / / _ \/ _` \ \ / / |/ _` | __/ _ \
//  \ V  V /  __/ (_| |\ V /| | (_| | ||  __/
//   \_/\_/ \___|\__,_| \_/ |_|\__,_|\__\___|
//
//  Copyright © 2016 - 2024 Weaviate B.V. All rights reserved.
//
//  CONTACT: hello@weaviate.io
//

package store

import (
	"fmt"
	"sync"

	command "github.com/weaviate/weaviate/cluster/proto/cluster"
	"github.com/weaviate/weaviate/entities/models"
	"github.com/weaviate/weaviate/usecases/sharding"
	"golang.org/x/exp/slices"
)

type metaClass struct {
	sync.RWMutex
	Class    models.Class
	Sharding sharding.State
}

func (m *metaClass) ClassInfo() (ci ClassInfo) {
	if m == nil {
		return
	}
	m.RLock()
	defer m.RUnlock()
	ci.Exists = true
	ci.Properties = len(m.Class.Properties)
	ci.MultiTenancy = m.MultiTenancyConfig()
	ci.ReplicationFactor = 1
	if m.Class.ReplicationConfig != nil && m.Class.ReplicationConfig.Factor > 1 {
		ci.ReplicationFactor = int(m.Class.ReplicationConfig.Factor)
	}
	ci.Tenants = len(m.Sharding.Physical)
	return ci
}

func (m *metaClass) MultiTenancyConfig() (cfg models.MultiTenancyConfig) {
	if m == nil {
		return
	}
	m.RLock()
	defer m.RUnlock()
	if m.Class.MultiTenancyConfig == nil {
		return
	}

	cfg = *m.Class.MultiTenancyConfig
	return
}

// CloneClass returns a shallow copy of m
func (m *metaClass) CloneClass() *models.Class {
	m.RLock()
	defer m.RUnlock()
	cp := m.Class
	return &cp
}

// ShardOwner returns the node owner of the specified shard
func (m *metaClass) ShardOwner(shard string) (string, error) {
	m.RLock()
	defer m.RUnlock()
	x, ok := m.Sharding.Physical[shard]

	if !ok {
		return "", errShardNotFound
	}
	if len(x.BelongsToNodes) < 1 || x.BelongsToNodes[0] == "" {
		return "", fmt.Errorf("owner node not found")
	}
	return x.BelongsToNodes[0], nil
}

// ShardFromUUID returns shard name of the provided uuid
func (m *metaClass) ShardFromUUID(uuid []byte) string {
	m.RLock()
	defer m.RUnlock()
	return m.Sharding.PhysicalShard(uuid)
}

// ShardReplicas returns the replica nodes of a shard
func (m *metaClass) ShardReplicas(shard string) ([]string, error) {
	m.RLock()
	defer m.RUnlock()
	x, ok := m.Sharding.Physical[shard]
	if !ok {
		return nil, errShardNotFound
	}
	return slices.Clone(x.BelongsToNodes), nil
}

// TenantShard returns shard name for the provided tenant and its activity status
func (m *metaClass) TenantShard(tenant string) (string, string) {
	m.RLock()
	defer m.RUnlock()

	if !m.Sharding.PartitioningEnabled {
		return "", ""
	}
	if physical, ok := m.Sharding.Physical[tenant]; ok {
		return tenant, physical.ActivityStatus()
	}
	return "", ""
}

// CopyShardingState returns a deep copy of the sharding state
func (m *metaClass) CopyShardingState() *sharding.State {
	m.RLock()
	defer m.RUnlock()
	st := m.Sharding.DeepCopy()
	return &st
}

func (m *metaClass) AddProperty(p models.Property) error {
	m.Lock()
	defer m.Unlock()

	// update all at once to prevent race condition with concurrent readers
	src := m.Class.Properties
	dest := make([]*models.Property, len(src)+1)
	copy(dest, src)
	dest[len(src)] = &p
	m.Class.Properties = dest
	return nil
}

func (m *metaClass) AddTenants(nodeID string, req *command.AddTenantsRequest) error {
	req.Tenants = removeNilTenants(req.Tenants)
	m.Lock()
	defer m.Unlock()

	for i, t := range req.Tenants {
		if _, ok := m.Sharding.Physical[t.Name]; ok {
			req.Tenants[i] = nil // already exists
			continue
		}

		p := sharding.Physical{Name: t.Name, Status: t.Status, BelongsToNodes: t.Nodes}
		m.Sharding.Physical[t.Name] = p
		if !slices.Contains(t.Nodes, nodeID) {
			req.Tenants[i] = nil // is owner by another node
		}
	}
	req.Tenants = removeNilTenants(req.Tenants)
	return nil
}

func (m *metaClass) DeleteTenants(req *command.DeleteTenantsRequest) error {
	m.Lock()
	defer m.Unlock()

	for _, name := range req.Tenants {
		m.Sharding.DeletePartition(name)
	}
	return nil
}

func (m *metaClass) UpdateTenants(nodeID string, req *command.UpdateTenantsRequest) (n int, err error) {
	m.Lock()
	defer m.Unlock()

	missingShards := []string{}
	ps := m.Sharding.Physical
	for i, u := range req.Tenants {

		p, ok := ps[u.Name]
		if !ok {
			missingShards = append(missingShards, u.Name)
			req.Tenants[i] = nil
			continue
		}
		if p.ActivityStatus() == u.Status {
			req.Tenants[i] = nil
			continue
		}
		copy := p.DeepCopy()
		copy.Status = u.Status
		if u.Nodes != nil && len(u.Nodes) >= 0 {
			copy.BelongsToNodes = u.Nodes
		}
		ps[u.Name] = copy
		if !slices.Contains(copy.BelongsToNodes, nodeID) {
			req.Tenants[i] = nil
		}
		n++
	}
	if len(missingShards) > 0 {
		err = fmt.Errorf("%w: %v", errShardNotFound, missingShards)
	}

	req.Tenants = removeNilTenants(req.Tenants)
	return
}

// LockGuard provides convenient mechanism for owning mutex by function which mutates the state.
func (m *metaClass) LockGuard(mutator func(*models.Class, *sharding.State) error) error {
	m.Lock()
	defer m.Unlock()
	return mutator(&m.Class, &m.Sharding)
}

// RLockGuard provides convenient mechanism for owning mutex function which doesn't mutates the state
func (m *metaClass) RLockGuard(reader func(*models.Class, *sharding.State) error) error {
	m.RLock()
	defer m.RUnlock()
	return reader(&m.Class, &m.Sharding)
}
