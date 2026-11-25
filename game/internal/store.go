package internal

import (
	"leaf_server/base"
	"leaf_server/session"

	"github.com/name5566/leaf/log"
	"github.com/name5566/leaf/util"
	"gopkg.in/mgo.v2"
)

// playerWriter 抽象玩家数据的实际写入动作，生产环境由 mongodb 实现，
// 测试环境可注入假实现（避免依赖真实数据库与 nil mgodb）。
type playerWriter interface {
	// Write 异步写入玩家数据。
	Write(p *Player)
	// WriteSync 同步写入玩家数据，失败返回非 nil error。
	WriteSync(p *Player) error
}

// mgoWriter 生产环境的写入器，封装对 mongodb 的访问。
type mgoWriter struct{}

func (mgoWriter) Write(p *Player) {
	mgodb.Set(base.DBTask{
		ObjID:      p.objid,
		DB:         base.DBNAME,
		Collection: base.PLAYERSET,
		Key:        "_id",
		KeyV:       base.BsonObjectID(p.objid),
		Ret:        util.DeepClone(p),
		Cb: func(param interface{}, err error) {
			if err != nil {
				log.Error("save playerdata failed:", p.objid, " ", err.Error())
			}
		},
	})
}

func (mgoWriter) WriteSync(p *Player) error {
	if err := mgodb.SetSync(base.DBNAME, base.PLAYERSET, "_id", base.BsonObjectID(p.objid), p); err != nil {
		log.Error("save playerdata failed:", p.objid, " ", err.Error())
		return err
	}
	return nil
}

// playerStore 是玩家保存动作当前使用的写入器，可在测试中替换。
var playerStore playerWriter = mgoWriter{}

// SessionStore 是会话流程使用的存储抽象，生产环境为 dbStore（mongodb），
// 测试时可替换为本地可控的假实现。
var SessionStore session.Store = &dbStore{}

// dbStore 把游戏服的 mongodb 封装适配为 session.Store。
// 方法在 game 模块的 skeleton 主 goroutine 上调用，
// 异步任务通过 mgodb 的 LinearContext 执行，回调仍回到主 goroutine。
type dbStore struct{}

var _ session.Store = (*dbStore)(nil)

// Load 加载玩家存档。库中不存在时视为新玩家：返回零值 Player 与 nil error，
// 由登录流程（rpcLoginSuccess）负责 InitData 与首次落库。
func (st *dbStore) Load(account string, cb func(data interface{}, err error)) {
	mgodb.Get(base.DBTask{
		ObjID:      account,
		DB:         base.DBNAME,
		Collection: base.PLAYERSET,
		Key:        "_id",
		KeyV:       base.BsonObjectID(account),
		Ret:        CreatePlayer(),
		Cb: func(param interface{}, err error) {
			if err == mgo.ErrNotFound {
				// 新玩家：Ret 已是 CreatePlayer() 给出的零值对象。
				err = nil
			}
			cb(param, err)
		},
	})
}

// Save 异步落库（断线/退出时使用）。
func (st *dbStore) Save(account string, data interface{}) {
	data.(*Player).Save()
}

// SaveSync 同步落库（关停时使用），错误原样上抛，不得静默丢弃。
func (st *dbStore) SaveSync(account string, data interface{}) error {
	return data.(*Player).SaveSync()
}
