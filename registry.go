package beehive

import (
	"encoding/gob"
	"errors"
	"fmt"
	"sync"

	"github.com/kandoo/beehive/Godeps/_workspace/src/github.com/coreos/etcd/raft/raftpb"
	"github.com/kandoo/beehive/Godeps/_workspace/src/github.com/golang/glog"
	bhgob "github.com/kandoo/beehive/gob"
	"github.com/kandoo/beehive/raft"
)

var (
	ErrUnsupportedRequest = errors.New("unsupported request")
	ErrInvalidParam       = errors.New("invalid parameter")
	ErrNoSuchHive         = errors.New("no such hive")
	ErrDuplicateHive      = errors.New("dupblicate hive")
	ErrNoSuchBee          = errors.New("no such bee")
	ErrDuplicateBee       = errors.New("duplicate bee")
)

// noOp is a barrier: a raft request to make sure all the updates are
// applied to store.
type noOp struct{}

// newHiveID is the registry request to create a unique 64-bit hive ID.
type newHiveID struct {
	Addr string
}

// newBeeID is the registry request to create a unique 64-bit bee ID.
type newBeeID struct{}

// BeeInfo stores the metadata about a bee.
type BeeInfo struct {
	ID       uint64 `json:"id"`
	Hive     uint64 `json:"hive"`
	App      string `json:"app"`
	Colony   Colony `json:"colony"`
	Detached bool   `json:"detached"`
}

// addBee is the registery request to add a new bee.
type addBee BeeInfo

// delBee is the registery request to delete a bee.
type delBee uint64

// moveBee is the registery request to move a bee from a hive to another hive.
type moveBee struct {
	ID       uint64
	FromHive uint64
	ToHive   uint64
}

// Colony is a registery request to update a colony.
type updateColony struct {
	Old Colony
	New Colony
}

// lockMappedCell locks a mapped cell for a colony.
type lockMappedCell struct {
	Colony Colony
	App    string
	Cells  MappedCells
}

// TransferLocks transfers cells of a colony to another colony.
type transferCells struct {
	From Colony
	To   Colony
}

type registry struct {
	m    sync.RWMutex
	name string

	HiveID uint64
	BeeID  uint64
	Hives  map[uint64]HiveInfo
	Bees   map[uint64]BeeInfo
	Store  cellStore
}

func newRegistry(name string) *registry {
	return &registry{
		name:   name,
		HiveID: 1, // We need to start from one to preserve the first hive's ID.
		BeeID:  0,
		Hives:  make(map[uint64]HiveInfo),
		Bees:   make(map[uint64]BeeInfo),
		Store:  newCellStore(),
	}
}

func (r *registry) Save() ([]byte, error) {
	r.m.RLock()
	defer r.m.RUnlock()
	glog.V(2).Info("registry saved")
	return bhgob.Encode(r)
}

func (r *registry) Restore(b []byte) error {
	r.m.Lock()
	defer r.m.Unlock()
	glog.V(2).Info("registry restored")
	return bhgob.Decode(r, b)
}

func (r *registry) Apply(req interface{}) (interface{}, error) {
	r.m.Lock()
	defer r.m.Unlock()

	switch tr := req.(type) {
	case noOp:
		return nil, nil
	case newHiveID:
		return r.newHiveID(tr.Addr), nil
	case newBeeID:
		return r.newBeeID(), nil
	case addBee:
		return nil, r.addBee(BeeInfo(tr))
	case delBee:
		return nil, r.delBee(uint64(tr))
	case moveBee:
		return nil, r.moveBee(tr)
	case updateColony:
		return nil, r.updateColony(tr)
	case lockMappedCell:
		return r.lock(tr)
	case transferCells:
		return nil, r.transfer(tr)
	}

	glog.Errorf("%v cannot handle %v", r, req)
	return nil, ErrUnsupportedRequest
}

func (r *registry) String() string {
	return fmt.Sprintf("registry for %v", r.name)
}

func (r *registry) ApplyConfChange(cc raftpb.ConfChange,
	n raft.NodeInfo) error {

	glog.V(2).Infof("%v applies conf change %#v for %v", r, cc, n)
	switch cc.Type {
	case raftpb.ConfChangeAddNode:
		if n.ID != cc.NodeID {
			glog.Fatalf("invalid data in the config change: %v != %v", n, cc.NodeID)
		}
		if n.Addr != "" {
			r.addHive(HiveInfo(n))
		}
		glog.V(2).Infof("%v adds hive %v@%v", r, n.ID, n.Addr)

	case raftpb.ConfChangeRemoveNode:
		r.delHive(cc.NodeID)
		glog.V(2).Infof("%v deletes hive %v", r, cc.NodeID)
	}
	return nil
}

