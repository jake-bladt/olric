// Copyright 2018-2019 Burak Sezer
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

package olric

import (
	"bytes"
	"errors"
	"sort"
	"time"

	"github.com/buraksezer/olric/config"
	"github.com/buraksezer/olric/internal/discovery"
	"github.com/buraksezer/olric/internal/protocol"
	"github.com/buraksezer/olric/internal/storage"
	"github.com/vmihailenco/msgpack"
)

var ErrReadQuorum = errors.New("read quorum cannot be reached")

type version struct {
	host *discovery.Member
	Data *storage.VData
}

func (db *Olric) unmarshalValue(rawval []byte) (interface{}, error) {
	var value interface{}
	err := db.serializer.Unmarshal(rawval, &value)
	if err != nil {
		return nil, err
	}
	if _, ok := value.(struct{}); ok {
		return nil, nil
	}
	return value, nil
}

// lookupOnOwners collects versions of a key/value pair on the partition owner
// by including previous partition owners.
func (db *Olric) lookupOnOwners(dm *dmap, hkey uint64, name, key string) []*version {
	var versions []*version

	// Check on localhost, the partition owner.
	value, err := dm.storage.Get(hkey)
	ver := &version{host: &db.this}
	if err == nil {
		ver.Data = value
	} else {
		if db.log.V(3).Ok() {
			db.log.V(3).Printf("[ERROR] Failed to get key from local storage: %v", err)
		}
	}
	versions = append(versions, ver)

	// Run a query on the previous owners.
	owners := db.getPartitionOwners(hkey)
	if len(owners) == 0 {
		panic("partition owners list cannot be empty")
	}

	// Traverse in reverse order. Except from the latest host, this one.
	for i := len(owners) - 2; i >= 0; i-- {
		owner := owners[i]
		req := &protocol.Message{
			DMap: name,
			Key:  key,
		}

		ver := &version{host: &owner}
		resp, err := db.requestTo(owner.String(), protocol.OpGetPrev, req)
		if err != nil {
			if db.log.V(3).Ok() {
				db.log.V(3).Printf("[ERROR] Failed to call get on a previous "+
					"primary owner: %s: %v", owner, err)
			}
		} else {
			data := storage.VData{}
			err = msgpack.Unmarshal(resp.Value, data)
			if err != nil {
				db.log.V(2).Printf("[ERROR] Failed to unmarshal data from the "+
					"previous primary owner: %s: %v", owner, err)
			} else {
				ver.Data = &data
				// Ignore failed owners. The data on those hosts will be wiped out
				// by the rebalancer.
				versions = append(versions, ver)
			}
		}
	}
	return versions
}

func (db *Olric) sortVersions(versions []*version) []*version {
	sort.Slice(versions,
		func(i, j int) bool {
			if versions[i].Data.Timestamp == versions[j].Data.Timestamp {
				// The first one is greater or equal than the second one.
				return bytes.Compare(versions[i].Data.Value, versions[j].Data.Value) >= 0
			}
			return versions[i].Data.Timestamp > versions[j].Data.Timestamp
		},
	)
	// Explicit is better than implicit.
	return versions
}

func (db *Olric) sanitizeAndSortVersions(versions []*version) []*version {
	var sanitized []*version
	// We use versions slice for read-repair. Clear nil values first.
	for _, ver := range versions {
		if ver.Data != nil {
			sanitized = append(sanitized, ver)
		}
	}
	if len(sanitized) <= 1 {
		return sanitized
	}
	return db.sortVersions(sanitized)
}

func (db *Olric) lookupOnReplicas(dm *dmap, hkey uint64, name, key string) []*version {
	var versions []*version
	// Check backups.
	backups := db.getBackupPartitionOwners(hkey)
	for _, replica := range backups {
		req := &protocol.Message{
			DMap: name,
			Key:  key,
		}

		ver := &version{host: &replica}
		resp, err := db.requestTo(replica.String(), protocol.OpGetBackup, req)
		if err != nil {
			if db.log.V(3).Ok() {
				db.log.V(3).Printf("[ERROR] Failed to call get on a replica owner: %s: %v", replica, err)
			}
		} else {
			value := storage.VData{}
			err = msgpack.Unmarshal(resp.Value, &value)
			if err != nil {
				db.log.V(2).Printf("[ERROR] Failed to unmarshal data from a replica owner: %s: %v", replica, err)
			} else {
				ver.Data = &value
			}
		}
		versions = append(versions, ver)
	}
	return versions
}

func (db *Olric) readRepair(name string, dm *dmap, winner *version, versions []*version) {
	for _, ver := range versions {
		if ver.Data != nil && winner.Data.Timestamp == ver.Data.Timestamp {
			continue
		}

		// If readRepair is enabled, this function is called by every GET request.
		req := &protocol.Message{
			DMap:  name,
			Key:   winner.Data.Key,
			Value: winner.Data.Value,
		}
		var op protocol.OpCode
		if winner.Data.TTL == 0 {
			op = protocol.OpPutReplica
			req.Extra = protocol.PutExtra{Timestamp: winner.Data.Timestamp}
		} else {
			op = protocol.OpPutExReplica
			req.Extra = protocol.PutExExtra{
				Timestamp: winner.Data.Timestamp,
				TTL:       winner.Data.TTL,
			}
		}

		// Sync
		if hostCmp(*ver.host, db.this) {
			hkey := db.getHKey(name, winner.Data.Key)
			w := &writeop{
				dmap:      name,
				key:       winner.Data.Key,
				value:     winner.Data.Value,
				timestamp: winner.Data.Timestamp,
				timeout:   time.Duration(winner.Data.TTL),
			}
			dm.Lock()
			err := db.localPut(hkey, dm, w)
			if err != nil {
				db.log.V(2).Printf("[ERROR] Failed to synchronize with replica: %v", err)
			}
			dm.Unlock()
		} else {
			_, err := db.requestTo(ver.host.String(), op, req)
			if err != nil {
				db.log.V(2).Printf("[ERROR] Failed to synchronize replica %s: %v", ver.host, err)
			}
		}
	}
}

