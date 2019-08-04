package activation

import (
	"errors"
	"fmt"
	"github.com/spacemeshos/go-spacemesh/common"
	"github.com/spacemeshos/go-spacemesh/database"
	"github.com/spacemeshos/go-spacemesh/log"
	"github.com/spacemeshos/go-spacemesh/mesh"
	"github.com/spacemeshos/go-spacemesh/types"
	"sync"
	"time"
)

const CounterKey = 0xaaaa
const posAtxKey = "posAtxKey"

type ActivationDb struct {
	sync.RWMutex
	//todo: think about whether we need one db or several
	atxs            database.DB
	nipsts          database.DB
	nipstLock       sync.RWMutex
	atxCache        AtxCache
	meshDb          *mesh.MeshDB
	LayersPerEpoch  uint16
	nipstValidator  NipstValidator
	ids             IdStore
	log             log.Log
	processAtxMutex sync.Mutex
}

func NewActivationDb(dbstore database.DB, nipstStore database.DB, idstore IdStore, meshDb *mesh.MeshDB, layersPerEpoch uint16, nipstValidator NipstValidator, log log.Log) *ActivationDb {
	return &ActivationDb{atxs: dbstore, nipsts: nipstStore, atxCache: NewAtxCache(350), meshDb: meshDb, nipstValidator: nipstValidator, LayersPerEpoch: layersPerEpoch, ids: idstore, log: log}
}

// ProcessAtx validates the active set size declared in the atx, and contextually validates the atx according to atx
// validation rules it then stores the atx with flag set to validity of the atx.
//
// ATXs received as input must be already syntactically valid. Only contextual validation is performed.
func (db *ActivationDb) ProcessAtx(atx *types.ActivationTx) {
	db.log.Info("waiting for mutex, atx %v", atx.ShortId())
	db.processAtxMutex.Lock()
	defer db.processAtxMutex.Unlock()

	db.log.Info("aquired mutex, atx %v", atx.ShortId())
	eatx, _ := db.GetAtx(atx.Id())
	if eatx != nil {
		db.log.Info("at ProcessAtx, atx %v already in DB", atx.ShortId())
		atx.Nipst = nil
		return
	}
	epoch := atx.PubLayerIdx.GetEpoch(db.LayersPerEpoch)
	db.log.With().Info("processing atx", log.AtxId(atx.ShortId()), log.EpochId(uint64(epoch)),
		log.NodeId(atx.NodeId.Key[:5]), log.LayerId(uint64(atx.PubLayerIdx)))
	err := db.ContextuallyValidateAtx(atx)
	if err != nil {
		db.log.With().Error("ATX failed contextual validation", log.AtxId(atx.ShortId()), log.Err(err))
	} else {
		db.log.With().Info("ATX is valid", log.AtxId(atx.ShortId()))
	}
	err = db.StoreAtx(epoch, atx)
	if err != nil {
		db.log.With().Error("cannot store atx", log.AtxId(atx.ShortId()), log.Err(err))
	}

	err = db.ids.StoreNodeIdentity(atx.NodeId)
	if err != nil {
		db.log.With().Error("cannot store node identity", log.NodeId(atx.NodeId.ShortString()), log.AtxId(atx.ShortId()), log.Err(err))
	}
}

