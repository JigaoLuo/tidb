// Copyright 2022 PingCAP, Inc. Licensed under Apache-2.0.

package streamhelper_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"math"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"testing"

	backup "github.com/pingcap/kvproto/pkg/brpb"
	"github.com/pingcap/kvproto/pkg/errorpb"
	logbackup "github.com/pingcap/kvproto/pkg/logbackuppb"
	"github.com/pingcap/kvproto/pkg/metapb"
	"github.com/pingcap/log"
	"github.com/pingcap/tidb/br/pkg/streamhelper"
	"github.com/pingcap/tidb/br/pkg/utils"
	"github.com/pingcap/tidb/kv"
	"go.uber.org/zap"
	"google.golang.org/grpc"
)

type flushSimulator struct {
	flushedEpoch uint64
	enabled      bool
}

func (c flushSimulator) makeError(requestedEpoch uint64) *errorpb.Error {
	if !c.enabled {
		return nil
	}
	if c.flushedEpoch == 0 {
		e := errorpb.Error{
			Message: "not flushed",
		}
		return &e
	}
	if c.flushedEpoch != requestedEpoch {
		e := errorpb.Error{
			Message: "flushed epoch not match",
		}
		return &e
	}
	return nil
}

func (c flushSimulator) fork() flushSimulator {
	return flushSimulator{
		enabled: c.enabled,
	}
}

type region struct {
	rng        kv.KeyRange
	leader     uint64
	epoch      uint64
	id         uint64
	checkpoint uint64

	fsim flushSimulator
}

type fakeStore struct {
	id      uint64
	regions map[uint64]*region
}

type fakeCluster struct {
	mu        sync.Mutex
	idAlloced uint64
	stores    map[uint64]*fakeStore
	regions   []*region
	testCtx   *testing.T

	onGetClient func(uint64) error
}

func overlaps(a, b kv.KeyRange) bool {
	if len(b.EndKey) == 0 {
		return len(a.EndKey) == 0 || bytes.Compare(a.EndKey, b.StartKey) > 0
	}
	if len(a.EndKey) == 0 {
		return len(b.EndKey) == 0 || bytes.Compare(b.EndKey, a.StartKey) > 0
	}
	return bytes.Compare(a.StartKey, b.EndKey) < 0 && bytes.Compare(b.StartKey, a.EndKey) < 0
}

func (r *region) splitAt(newID uint64, k string) *region {
	newRegion := &region{
		rng:        kv.KeyRange{StartKey: []byte(k), EndKey: r.rng.EndKey},
		leader:     r.leader,
		epoch:      r.epoch + 1,
		id:         newID,
		checkpoint: r.checkpoint,
		fsim:       r.fsim.fork(),
	}
	r.rng.EndKey = []byte(k)
	r.epoch += 1
	r.fsim = r.fsim.fork()
	return newRegion
}

func (r *region) flush() {
	r.fsim.flushedEpoch = r.epoch
}

func (f *fakeStore) GetLastFlushTSOfRegion(ctx context.Context, in *logbackup.GetLastFlushTSOfRegionRequest, opts ...grpc.CallOption) (*logbackup.GetLastFlushTSOfRegionResponse, error) {
	resp := &logbackup.GetLastFlushTSOfRegionResponse{
		Checkpoints: []*logbackup.RegionCheckpoint{},
	}
	for _, r := range in.Regions {
		region, ok := f.regions[r.Id]
		if !ok || region.leader != f.id {
			resp.Checkpoints = append(resp.Checkpoints, &logbackup.RegionCheckpoint{
				Err: &errorpb.Error{
					Message: "not found",
				},
				Region: &logbackup.RegionIdentity{
					Id:           region.id,
					EpochVersion: region.epoch,
				},
			})
			continue
		}
		if err := region.fsim.makeError(r.EpochVersion); err != nil {
			resp.Checkpoints = append(resp.Checkpoints, &logbackup.RegionCheckpoint{
				Err: err,
				Region: &logbackup.RegionIdentity{
					Id:           region.id,
					EpochVersion: region.epoch,
				},
			})
			continue
		}
		if region.epoch != r.EpochVersion {
			resp.Checkpoints = append(resp.Checkpoints, &logbackup.RegionCheckpoint{
				Err: &errorpb.Error{
					Message: "epoch not match",
				},
				Region: &logbackup.RegionIdentity{
					Id:           region.id,
					EpochVersion: region.epoch,
				},
			})
			continue
		}
		resp.Checkpoints = append(resp.Checkpoints, &logbackup.RegionCheckpoint{
			Checkpoint: region.checkpoint,
			Region: &logbackup.RegionIdentity{
				Id:           region.id,
				EpochVersion: region.epoch,
			},
		})
	}
	log.Debug("Get last flush ts of region", zap.Stringer("in", in), zap.Stringer("out", resp))
	return resp, nil
}

