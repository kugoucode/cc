package migrate

import (
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jxwr/cc/log"
	"github.com/jxwr/cc/meta"
	"github.com/jxwr/cc/redis"
	"github.com/jxwr/cc/streams"
	"github.com/jxwr/cc/topo"
)

const (
	StateRunning int32 = iota
	StatePausing
	StatePaused
	StateCancelling
	StateCancelled
	StateDone
	StateTargetNodeFailure
)

var stateNames = map[int32]string{
	StateRunning:           "Migrating",
	StatePausing:           "Pausing",
	StatePaused:            "Paused",
	StateCancelling:        "Cancelling",
	StateCancelled:         "Cancelled",
	StateDone:              "Done",
	StateTargetNodeFailure: "TargetNodeFailure",
}

type MigrateTask struct {
	ranges           []topo.Range
	source           atomic.Value
	target           atomic.Value
	currRangeIndex   int // current range index
	currSlot         int // current slot
	state            int32
	backupReplicaSet *topo.ReplicaSet
	lastPubTime      time.Time
}

func NewMigrateTask(sourceRS, targetRS *topo.ReplicaSet, ranges []topo.Range) *MigrateTask {
	t := &MigrateTask{
		ranges:      ranges,
		state:       StateRunning,
		lastPubTime: time.Now(),
	}
	t.ReplaceSourceReplicaSet(sourceRS)
	t.ReplaceTargetReplicaSet(targetRS)
	return t
}

func (t *MigrateTask) TaskName() string {
	return fmt.Sprintf("Mig(%s->%s)", t.SourceNode().Id, t.TargetNode().Id)
}

func (t *MigrateTask) setSlotToNode(rs *topo.ReplicaSet, slot int, targetId string) error {
	// 先清理从节点的MIGRATING状态
	for _, node := range rs.Slaves() {
		if node.Fail {
			continue
		}
		err := redis.SetSlot(node.Addr(), slot, redis.SLOT_NODE, targetId)
		if err != nil {
			return err
		}
	}
	err := redis.SetSlot(rs.Master().Addr(), slot, redis.SLOT_NODE, targetId)
	if err != nil {
		return err
	}
	return nil
}

func (t *MigrateTask) setSlotStable(rs *topo.ReplicaSet, slot int) error {
	// 先清理从节点的MIGRATING状态
	for _, node := range rs.Slaves() {
		if node.Fail {
			continue
		}
		err := redis.SetSlot(node.Addr(), slot, redis.SLOT_STABLE, "")
		if err != nil {
			return err
		}
	}
	err := redis.SetSlot(rs.Master().Addr(), slot, redis.SLOT_STABLE, "")
	if err != nil {
		return err
	}
	return nil
}

/// 迁移slot过程:
/// 1. 标记所有Source分片节点为MIGRATING
/// 2. 标记Target分片Master为IMPORTING
/// 3. 从Source分片Master取keys迁移，直到空，数据迁移完成
/// 4. 设置Target的Slave的slot归属到Target
/// 5. 设置Target的Master的slot归属到Target
/// 6. 设置Source所有节点的slot归属到Target
/// 命令:
/// 1. <Source Slaves> setslot $slot MIGRATING $targetId
/// 2. <Source Master> setslot $slot MIGRATING $targetId
/// 3. <Target Master> setslot $slot IMPORTING $sourceId
/// ... migrating all keys
/// 4. <Target Slaves> setslot $slot node $targetId
/// 5. <Target Master> setslot $slot node $targetId
/// 6. <Source Slaves> setslot $slot node $targetId
/// 7. <Source Master> setslot $slot node $targetId
func (t *MigrateTask) migrateSlot(slot int, keysPer int) (int, error) {
	rs := t.SourceReplicaSet()
	sourceNode := t.SourceNode()
	targetNode := t.TargetNode()

	// 需要将Source分片的所有节点标记为MIGRATING，最大限度避免从地域的读造成的数据不一致
	for _, node := range rs.AllNodes() {
		err := redis.SetSlot(node.Addr(), slot, redis.SLOT_MIGRATING, targetNode.Id)
		if err != nil {
			if strings.HasPrefix(err.Error(), "ERR I'm not the owner of hash slot") {
				log.Warningf(t.TaskName(), "mig: %s is not the owner of hash slot %d",
					sourceNode.Id, slot)
				srs := t.SourceReplicaSet()
				err2 := t.setSlotStable(srs, slot)
				if err2 != nil {
					log.Warningf(t.TaskName(), "mig: failed to clean MIGRATING state of source server.")
					return 0, err2
				}
				return 0, nil
			}
			return 0, err
		}
	}

	err := redis.SetSlot(targetNode.Addr(), slot, redis.SLOT_IMPORTING, sourceNode.Id)
	if err != nil {
		if strings.HasPrefix(err.Error(), "ERR I'm already the owner of hash slot") {
			log.Warningf(t.TaskName(), "mig: %s already the owner of hash slot %d",
				targetNode.Id, slot)
			// 逻辑到此，说明Target已经包含该slot，但是Source处于Migrating状态
			// 迁移实际已经完成，需要清理Source的Migrating状态
			srs := t.SourceReplicaSet()
			err = t.setSlotToNode(srs, slot, targetNode.Id)
			if err != nil {
				return 0, err
			}
			trs := t.TargetReplicaSet()
			err = t.setSlotToNode(trs, slot, targetNode.Id)
			return 0, err
		}
		return 0, err
	}

	/// 迁移的速度甚至迁移超时的配置可能都有不小问题，目前所有命令是短连接，且一次只迁移一个key

	// 一共迁移多少个key
	nkeys := 0
	app := meta.GetAppConfig()
	for {
		keys, err := redis.GetKeysInSlot(sourceNode.Addr(), slot, keysPer)
		if err != nil {
			return nkeys, err
		}
		for _, key := range keys {
			_, err := redis.Migrate(sourceNode.Addr(), targetNode.Ip, targetNode.Port, key, app.MigrateTimeout)
			if err != nil {
				return nkeys, err
			}
			nkeys++
		}
		if len(keys) == 0 {
			// 迁移完成，需要等SourceSlavess同步(DEL)完成，即SourceSlaves节点中该slot内已无key
			slaveSyncDone := true
			srs := t.SourceReplicaSet()
			for _, node := range srs.AllNodes() {
				keys, err := redis.GetKeysInSlot(node.Addr(), slot, keysPer)
				if err != nil {
					return nkeys, err
				}
				if len(keys) != 0 {
					slaveSyncDone = false
				}
			}
			if !slaveSyncDone {
				return nkeys, fmt.Errorf("mig: source nodes not all empty, will retry.")
			}
			// 设置slot归属到新节点，该操作自动清理IMPORTING和MIGRATING状态
			// 如果设置的是Source节点，设置slot归属时，Redis会确保该slot中已无剩余的key
			trs := t.TargetReplicaSet()
			// 优先设置从节点，保证当主的数据分布还未广播到从节点时主挂掉，slot信息也不会丢失
			for _, node := range trs.Slaves() {
				if node.Fail {
					continue
				}
				err = redis.SetSlot(node.Addr(), slot, redis.SLOT_NODE, targetNode.Id)
				if err != nil {
					return nkeys, err
				}
			}
			// 该操作增加Epoch并广播出去
			err = redis.SetSlot(trs.Master().Addr(), slot, redis.SLOT_NODE, targetNode.Id)
			if err != nil {
				return nkeys, err
			}
			// 更新源节点上slot的归属
			srs = t.SourceReplicaSet()
			err = t.setSlotToNode(srs, slot, targetNode.Id)
			if err != nil {
				return nkeys, err
			}
			break
		}
	}

	return nkeys, nil
}