// CalcActiveSetFromView traverses the view found in a - the activation tx and counts number of active ids published
// in the epoch prior to the epoch that a was published at, this number is the number of active ids in the next epoch
// the function returns error if the view is not found
func (db *ActivationDb) CalcActiveSetFromView(a *types.ActivationTx) (uint32, error) {
	viewBytes, err := types.ViewAsBytes(a.View)
	if err != nil {
		return 0, err
	}

	hash := common.BytesToHash(viewBytes)
	count, found := activesetCache.Get(hash)
	if found {
		db.log.Info("cache hit on active set size : %v hash %v", a.ShortId(), hash)
		return count, nil
	}
	db.log.Info("cache miss on active set size : %v hash %v", a.ShortId(), hash)

	var counter uint32 = 0
	set := make(map[types.AtxId]struct{})
	pubEpoch := a.PubLayerIdx.GetEpoch(db.LayersPerEpoch)
	if pubEpoch < 1 {
		return 0, fmt.Errorf("publication epoch cannot be less than 1, found %v", pubEpoch)
	}
	countingEpoch := pubEpoch - 1
	firstLayerOfLastEpoch := types.LayerID(countingEpoch) * types.LayerID(db.LayersPerEpoch)

	traversalFunc := func(blk *types.Block) error {
		//skip blocks not from atx epoch
		if blk.LayerIndex.GetEpoch(db.LayersPerEpoch) != countingEpoch {
			return nil
		}
		for _, id := range blk.AtxIds {
			if _, found := set[id]; found {
				continue
			}
			set[id] = struct{}{}
			atx, err := db.GetAtx(id)
			if err != nil {
				log.Panic("error fetching atx %v from database -- inconsistent state", id.ShortId()) // TODO: handle inconsistent state
				return fmt.Errorf("error fetching atx %v from database -- inconsistent state", id.ShortId())
			}
			if atx.TargetEpoch(db.LayersPerEpoch) != pubEpoch {
				db.log.Debug("atx %v found, but targeting epoch %v instead of publication epoch %v",
					atx.ShortId(), atx.TargetEpoch(db.LayersPerEpoch), pubEpoch)
				continue
			}
			counter++
			db.log.Debug("atx %v (epoch %d) found traversing in block %x (epoch %d)",
				atx.ShortId(), atx.TargetEpoch(db.LayersPerEpoch), blk.Id, blk.LayerIndex.GetEpoch(db.LayersPerEpoch))
		}
		return nil
	}

	mp := map[types.BlockID]struct{}{}
	for _, blk := range a.View {
		mp[blk] = struct{}{}
	}

	err = db.meshDb.ForBlockInView(mp, firstLayerOfLastEpoch, traversalFunc)
	if err != nil {
		return 0, err
	}

	activesetCache.Add(common.BytesToHash(viewBytes), counter)

	return counter, nil

}

// SyntacticallyValidateAtx ensures the following conditions apply, otherwise it returns an error.
//
// - If the sequence number is non-zero: PrevATX points to a syntactically valid ATX whose sequence number is one less
//   than the current ATX's sequence number.
// - If the sequence number is zero: PrevATX is empty.
// - Positioning ATX points to a syntactically valid ATX.
// - NIPST challenge is a hash of the serialization of the following fields:
//   NodeID, SequenceNumber, PrevATXID, LayerID, StartTick, PositioningATX.
// - The NIPST is valid.
// - ATX LayerID is NipstLayerTime or less after the PositioningATX LayerID.
// - The ATX view of the previous epoch contains ActiveSetSize activations.
func (db *ActivationDb) SyntacticallyValidateAtx(atx *types.ActivationTx) error {
	t := time.Now() //todo: remove time calc
	if atx.PrevATXId != *types.EmptyAtxId {
		prevATX, err := db.GetAtx(atx.PrevATXId)
		if err != nil {
			return fmt.Errorf("validation failed: prevATX not found: %v", err)
		}
		if prevATX.NodeId.Key != atx.NodeId.Key {
			return fmt.Errorf("previous ATX belongs to different miner. atx.Id: %v, atx.NodeId: %v, prevAtx.NodeId: %v",
				atx.ShortId(), atx.NodeId.Key, prevATX.NodeId.Key)
		}
		if prevATX.Sequence+1 != atx.Sequence {
			return fmt.Errorf("sequence number is not one more than prev sequence number")
		}
	} else {
		if atx.Sequence != 0 {
			return fmt.Errorf("no prevATX reported, but sequence number not zero")
		}
	}
	prevT := time.Since(t) //todo: remove time calc

	t1 := time.Now() //todo: remove time calc
	if atx.PositioningAtx != *types.EmptyAtxId {
		posAtx, err := db.GetAtx(atx.PositioningAtx)
		if err != nil {
			return fmt.Errorf("positioning atx not found")
		}
		if atx.PubLayerIdx <= posAtx.PubLayerIdx {
			return fmt.Errorf("atx layer (%v) must be after positioning atx layer (%v)",
				atx.PubLayerIdx, posAtx.PubLayerIdx)
		}
		if uint64(atx.PubLayerIdx-posAtx.PubLayerIdx) > uint64(db.LayersPerEpoch) {
			return fmt.Errorf("expected distance of one epoch (%v layers) from pos ATX but found %v",
				db.LayersPerEpoch, atx.PubLayerIdx-posAtx.PubLayerIdx)
		}
	} else {
		publicationEpoch := atx.PubLayerIdx.GetEpoch(db.LayersPerEpoch)
		if !publicationEpoch.IsGenesis() {
			return fmt.Errorf("no positioning atx found")
		}
	}
	posT := time.Since(t1) //todo: remove time calc

	t1 = time.Now() //todo: remove time calc
	activeSet, err := db.CalcActiveSetFromView(atx)
	if err != nil && !atx.PubLayerIdx.GetEpoch(db.LayersPerEpoch).IsGenesis() {
		return fmt.Errorf("could not calculate active set for ATX %v", atx.ShortId())
	}
	asT := time.Since(t1) //todo: remove time calc

	if atx.ActiveSetSize != activeSet {
		return fmt.Errorf("atx contains view with unequal active ids (%v) than seen (%v)", atx.ActiveSetSize, activeSet)
	}

	hash, err := atx.NIPSTChallenge.Hash()
	if err != nil {
		return fmt.Errorf("cannot get NIPST Challenge hash: %v", err)
	}

	t1 = time.Now()
	if err = db.nipstValidator.Validate(atx.Nipst, *hash); err != nil {
		return fmt.Errorf("NIPST not valid: %v", err)
	}
	npstT := time.Since(t1)
	db.log.With().Info("SyntacticallyValidateAtx",
		log.String("atx", atx.ShortId()),
		log.String("challenge_hash", hash.ShortString()),
		log.Duration("activeSetCalc", asT),
		log.Duration("prevT", prevT),
		log.Duration("posT", posT),
		log.Duration("npstValid", npstT),
		log.Duration("total", time.Since(t)))

	return nil
}