func (r *registry) newHiveID(addr string) uint64 {
	r.HiveID++
	if addr != "" {
		r.addHive(HiveInfo{ID: r.HiveID, Addr: addr})
	}
	glog.V(2).Infof("%v allocates new hive ID %v", r, r.HiveID)
	return r.HiveID
}

func (r *registry) newBeeID() uint64 {
	r.BeeID++
	glog.V(2).Infof("%v allocates new bee ID %v", r, r.BeeID)
	return r.BeeID
}

func (r *registry) delHive(id uint64) error {
	if _, ok := r.Hives[id]; !ok {
		return fmt.Errorf("no such hive %v", id)
	}
	delete(r.Hives, id)
	return nil
}

func (r *registry) initHives(hives map[uint64]HiveInfo) error {
	r.m.Lock()
	defer r.m.Unlock()

	for _, h := range hives {
		if err := r.addHive(h); err != nil {
			return err
		}
	}
	return nil
}

func (r *registry) addHive(info HiveInfo) error {
	glog.V(2).Infof("%v sets hive %v's address to %v", r, info.ID, info.Addr)
	for _, h := range r.Hives {
		if h.Addr == info.Addr && h.ID != info.ID {
			glog.Fatalf("%v has duplicate address %v for hives %v and %v", r,
				info.Addr, info.ID, h.ID)
		}
	}
	r.Hives[info.ID] = info
	return nil
}

func (r *registry) addBee(info BeeInfo) error {
	glog.V(2).Infof("%v add bee %v (detached=%v) for %v with %v,", r, info.ID,
		info.Detached, info.App, info.Colony)

	if info.ID == Nil {
		glog.Fatalf("invalid bee info: %v", info)
	}

	if _, ok := r.Bees[info.ID]; ok {
		return ErrDuplicateBee
	}
	if r.BeeID < info.ID {
		glog.Fatalf("%v has invalid bee ID: %v < %v", r, info.ID, r.HiveID)
	}
	r.Bees[info.ID] = info
	return nil
}

func (r *registry) delBee(id uint64) error {
	glog.V(2).Infof("%v removes bee %v", r, id)
	if _, ok := r.Bees[id]; !ok {
		return ErrNoSuchBee
	}
	delete(r.Bees, id)
	return nil
}

func (r *registry) moveBee(m moveBee) error {
	b, ok := r.Bees[m.ID]
	if !ok {
		return ErrNoSuchBee
	}

	if b.Hive != m.FromHive {
		return ErrInvalidParam
	}

	if m.FromHive == m.ToHive {
		return nil
	}

	b.Hive = m.ToHive
	r.Bees[m.ID] = b
	return nil
}

func (r *registry) updateColony(up updateColony) error {
	if up.New.IsNil() || up.Old.IsNil() {
		return ErrInvalidParam
	}

	glog.V(2).Infof("%v updates %v with %v", r, up.Old, up.New)
	b := r.mustFindBee(up.New.Leader)
	if err := r.Store.updateColony(b.App, up.Old, up.New); err != nil {
		return err
	}

	if up.Old.Leader != up.New.Leader {
		b = r.mustFindBee(up.Old.Leader)
		if up.New.Contains(up.Old.Leader) {
			b.Colony = up.New
		} else {
			b.Colony = Colony{}
		}
		r.Bees[up.Old.Leader] = b
	}

	for _, f := range up.Old.Followers {
		if !up.New.Contains(f) {
			b = r.mustFindBee(f)
			b.Colony = Colony{}
			r.Bees[f] = b
		}
	}

	for _, f := range up.New.Followers {
		b = r.mustFindBee(f)
		b.Colony = up.New
		r.Bees[f] = b
	}

	b = r.mustFindBee(up.New.Leader)
	b.Colony = up.New
	r.Bees[up.New.Leader] = b

	return nil
}

func (r *registry) mustFindBee(id uint64) BeeInfo {
	info, ok := r.Bees[id]
	if !ok {
		glog.Fatalf("cannot find bee %v", id)
	}
	return info
}

