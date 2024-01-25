package shardkv

import (
	"6.824/labrpc"
	"6.824/shardctrler"
	"bytes"
	"log"
	"sync/atomic"
	"time"
)
import "6.824/raft"
import "sync"
import "6.824/labgob"

const (
	UpConfigLoopInterval = 100 * time.Millisecond
	GetTimeout           = 500 * time.Millisecond
	AppOrPutTimeout      = 500 * time.Millisecond
	UpConfigTimeout      = 500 * time.Millisecond
	AddShardsTimeout     = 500 * time.Millisecond
	RemoveShardsTimeout  = 500 * time.Millisecond
)

type Shard struct {
	KvMap     map[string]string
	ConfigNum int
}

type Op struct {
	// Your definitions here.
	// Field names must start with capital letters,
	// otherwise RPC will break.
	ClientId int64
	SeqId    int
	Key      string
	Value    string
	OpType   Operation
	UpConfig shardctrler.Config
	ShardId  int
	Shard    Shard
	SeqMap   map[int64]int
}

type ShardKV struct {
	mu           sync.Mutex
	me           int
	rf           *raft.Raft
	applyCh      chan raft.ApplyMsg
	make_end     func(string) *labrpc.ClientEnd
	gid          int
	ctrlers      []*labrpc.ClientEnd
	maxRaftState int // snapshot if log grows this big

	// Your definitions here.
	dead          int32
	shardsPersist []Shard
	SeqMap        map[int64]int
	Config        shardctrler.Config
	LastConfig    shardctrler.Config
	waitChMap     map[int]chan OpReply
	sck           *shardctrler.Clerk
}

type OpReply struct {
	ClientId int64
	SeqId    int
	Err      Err
}

func (kv *ShardKV) Get(args *GetArgs, reply *GetReply) {
	// Your code here.
	shardId := key2shard(args.Key)
	kv.mu.Lock()
	if kv.Config.Shards[shardId] != kv.gid {
		reply.Err = ErrWrongGroup
	} else if kv.shardsPersist[shardId].KvMap == nil {
		reply.Err = ShardNotArrived
	}
	kv.mu.Unlock()
	if reply.Err == ErrWrongGroup || reply.Err == ShardNotArrived {
		return
	}
	command := Op{
		OpType:   GetType,
		Key:      args.Key,
		SeqId:    args.RequestId,
		ClientId: args.ClientId,
	}
	err := kv.startCommand(command, GetTimeout)
	if err != OK {
		reply.Err = err
		return
	}
	kv.mu.Lock()

	if kv.Config.Shards[shardId] != kv.gid {
		reply.Err = ErrWrongGroup
	} else if kv.shardsPersist[shardId].KvMap == nil {
		reply.Err = ShardNotArrived
	} else {
		reply.Err = OK
		reply.Value = kv.shardsPersist[shardId].KvMap[args.Key]
	}
	kv.mu.Unlock()
	return
}

func (kv *ShardKV) PutAppend(args *PutAppendArgs, reply *PutAppendReply) {
	// Your code here.
	shardId := key2shard(args.Key)
	kv.mu.Lock()
	if kv.Config.Shards[shardId] != kv.gid {
		reply.Err = ErrWrongGroup
	} else if kv.shardsPersist[shardId].KvMap == nil {
		reply.Err = ShardNotArrived
	}
	kv.mu.Unlock()
	if reply.Err == ErrWrongGroup || reply.Err == ShardNotArrived {
		return
	}
	command := Op{
		OpType:   args.Op,
		Key:      args.Key,
		Value:    args.Value,
		SeqId:    args.RequestId,
		ClientId: args.ClientId,
	}
	reply.Err = kv.startCommand(command, AppOrPutTimeout)
	return
}

func (kv *ShardKV) AddShard(args *SendShardArg, reply *AddShardReply) {
	command := Op{
		OpType:   AddShardType,
		ClientId: args.ClientId,
		SeqId:    args.RequestId,
		ShardId:  args.ShardId,
		Shard:    args.Shard,
		SeqMap:   args.LastAppliedRequestId,
	}
	reply.Err = kv.startCommand(command, AddShardsTimeout)
}