// ContextuallyValidateAtx ensures that the previous ATX referenced is the last known ATX for the referenced miner ID.
// If a previous ATX is not referenced, it validates that indeed there's no previous known ATX for that miner ID.
func (db *ActivationDb) ContextuallyValidateAtx(atx *types.ActivationTx) error {
	if atx.PrevATXId != *types.EmptyAtxId {
		lastAtx, err := db.GetNodeLastAtxId(atx.NodeId)
		if err != nil {
			db.log.WithFields(
				log.String("atx_id", atx.ShortId()), log.String("node_id", atx.NodeId.ShortString())).
				Error("could not fetch node last ATX: %v", err)
			return fmt.Errorf("could not fetch node last ATX: %v", err)
		}
		// last atx is not the one referenced
		if lastAtx != atx.PrevATXId {
			return fmt.Errorf("last atx is not the one referenced")
		}
	} else {
		lastAtx, err := db.GetNodeLastAtxId(atx.NodeId)
		if err != nil && err != database.ErrNotFound {
			db.log.Error("fetching ATX ids failed: %v", err)
			return err
		}
		if err == nil { // we found an ATX for this node ID, although it reported no prevATX -- this is invalid
			return fmt.Errorf("no prevATX reported, but other ATX with same nodeID (%v) found: %v",
				atx.NodeId.ShortString(), lastAtx.ShortString())
		}
	}

	return nil
}

// StoreAtx stores an atx for epoh ech, it stores atx for the current epoch and adds the atx for the nodeid
// that created it in a sorted manner by the sequence id. this function does not validate the atx and assumes all data is correct
// and that all associated atx exist in the db. will return error if writing to db failed
func (db *ActivationDb) StoreAtx(ech types.EpochId, atx *types.ActivationTx) error {
	db.Lock()
	defer db.Unlock()

	//todo: maybe cleanup DB if failed by using defer
	if b, err := db.atxs.Get(atx.Id().Bytes()); err == nil && len(b) > 0 {
		// exists - how should we handle this?
		return nil
	}

	err := db.storeAtxUnlocked(atx)
	if err != nil {
		return err
	}

	err = db.updatePosAtxIfNeeded(atx)
	if err != nil {
		return err
	}

	err = db.incValidAtxCounter(ech)
	if err != nil {
		return err
	}
	err = db.addAtxToNodeId(atx.NodeId, atx)
	if err != nil {
		return err
	}
	db.log.Debug("finished storing atx %v, in epoch %v", atx.ShortId(), ech)

	return nil
}

func (db *ActivationDb) storeAtxUnlocked(atx *types.ActivationTx) error {
	b, err := types.InterfaceToBytes(atx.Nipst)
	if err != nil {
		return err
	}
	db.nipstLock.Lock()
	err = db.nipsts.Put(atx.Id().Bytes(), b)
	db.nipstLock.Unlock()
	if err != nil {
		return err
	}
	//todo: think of how to break down the object better
	atx.Nipst = nil

	b, err = types.AtxAsBytes(atx)
	if err != nil {
		return err
	}

	err = db.atxs.Put(atx.Id().Bytes(), b)
	if err != nil {
		return err
	}
	return nil
}