func (r *registry) lock(l lockMappedCell) (Colony, error) {
	if l.Colony.Leader == 0 {
		return Colony{}, ErrInvalidParam
	}

	locked := false
	openk := make(MappedCells, 0, 10)
	for _, k := range l.Cells {
		c, ok := r.Store.colony(l.App, k)
		if !ok {
			if locked {
				r.Store.assign(l.App, k, l.Colony)
			} else {
				openk = append(openk, k)
			}
			continue
		}

		if locked && !c.Equals(l.Colony) {
			// FIXME(soheil): Fix this bug after refactoring the code.
			glog.Fatal("TODO registery conflict between colonies")
		}

		locked = true
		l.Colony = c
	}

	if locked {
		for _, k := range openk {
			r.Store.assign(l.App, k, l.Colony)
		}
		return l.Colony, nil
	}

	for _, k := range l.Cells {
		r.Store.assign(l.App, k, l.Colony)
	}
	return l.Colony, nil
}

func (r *registry) transfer(t transferCells) error {
	i, ok := r.Bees[t.From.Leader]
	if !ok {
		return ErrNoSuchBee
	}
	keys := r.Store.cells(t.From.Leader)
	if len(keys) == 0 {
		return ErrInvalidParam
	}
	for _, k := range keys {
		r.Store.assign(i.App, k, t.To)
	}
	return nil
}

func (r *registry) hives() []HiveInfo {
	r.m.RLock()
	hives := make([]HiveInfo, 0, len(r.Hives))
	for _, h := range r.Hives {
		hives = append(hives, h)
	}
	r.m.RUnlock()
	return hives
}

func (r *registry) hive(id uint64) (HiveInfo, error) {
	r.m.RLock()
	i, ok := r.Hives[id]
	r.m.RUnlock()
	if !ok {
		return i, ErrNoSuchHive
	}
	return i, nil
}

func (r *registry) bees() []BeeInfo {
	r.m.RLock()
	bees := make([]BeeInfo, 0, len(r.Bees))
	for _, b := range r.Bees {
		bees = append(bees, b)
	}
	r.m.RUnlock()
	return bees
}

func (r *registry) beesOfHive(id uint64) []BeeInfo {
	r.m.RLock()
	var bees []BeeInfo
	for _, b := range r.Bees {
		if b.Hive == id {
			bees = append(bees, b)
		}
	}
	r.m.RUnlock()
	return bees
}

func (r *registry) bee(id uint64) (BeeInfo, error) {
	r.m.RLock()
	i, ok := r.Bees[id]
	r.m.RUnlock()
	if !ok {
		return i, ErrNoSuchBee
	}
	return i, nil
}

func (r *registry) beeAndHive(id uint64) (BeeInfo, HiveInfo, error) {
	r.m.RLock()
	defer r.m.RUnlock()
	bi, ok := r.Bees[id]
	if !ok {
		return bi, HiveInfo{}, ErrNoSuchBee
	}

	if bi.ID != id {
		glog.Fatalf("bee %v has invalid info: %#v", id, bi)
	}

	hi, ok := r.Hives[bi.Hive]
	if !ok {
		return bi, hi, ErrNoSuchHive
	}
	return bi, hi, nil
}

func (r *registry) beeForCells(app string, cells MappedCells) (info BeeInfo,
	hasAll bool, err error) {

	r.m.RLock()
	defer r.m.RUnlock()

	hasAll = true
	for _, k := range cells {
		col, ok := r.Store.colony(app, k)
		if !ok {
			hasAll = false
			continue
		}

		if info.ID == 0 {
			info = r.Bees[col.Leader]
			if info.ID != col.Leader {
				glog.Fatalf("bee %b has an invalid info %#v", col.Leader, info)
			}
		} else if info.ID != col.Leader {
			// Incosistencies should be handled by consensus.
			hasAll = false
		}

		if !hasAll {
			return info, hasAll, nil
		}
	}
	if info.ID == 0 {
		return info, hasAll, ErrNoSuchBee
	}
	return info, hasAll, nil
}

func init() {
	gob.Register(noOp{})
	gob.Register(newBeeID{})
	gob.Register(newHiveID{})
	gob.Register(HiveInfo{})
	gob.Register([]HiveInfo{})
	gob.Register(BeeInfo{})
	gob.Register(addBee{})
	gob.Register(delBee(0))
	gob.Register(updateColony{})
	gob.Register(lockMappedCell{})
	gob.Register(transferCells{})
	gob.Register(cellStore{})
}