func (kv *ShardKV) startCommand(command Op, timeoutPeriod time.Duration) Err {
	kv.mu.Lock()
	index, _, isLeader := kv.rf.Start(command)
	if !isLeader {
		kv.mu.Unlock()
		return ErrWrongLeader
	}
	ch := kv.getWaitCh(index)
	kv.mu.Unlock()

	timer := time.NewTicker(timeoutPeriod)
	defer timer.Stop()

	select {
	case re := <-ch:
		kv.mu.Lock()
		delete(kv.waitChMap, index)
		if re.SeqId != command.SeqId || re.ClientId != command.ClientId {
			kv.mu.Unlock()
			return ErrInconsistentData
		}
		kv.mu.Unlock()
		return re.Err

	case <-timer.C:
		return ErrOverTime
	}
}

//
// the tester calls Kill() when a ShardKV instance won't
// be needed again. you are not required to do anything
// in Kill(), but it might be convenient to (for example)
// turn off debug output from this instance.
//
func (kv *ShardKV) Kill() {
	atomic.StoreInt32(&kv.dead, 1)
	kv.rf.Kill()
	// Your code here, if desired.
}

func (kv *ShardKV) killed() bool {
	z := atomic.LoadInt32(&kv.dead)
	return z == 1
}

//
// servers[] contains the ports of the servers in this group.
//
// me is the index of the current server in servers[].
//
// the k/v server should store snapshots through the underlying Raft
// implementation, which should call persister.SaveStateAndSnapshot() to
// atomically save the Raft state along with the snapshot.
//
// the k/v server should snapshot when Raft's saved state exceeds
// maxraftstate bytes, in order to allow Raft to garbage-collect its
// log. if maxraftstate is -1, you don't need to snapshot.
//
// gid is this group's GID, for interacting with the shardctrler.
//
// pass ctrlers[] to shardctrler.MakeClerk() so you can send
// RPCs to the shardctrler.
//
// make_end(servername) turns a server name from a
// Config.Groups[gid][i] into a labrpc.ClientEnd on which you can
// send RPCs. You'll need this to send RPCs to other groups.
//
// look at client.go for examples of how to use ctrlers[]
// and make_end() to send RPCs to the group owning a specific shard.
//
// StartServer() must return quickly, so it should start goroutines
// for any long-running work.
//
func StartServer(servers []*labrpc.ClientEnd, me int, persister *raft.Persister, maxraftstate int, gid int,
	ctrlers []*labrpc.ClientEnd, make_end func(string) *labrpc.ClientEnd) *ShardKV {
	// call labgob.Register on structures you want
	// Go's RPC library to marshall/unmarshall.
	labgob.Register(Op{})

	kv := new(ShardKV)
	kv.me = me
	kv.maxRaftState = maxraftstate
	kv.make_end = make_end
	kv.gid = gid
	kv.ctrlers = ctrlers

	// Your initialization code here.

	// Use something like this to talk to the shardctrler:
	// kv.mck = shardctrler.MakeClerk(kv.ctrlers)
	kv.shardsPersist = make([]Shard, shardctrler.NShards)
	kv.SeqMap = make(map[int64]int)

	// remember add sck and waitchmap
	kv.sck = shardctrler.MakeClerk(kv.ctrlers)
	kv.waitChMap = make(map[int]chan OpReply)

	snapshot := persister.ReadSnapshot()
	if len(snapshot) > 0 {
		kv.DecodeSnapShot(snapshot)
	}

	kv.applyCh = make(chan raft.ApplyMsg)
	kv.rf = raft.Make(servers, me, persister, kv.applyCh)

	go kv.applyMsgHandlerLoop()
	go kv.ConfigDetectedLoop()

	return kv
}

func (kv *ShardKV) DecodeSnapShot(snapshot []byte) {
	if snapshot == nil || len(snapshot) < 1 {
		return
	}
	r := bytes.NewBuffer(snapshot)
	d := labgob.NewDecoder(r)

	var shardsPersist []Shard
	var SeqMap map[int64]int
	var MaxRaftState int
	var Config, LastConfig shardctrler.Config
	if d.Decode(&shardsPersist) == nil && d.Decode(&SeqMap) == nil &&
		d.Decode(&MaxRaftState) == nil && d.Decode(&Config) == nil && d.Decode(&LastConfig) == nil {
		kv.shardsPersist = shardsPersist
		kv.SeqMap = SeqMap
		kv.maxRaftState = MaxRaftState
		kv.Config = Config
		kv.LastConfig = LastConfig
	} else {
		log.Fatalf("[Server(%v) Failed to decode snapshot!\n", kv.me)
	}
}