func (db *ActivationDb) GetNipst(atxId types.AtxId) (*types.NIPST, error) {
	db.nipstLock.RLock()
	bts, err := db.nipsts.Get(atxId.Bytes())
	db.nipstLock.RUnlock()
	if err != nil {
		return nil, err
	}
	npst := types.NIPST{}
	err = types.BytesToInterface(bts, &npst)
	if err != nil {
		return nil, err
	}
	return &npst, nil
}

func epochCounterKey(ech types.EpochId) []byte {
	return append(ech.ToBytes(), common.Uint64ToBytes(uint64(CounterKey))...)
}

// incValidAtxCounter increases the number of active ids seen for epoch ech. Use only under db.lock.
func (db *ActivationDb) incValidAtxCounter(ech types.EpochId) error {
	key := epochCounterKey(ech)
	val, err := db.atxs.Get(key)
	if err != nil {
		db.log.Debug("incrementing epoch %v ATX counter to 1", ech)
		return db.atxs.Put(key, common.Uint32ToBytes(1))
	}
	db.log.Debug("incrementing epoch %v ATX counter to %v", ech, common.BytesToUint32(val)+1)
	return db.atxs.Put(key, common.Uint32ToBytes(common.BytesToUint32(val)+1))
}

// ActiveSetSize returns the active set size stored in db for epoch ech
func (db *ActivationDb) ActiveSetSize(epochId types.EpochId) (uint32, error) {
	key := epochCounterKey(epochId)
	db.RLock()
	val, err := db.atxs.Get(key)
	db.RUnlock()
	if err != nil {
		return 0, fmt.Errorf("could not fetch active set size from cache: %v", err)
	}
	return common.BytesToUint32(val), nil
}

type atxIdAndLayer struct {
	AtxId   types.AtxId
	LayerId types.LayerID
}

// addAtxToEpoch adds atx to epoch epochId
// this function is not thread safe and needs to be called under a global lock
func (db *ActivationDb) updatePosAtxIfNeeded(atx *types.ActivationTx) error {
	currentIdAndLayer, err := db.getCurrentAtxIdAndLayer()
	if err != nil && err != database.ErrNotFound {
		return fmt.Errorf("failed to get current ATX ID and layer: %v", err)
	}
	if err == nil && currentIdAndLayer.LayerId >= atx.PubLayerIdx {
		return nil
	}

	newIdAndLayer := atxIdAndLayer{
		AtxId:   atx.Id(),
		LayerId: atx.PubLayerIdx,
	}
	idAndLayerBytes, err := types.InterfaceToBytes(&newIdAndLayer)
	if err != nil {
		return fmt.Errorf("failed to marshal posAtx ID and layer: %v", err)
	}

	err = db.atxs.Put([]byte(posAtxKey), idAndLayerBytes)
	if err != nil {
		return fmt.Errorf("failed to store posAtx ID and layer: %v", err)
	}
	return nil
}

func (db ActivationDb) getCurrentAtxIdAndLayer() (atxIdAndLayer, error) {
	posAtxBytes, err := db.atxs.Get([]byte(posAtxKey))
	if err != nil {
		return atxIdAndLayer{}, err
	}
	var currentIdAndLayer atxIdAndLayer
	err = types.BytesToInterface(posAtxBytes, &currentIdAndLayer)
	if err != nil {
		return atxIdAndLayer{}, fmt.Errorf("failed to unmarshal posAtx ID and layer: %v", err)
	}
	return currentIdAndLayer, nil
}

func getNodeIdKey(id types.NodeId) []byte {
	return []byte(id.Key)
}

// addAtxToNodeId inserts activation atx id by node
func (db *ActivationDb) addAtxToNodeId(nodeId types.NodeId, atx *types.ActivationTx) error {
	key := getNodeIdKey(nodeId)
	err := db.atxs.Put(key, atx.Id().Bytes())
	if err != nil {
		return fmt.Errorf("failed to store ATX ID for node: %v", err)
	}
	return nil
}

// GetNodeLastAtxId returns the last atx id that was received for node nodeId
func (db *ActivationDb) GetNodeLastAtxId(nodeId types.NodeId) (types.AtxId, error) {
	key := getNodeIdKey(nodeId)
	db.log.Debug("fetching atxIDs for node %v", nodeId.ShortString())

	id, err := db.atxs.Get(key)
	if err != nil {
		return *types.EmptyAtxId, err
	}
	return types.AtxId{Hash: common.BytesToHash(id)}, nil
}