// RegionScan gets a list of regions, starts from the region that contains key.
// Limit limits the maximum number of regions returned.
func (f *fakeCluster) RegionScan(ctx context.Context, key []byte, endKey []byte, limit int) ([]streamhelper.RegionWithLeader, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sort.Slice(f.regions, func(i, j int) bool {
		return bytes.Compare(f.regions[i].rng.StartKey, f.regions[j].rng.StartKey) < 0
	})

	result := make([]streamhelper.RegionWithLeader, 0, limit)
	for _, region := range f.regions {
		if overlaps(kv.KeyRange{StartKey: key, EndKey: endKey}, region.rng) && len(result) < limit {
			regionInfo := streamhelper.RegionWithLeader{
				Region: &metapb.Region{
					Id:       region.id,
					StartKey: region.rng.StartKey,
					EndKey:   region.rng.EndKey,
					RegionEpoch: &metapb.RegionEpoch{
						Version: region.epoch,
					},
				},
				Leader: &metapb.Peer{
					StoreId: region.leader,
				},
			}
			result = append(result, regionInfo)
		} else if bytes.Compare(region.rng.StartKey, key) > 0 {
			break
		}
	}
	return result, nil
}

func (f *fakeCluster) GetLogBackupClient(ctx context.Context, storeID uint64) (logbackup.LogBackupClient, error) {
	if f.onGetClient != nil {
		err := f.onGetClient(storeID)
		if err != nil {
			return nil, err
		}
	}
	cli, ok := f.stores[storeID]
	if !ok {
		f.testCtx.Fatalf("the store %d doesn't exist", storeID)
	}
	return cli, nil
}

func (f *fakeCluster) findRegionById(rid uint64) *region {
	for _, r := range f.regions {
		if r.id == rid {
			return r
		}
	}
	return nil
}

func (f *fakeCluster) findRegionByKey(key []byte) *region {
	for _, r := range f.regions {
		if bytes.Compare(key, r.rng.StartKey) >= 0 && (len(r.rng.EndKey) == 0 || bytes.Compare(key, r.rng.EndKey) < 0) {
			return r
		}
	}
	panic(fmt.Sprintf("inconsistent key space; key = %X", key))
}

func (f *fakeCluster) transferRegionTo(rid uint64, newPeers []uint64) {
	r := f.findRegionById(rid)
storeLoop:
	for _, store := range f.stores {
		for _, pid := range newPeers {
			if pid == store.id {
				store.regions[rid] = r
				continue storeLoop
			}
		}
		delete(store.regions, rid)
	}
}

func (f *fakeCluster) splitAt(key string) {
	k := []byte(key)
	r := f.findRegionByKey(k)
	newRegion := r.splitAt(f.idAlloc(), key)
	for _, store := range f.stores {
		_, ok := store.regions[r.id]
		if ok {
			store.regions[newRegion.id] = newRegion
		}
	}
	f.regions = append(f.regions, newRegion)
}

func (f *fakeCluster) idAlloc() uint64 {
	f.idAlloced++
	return f.idAlloced
}

func (f *fakeCluster) chooseStores(n int) []uint64 {
	s := make([]uint64, 0, len(f.stores))
	for id := range f.stores {
		s = append(s, id)
	}
	rand.Shuffle(len(s), func(i, j int) {
		s[i], s[j] = s[j], s[i]
	})
	return s[:n]
}

func (f *fakeCluster) findPeers(rid uint64) (result []uint64) {
	for _, store := range f.stores {
		if _, ok := store.regions[rid]; ok {
			result = append(result, store.id)
		}
	}
	return
}

func (f *fakeCluster) shuffleLeader(rid uint64) {
	r := f.findRegionById(rid)
	peers := f.findPeers(rid)
	rand.Shuffle(len(peers), func(i, j int) {
		peers[i], peers[j] = peers[j], peers[i]
	})

	newLeader := peers[0]
	r.leader = newLeader
}

func (f *fakeCluster) splitAndScatter(keys ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, key := range keys {
		f.splitAt(key)
	}
	for _, r := range f.regions {
		chosen := f.chooseStores(3)
		f.transferRegionTo(r.id, chosen)
		f.shuffleLeader(r.id)
	}
}

// a stub once in the future we want to make different stores hold different region instances.
func (f *fakeCluster) updateRegion(rid uint64, mut func(*region)) {
	r := f.findRegionById(rid)
	mut(r)
}