func (kv *ShardKV) ConfigDetectedLoop() {
	kv.mu.Lock()
	curConfig := kv.Config
	rf := kv.rf
	kv.mu.Unlock()

	for !kv.killed() {
		if _, isLeader := rf.GetState(); !isLeader {
			time.Sleep(UpConfigLoopInterval)
			continue
		}
		kv.mu.Lock()

		if !kv.allSent() {
			SeqMap := make(map[int64]int)
			for k, v := range kv.SeqMap {
				SeqMap[k] = v
			}
			for shardId, gid := range kv.LastConfig.Shards {
				if gid == kv.gid && kv.Config.Shards[shardId] != kv.gid &&
					kv.shardsPersist[shardId].ConfigNum < kv.Config.Num {
					sendDate := kv.cloneShard(kv.Config.Num, kv.shardsPersist[shardId].KvMap)
					args := SendShardArg{
						LastAppliedRequestId: SeqMap,
						ShardId:              shardId,
						Shard:                sendDate,
						ClientId:             int64(gid),
						RequestId:            kv.Config.Num,
					}
					serversList := kv.Config.Groups[kv.Config.Shards[shardId]]
					servers := make([]*labrpc.ClientEnd, len(serversList))
					for i, name := range serversList {
						servers[i] = kv.make_end(name)
					}
					go func(servers []*labrpc.ClientEnd, args *SendShardArg) {
						index := 0
						start := time.Now()
						for {
							var reply AddShardReply
							// need &args ?
							ok := servers[index].Call("ShardKV.AddShard", args, &reply)
							// two time instead of 5 time
							if ok && reply.Err == OK || time.Now().Sub(start) >= 2*time.Second {
								kv.mu.Lock()
								command := Op{
									OpType:   RemoveShardType,
									ClientId: int64(kv.gid),
									SeqId:    kv.Config.Num,
									ShardId:  args.ShardId,
								}
								kv.mu.Unlock()
								kv.startCommand(command, RemoveShardsTimeout)
								break
							}
							index = (index + 1) % len(servers)
							if index == 0 {
								time.Sleep(UpConfigLoopInterval)
							}
						}
					}(servers, &args)
				}
			}
			kv.mu.Unlock()
			time.Sleep(UpConfigLoopInterval)
			continue
		}
		if !kv.allReceived() {
			kv.mu.Unlock()
			time.Sleep(UpConfigLoopInterval)
			continue
		}
		curConfig = kv.Config
		sck := kv.sck
		kv.mu.Unlock()

		newConfig := sck.Query(curConfig.Num + 1)
		if newConfig.Num != curConfig.Num+1 {
			time.Sleep(UpConfigLoopInterval)
			continue
		}
		command := Op{
			OpType:   UpConfigType,
			ClientId: int64(kv.gid),
			SeqId:    newConfig.Num,
			UpConfig: newConfig,
		}
		kv.startCommand(command, UpConfigTimeout)

	}
}

func (kv *ShardKV) allSent() bool {
	for shard, gid := range kv.LastConfig.Shards {
		if gid == kv.gid && kv.Config.Shards[shard] != kv.gid &&
			kv.shardsPersist[shard].ConfigNum < kv.Config.Num {
			return false
		}
	}
	return true
}

func (kv *ShardKV) allReceived() bool {
	for shard, gid := range kv.LastConfig.Shards {
		if gid != kv.gid && kv.Config.Shards[shard] == kv.gid &&
			kv.shardsPersist[shard].ConfigNum < kv.Config.Num {
			return false
		}
	}
	return true
}