func (db *Olric) callGetOnCluster(hkey uint64, name, key string) ([]byte, error) {
	dm, err := db.getDMap(name, hkey)
	if err != nil {
		return nil, err
	}
	dm.RLock()
	// RUnlock should not be called with defer statement here because
	// readRepair function may call localPut function which needs a write
	// lock. Please don't forget calling RUnlock before returning here.

	versions := db.lookupOnOwners(dm, hkey, name, key)
	if db.config.ReadQuorum >= config.MinimumReplicaCount {
		v := db.lookupOnReplicas(dm, hkey, name, key)
		versions = append(versions, v...)
	}
	if len(versions) < db.config.ReadQuorum {
		dm.RUnlock()
		return nil, ErrReadQuorum
	}
	sorted := db.sanitizeAndSortVersions(versions)
	if len(sorted) == 0 {
		// We checked everywhere, it's not here.
		dm.RUnlock()
		return nil, ErrKeyNotFound
	}
	if len(sorted) < db.config.ReadQuorum {
		dm.RUnlock()
		return nil, ErrReadQuorum
	}

	// The most up-to-date version of the values.
	winner := sorted[0]
	if isKeyExpired(winner.Data.TTL) || dm.isKeyIdle(hkey) {
		dm.RUnlock()
		return nil, ErrKeyNotFound
	}
	// LRU and MaxIdleDuration eviction policies are only valid on
	// the partition owner. Normally, we shouldn't need to retrieve the keys
	// from the backup or the previous owners. When the fsck merge
	// a fragmented partition or recover keys from a backup, Olric
	// continue maintaining a reliable access log.
	dm.updateAccessLog(hkey)

	dm.RUnlock()
	if db.config.ReadRepair {
		// Parallel read operations may propagate different versions of
		// the same key/value pair. The rule is simple: last write wins.
		db.readRepair(name, dm, winner, versions)
	}
	return winner.Data.Value, nil
}

func (db *Olric) get(name, key string) ([]byte, error) {
	member, hkey := db.findPartitionOwner(name, key)
	// We are on the partition owner
	if hostCmp(member, db.this) {
		return db.callGetOnCluster(hkey, name, key)
	}
	// Redirect to the partition owner
	req := &protocol.Message{
		DMap: name,
		Key:  key,
	}
	resp, err := db.requestTo(member.String(), protocol.OpGet, req)
	if err != nil {
		return nil, err
	}
	return resp.Value, nil
}

// Get gets the value for the given key. It returns ErrKeyNotFound if the DB
// does not contains the key. It's thread-safe. It is safe to modify the contents
// of the returned value. It is safe to modify the contents of the argument
// after Get returns.
func (dm *DMap) Get(key string) (interface{}, error) {
	rawval, err := dm.db.get(dm.name, key)
	if err != nil {
		return nil, err
	}
	return dm.db.unmarshalValue(rawval)
}

func (db *Olric) exGetOperation(req *protocol.Message) *protocol.Message {
	value, err := db.get(req.DMap, req.Key)
	if err != nil {
		return db.prepareResponse(req, err)
	}
	resp := req.Success()
	resp.Value = value
	return resp
}

func (db *Olric) getBackupOperation(req *protocol.Message) *protocol.Message {
	hkey := db.getHKey(req.DMap, req.Key)
	dm, err := db.getBackupDMap(req.DMap, hkey)
	if err != nil {
		return db.prepareResponse(req, err)
	}
	dm.RLock()
	defer dm.RUnlock()
	vdata, err := dm.storage.Get(hkey)
	if err != nil {
		return db.prepareResponse(req, err)
	}
	if isKeyExpired(vdata.TTL) {
		return req.Error(protocol.StatusErrKeyNotFound, "key expired")
	}

	value, err := msgpack.Marshal(*vdata)
	if err != nil {
		return db.prepareResponse(req, err)
	}
	resp := req.Success()
	resp.Value = value
	return resp
}

func (db *Olric) getPrevOperation(req *protocol.Message) *protocol.Message {
	hkey := db.getHKey(req.DMap, req.Key)
	part := db.getPartition(hkey)
	tmp, ok := part.m.Load(req.DMap)
	if !ok {
		return req.Error(protocol.StatusErrKeyNotFound, "")
	}
	dm := tmp.(*dmap)

	vdata, err := dm.storage.Get(hkey)
	if err != nil {
		return db.prepareResponse(req, err)
	}

	if isKeyExpired(vdata.TTL) {
		return req.Error(protocol.StatusErrKeyNotFound, "key expired")
	}

	value, err := msgpack.Marshal(*vdata)
	if err != nil {
		return db.prepareResponse(req, err)
	}
	resp := req.Success()
	resp.Value = value
	return resp
}