func (f *fakeCluster) advanceCheckpoints() uint64 {
	minCheckpoint := uint64(math.MaxUint64)
	for _, r := range f.regions {
		f.updateRegion(r.id, func(r *region) {
			// The current implementation assumes that the server never returns checkpoint with value 0.
			// This assumption is true for the TiKV implementation, simulating it here.
			r.checkpoint += rand.Uint64()%256 + 1
			if r.checkpoint < minCheckpoint {
				minCheckpoint = r.checkpoint
			}
			r.fsim.flushedEpoch = 0
		})
	}
	log.Info("checkpoint updated", zap.Uint64("to", minCheckpoint))
	return minCheckpoint
}

func createFakeCluster(t *testing.T, n int, simEnabled bool) *fakeCluster {
	c := &fakeCluster{
		stores:  map[uint64]*fakeStore{},
		regions: []*region{},
		testCtx: t,
	}
	stores := make([]*fakeStore, 0, n)
	for i := 0; i < n; i++ {
		s := new(fakeStore)
		s.id = c.idAlloc()
		s.regions = map[uint64]*region{}
		stores = append(stores, s)
	}
	initialRegion := &region{
		rng:        kv.KeyRange{},
		leader:     stores[0].id,
		epoch:      0,
		id:         c.idAlloc(),
		checkpoint: 0,
		fsim: flushSimulator{
			enabled: simEnabled,
		},
	}
	for i := 0; i < 3; i++ {
		if i < len(stores) {
			stores[i].regions[initialRegion.id] = initialRegion
		}
	}
	for _, s := range stores {
		c.stores[s.id] = s
	}
	c.regions = append(c.regions, initialRegion)
	return c
}

func (r *region) String() string {
	return fmt.Sprintf("%d(%d):[%s,%s);%dL%dF%d",
		r.id,
		r.epoch,
		hex.EncodeToString(r.rng.StartKey),
		hex.EncodeToString(r.rng.EndKey),
		r.checkpoint,
		r.leader,
		r.fsim.flushedEpoch)
}

func (f *fakeStore) String() string {
	buf := new(strings.Builder)
	fmt.Fprintf(buf, "%d: ", f.id)
	for _, r := range f.regions {
		fmt.Fprintf(buf, "%s ", r)
	}
	return buf.String()
}

func (f *fakeCluster) flushAll() {
	for _, r := range f.regions {
		r.flush()
	}
}

func (f *fakeCluster) flushAllExcept(keys ...string) {
outer:
	for _, r := range f.regions {
		// Note: can we make it faster?
		for _, key := range keys {
			if utils.CompareBytesExt(r.rng.StartKey, false, []byte(key), false) <= 0 &&
				utils.CompareBytesExt([]byte(key), false, r.rng.EndKey, true) < 0 {
				continue outer
			}
		}
		r.flush()
	}
}

func (f *fakeStore) flush() {
	for _, r := range f.regions {
		if r.leader == f.id {
			r.flush()
		}
	}
}

func (f *fakeCluster) String() string {
	buf := new(strings.Builder)
	fmt.Fprint(buf, ">>> fake cluster <<<\nregions: ")
	for _, region := range f.regions {
		fmt.Fprint(buf, region, " ")
	}
	fmt.Fprintln(buf)
	for _, store := range f.stores {
		fmt.Fprintln(buf, store)
	}
	return buf.String()
}

type testEnv struct {
	*fakeCluster
	checkpoint uint64
	testCtx    *testing.T
	ranges     []kv.KeyRange

	mu sync.Mutex
}

func (t *testEnv) Begin(ctx context.Context, ch chan<- streamhelper.TaskEvent) error {
	rngs := t.ranges
	if len(rngs) == 0 {
		rngs = []kv.KeyRange{{}}
	}
	tsk := streamhelper.TaskEvent{
		Type: streamhelper.EventAdd,
		Name: "whole",
		Info: &backup.StreamBackupTaskInfo{
			Name: "whole",
		},
		Ranges: rngs,
	}
	ch <- tsk
	return nil
}

func (t *testEnv) UploadV3GlobalCheckpointForTask(ctx context.Context, _ string, checkpoint uint64) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if checkpoint < t.checkpoint {
		t.testCtx.Fatalf("checkpoint rolling back (from %d to %d)", t.checkpoint, checkpoint)
	}
	t.checkpoint = checkpoint
	return nil
}

func (t *testEnv) ClearV3GlobalCheckpointForTask(ctx context.Context, taskName string) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.checkpoint = 0
	return nil
}

func (t *testEnv) getCheckpoint() uint64 {
	t.mu.Lock()
	defer t.mu.Unlock()

	return t.checkpoint
}