func (kv *ShardKV) applyMsgHandlerLoop() {
	for {
		if kv.killed() {
			return
		}
		select {
		case msg := <-kv.applyCh:
			if msg.CommandValid {
				kv.mu.Lock()

				op := msg.Command.(Op)
				reply := OpReply{
					ClientId: op.ClientId,
					SeqId:    op.SeqId,
					Err:      OK,
				}
				if op.OpType == PutType || op.OpType == GetType ||
					op.OpType == AppendType {
					shardId := key2shard(op.Key)

					if kv.Config.Shards[shardId] != kv.gid {
						reply.Err = ErrWrongGroup
					} else if kv.shardsPersist[shardId].KvMap == nil {
						reply.Err = ShardNotArrived
					} else {
						if !kv.ifDuplicate(op.ClientId, op.SeqId) {
							kv.SeqMap[op.ClientId] = op.SeqId
							switch op.OpType {
							case PutType:
								kv.shardsPersist[shardId].KvMap[op.Key] = op.Value
							case AppendType:
								kv.shardsPersist[shardId].KvMap[op.Key] += op.Value
							case GetType:
							default:
								log.Fatalf("invalid command type: %v.", op.OpType)
							}
						}
					}
				} else {
					switch op.OpType {
					case UpConfigType:
						kv.upConfigHandler(op)
					case AddShardType:
						if kv.Config.Num < op.SeqId {
							reply.Err = ConfigNotArrived
							break
						}
						kv.addShardHandler(op)
					case RemoveShardType:
						kv.removeShardHandler(op)
					default:
						log.Fatalf("invalid command type: %v.", op.OpType)
					}
				}

				if kv.maxRaftState != -1 && kv.rf.GetRaftStateSize() > kv.maxRaftState {
					snapshot := kv.PersistSnapShot()
					kv.rf.Snapshot(msg.CommandIndex, snapshot)
				}
				kv.getWaitCh(msg.CommandIndex) <- reply
				kv.mu.Unlock()
			}
			if msg.SnapshotValid {

				if kv.rf.CondInstallSnapshot(msg.SnapshotTerm, msg.SnapshotTerm, msg.Snapshot) {
					// lock put into function
					kv.mu.Lock()
					kv.DecodeSnapShot(msg.Snapshot)
					kv.mu.Unlock()
				}

				continue
			}
		}
	}
}

func (kv *ShardKV) upConfigHandler(op Op) {
	curConfig := kv.Config
	upConfig := op.UpConfig
	if curConfig.Num >= upConfig.Num {
		return
	}
	for shard, gid := range upConfig.Shards {
		if gid == kv.gid && curConfig.Shards[shard] == 0 {
			kv.shardsPersist[shard].KvMap = make(map[string]string)
			kv.shardsPersist[shard].ConfigNum = upConfig.Num
		}
	}
	kv.LastConfig = curConfig
	kv.Config = upConfig

}

func (kv *ShardKV) addShardHandler(op Op) {
	if kv.shardsPersist[op.ShardId].KvMap != nil ||
		op.Shard.ConfigNum < kv.Config.Num {
		return
	}
	kv.shardsPersist[op.ShardId] = kv.cloneShard(op.Shard.ConfigNum, op.Shard.KvMap)
	for clientId, seqId := range op.SeqMap {
		if r, ok := kv.SeqMap[clientId]; !ok || r < seqId {
			kv.SeqMap[clientId] = seqId
		}
	}
}
func (kv *ShardKV) cloneShard(ConfigNum int, KvMap map[string]string) Shard {
	migrateShard := Shard{
		KvMap:     make(map[string]string),
		ConfigNum: ConfigNum,
	}
	for k, v := range KvMap {
		migrateShard.KvMap[k] = v
	}
	return migrateShard
}

func (kv *ShardKV) removeShardHandler(op Op) {
	if op.SeqId < kv.Config.Num {
		return
	}
	kv.shardsPersist[op.ShardId].KvMap = nil
	kv.shardsPersist[op.ShardId].ConfigNum = op.SeqId
}

func (kv *ShardKV) PersistSnapShot() []byte {
	//donnot need lock?
	//kv.mu.Lock()
	//defer kv.mu.Unlock()
	w := new(bytes.Buffer)
	e := labgob.NewEncoder(w)
	e.Encode(kv.shardsPersist)
	e.Encode(kv.SeqMap)
	e.Encode(kv.maxRaftState)
	e.Encode(kv.Config)
	e.Encode(kv.LastConfig)

	data := w.Bytes()
	return data
}

func (kv *ShardKV) ifDuplicate(clientId int64, seqId int) bool {
	//kv.mu.Lock()
	//defer kv.mu.Unlock()
	lastSeqId, exist := kv.SeqMap[clientId]
	if !exist {
		return false
	}
	return seqId <= lastSeqId
}

func (kv *ShardKV) getWaitCh(index int) chan OpReply {
	//kv.mu.Lock()
	//defer kv.mu.Unlock()
	ch, exist := kv.waitChMap[index]
	if !exist {
		kv.waitChMap[index] = make(chan OpReply, 1)
		ch = kv.waitChMap[index]
	}
	return ch
}