// GetPosAtxId returns the best (highest layer id), currently known to this node, pos atx id
func (db *ActivationDb) GetPosAtxId(epochId types.EpochId) (types.AtxId, error) {
	idAndLayer, err := db.getCurrentAtxIdAndLayer()
	if err != nil {
		return *types.EmptyAtxId, err
	}
	if idAndLayer.LayerId.GetEpoch(db.LayersPerEpoch) != epochId {
		return types.AtxId{}, fmt.Errorf("current posAtx (epoch %v) does not belong to the requested epoch (%v)",
			idAndLayer.LayerId.GetEpoch(db.LayersPerEpoch), epochId)
	}
	return idAndLayer.AtxId, nil
}

// getAtxUnlocked gets the atx from db, this function is not thread safe and should be called under db lock
// this function returns a pointer to an atx and an error if failed to retrieve it
func (db *ActivationDb) getAtxUnlocked(id types.AtxId) (*types.ActivationTx, error) {
	b, err := db.atxs.Get(id.Bytes())
	if err != nil {
		return nil, err
	}
	atx, err := types.BytesAsAtx(b)
	if err != nil {
		return nil, err
	}
	return atx, nil
}

// GetAtx returns the atx by the given id. this function is thread safe and will return error if the id is not found in the
// atx db
func (db *ActivationDb) GetAtx(id types.AtxId) (*types.ActivationTx, error) {
	if id == *types.EmptyAtxId {
		return nil, errors.New("trying to fetch empty atx id")
	}

	t := time.Now()
	if atx, gotIt := db.atxCache.Get(id); gotIt {
		//db.log.Info("running GetAtx (%v) took %v read from cache: %v", id, time.Since(t), true)
		return atx, nil
	}
	db.RLock()
	b, err := db.atxs.Get(id.Bytes())
	db.RUnlock()
	if err != nil {
		return nil, err
	}
	atx, err := types.BytesAsAtx(b)
	if err != nil {
		return nil, err
	}
	db.atxCache.Add(id, atx)
	db.log.Info("running GetAtx (%v) took %v read from cache: %v", id, time.Since(t), false)
	return atx, nil
}

// IsIdentityActive returns whether edId is active for the epoch of layer layer.
// it returns error if no associated atx is found in db
func (db *ActivationDb) IsIdentityActive(edId string, layer types.LayerID) (bool, types.AtxId, error) {
	// TODO: genesis flow should decide what we want to do here
	if layer.GetEpoch(db.LayersPerEpoch) == 0 {
		return true, *types.EmptyAtxId, nil
	}

	epoch := layer.GetEpoch(db.LayersPerEpoch)
	nodeId, err := db.ids.GetIdentity(edId)
	if err != nil { // means there is no such identity
		db.log.Error("IsIdentityActive erred while getting identity err=%v", err)
		return false, *types.EmptyAtxId, nil
	}
	atxId, err := db.GetNodeLastAtxId(nodeId)
	if err != nil {
		db.log.Error("IsIdentityActive erred while getting last node atx id err=%v", err)
		return false, *types.EmptyAtxId, err
	}
	atx, err := db.GetAtx(atxId)
	if err != nil {
		db.log.With().Error("IsIdentityActive erred while getting atx", log.AtxId(atxId.ShortId()), log.Err(err))
		return false, *types.EmptyAtxId, nil
	}

	lastAtxTargetEpoch := atx.TargetEpoch(db.LayersPerEpoch)
	if lastAtxTargetEpoch < epoch {
		db.log.With().Info("IsIdentityActive latest atx is too old", log.Uint64("expected", uint64(epoch)),
			log.Uint64("actual", uint64(lastAtxTargetEpoch)), log.AtxId(atx.ShortId()))
		return false, *types.EmptyAtxId, nil
	}

	if lastAtxTargetEpoch > epoch {
		// This could happen if we already published the ATX for the next epoch, so we check the previous one as well
		if atx.PrevATXId == *types.EmptyAtxId {
			db.log.With().Info("IsIdentityActive latest atx is too new but no previous atx", log.AtxId(atxId.ShortId()))
			return false, *types.EmptyAtxId, nil
		}
		prevAtxId := atx.PrevATXId
		atx, err = db.GetAtx(prevAtxId)
		if err != nil {
			db.log.With().Error("IsIdentityActive erred while getting atx for second newest", log.AtxId(prevAtxId.ShortId()), log.Err(err))
			return false, *types.EmptyAtxId, nil
		}
	}

	// lastAtxTargetEpoch = epoch
	return atx.TargetEpoch(db.LayersPerEpoch) == epoch, atx.Id(), nil
}