func (t *MigrateTask) streamPub(careSpeed bool) {
	data := &streams.MigrateStateStreamData{
		SourceId:       t.SourceNode().Id,
		TargetId:       t.TargetNode().Id,
		State:          stateNames[t.CurrentState()],
		Ranges:         t.ranges,
		CurrRangeIndex: t.currRangeIndex,
		CurrSlot:       t.currSlot,
	}
	if careSpeed {
		now := time.Now()
		if now.Sub(t.lastPubTime) > 100*time.Millisecond {
			streams.MigrateStateStream.Pub(data)
			t.lastPubTime = now
		}
	} else {
		streams.MigrateStateStream.Pub(data)
	}
}

func (t *MigrateTask) Run() {
	for i, r := range t.ranges {
		if r.Left < 0 {
			r.Left = 0
		}
		if r.Right > 16383 {
			r.Right = 16383
		}
		t.currRangeIndex = i
		t.currSlot = r.Left
		for t.currSlot <= r.Right {
			t.streamPub(true)
			// 尽量在迁移完一个完整Slot或遇到错误时，再进行状态的转换
			// 只是尽量而已，还是有可能停在一个Slot内部

			if t.CurrentState() == StateCancelling {
				t.SetState(StateCancelled)
				t.streamPub(false)
				return
			}

			// 暂停，sleep一会继续检查
			if t.CurrentState() == StatePausing {
				t.SetState(StatePaused)
			}
			if t.CurrentState() == StatePaused {
				time.Sleep(100 * time.Millisecond)
				continue
			}

			// 正常运行
			app := meta.GetAppConfig()
			nkeys, err := t.migrateSlot(t.currSlot, app.MigrateKeysEachTime)
			if err != nil {
				log.Warningf(t.TaskName(),
					"mig: Migrate slot %d error, %d keys have done, %v",
					t.currSlot, nkeys, err)
				time.Sleep(500 * time.Millisecond)
			} else {
				log.Infof(t.TaskName(), "mig: Migrate slot %d done, total %d keys",
					t.currSlot, nkeys)
				t.currSlot++
			}
		}
	}
	t.currSlot--
	t.SetState(StateDone)
	t.streamPub(false)
}

func (t *MigrateTask) BackupReplicaSet() *topo.ReplicaSet {
	return t.backupReplicaSet
}

func (t *MigrateTask) SetBackupReplicaSet(rs *topo.ReplicaSet) {
	t.backupReplicaSet = rs
}

// 下面方法在MigrateManager中使用，需要原子操作

func (t *MigrateTask) CurrentState() int32 {
	return atomic.LoadInt32(&t.state)
}

func (t *MigrateTask) SetState(state int32) {
	atomic.StoreInt32(&t.state, state)
}

func (t *MigrateTask) ReplaceSourceReplicaSet(rs *topo.ReplicaSet) {
	t.source.Store(rs)
}

func (t *MigrateTask) ReplaceTargetReplicaSet(rs *topo.ReplicaSet) {
	t.target.Store(rs)
}

func (t *MigrateTask) SourceReplicaSet() *topo.ReplicaSet {
	return t.source.Load().(*topo.ReplicaSet)
}

func (t *MigrateTask) TargetReplicaSet() *topo.ReplicaSet {
	return t.target.Load().(*topo.ReplicaSet)
}

func (t *MigrateTask) SourceNode() *topo.Node {
	return t.SourceReplicaSet().Master()
}

func (t *MigrateTask) TargetNode() *topo.Node {
	return t.TargetReplicaSet().Master()
}
