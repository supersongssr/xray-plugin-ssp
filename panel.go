package ssrpanel

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"code.cloudfoundry.org/bytefmt"
	"github.com/jinzhu/gorm"
	"github.com/robfig/cron"
	"github.com/shirou/gopsutil/load"
	"github.com/xtls/xray-core/common/protocol"
	"github.com/xtls/xray-core/common/serial"
	"github.com/xtls/xray-core/proxy/hysteria/account"
	"github.com/xtls/xray-core/proxy/trojan"
	"github.com/xtls/xray-core/proxy/vless"
	"github.com/xtls/xray-core/proxy/vmess"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type Panel struct {
	*Config
	handlerServiceClient *HandlerServiceClient
	statsServiceClient   *StatsServiceClient
	db                   *DB

	mu         sync.Mutex
	isSyncing  atomic.Bool
	userModels []UserModel

	startAt time.Time
	node    *Node
}

func NewPanel(gRPCConn *grpc.ClientConn, db *DB, cfg *Config) (*Panel, error) {
	node, err := db.GetNode(cfg.NodeID)
	if err != nil {
		return nil, err
	}

	newErrorf("node[%d] traffic rate %.2f", node.ID, node.TrafficRate).AtDebug().WriteToLog()

	return &Panel{
		Config:               cfg,
		db:                   db,
		handlerServiceClient: NewHandlerServiceClient(gRPCConn),
		statsServiceClient:   NewStatsServiceClient(gRPCConn),
		startAt:              time.Now(),
		node:                 node,
	}, nil
}

func (p *Panel) Start() {
	doFunc := func() {
		if err := p.do(); err != nil {
			newError("panel#do").Base(err).AtError().WriteToLog()
		}
	}
	doFunc()

	c := cron.New()
	c.AddFunc(fmt.Sprintf("@every %ds", p.CheckRate), doFunc)
	c.Start()
	c.Run()
}

func (p *Panel) do() error {
	// 防重入：确保同步周期不重叠执行
	if !p.isSyncing.CompareAndSwap(false, true) {
		newErrorf("panel#do is still syncing, skipping this cycle to prevent race conditions").AtWarning().WriteToLog()
		return nil
	}
	defer p.isSyncing.Store(false)

	var addedUserCount, deletedUserCount, onlineUsers int
	var uplinkTotal, downlinkTotal uint64

	if err := p.db.DB.DB().Ping(); err != nil {
		p.db.RetryTimes++
		newErrorf("Lost db connection, retry times: %d",
			p.db.RetryTimes).AtDebug().WriteToLog()
		return nil
	}
	p.db.RetryTimes = 0

	defer func() {
		newErrorf("+ %d users, - %d users, ↓ %s, ↑ %s, online %d",
			addedUserCount, deletedUserCount, bytefmt.ByteSize(downlinkTotal), bytefmt.ByteSize(uplinkTotal), onlineUsers).AtDebug().WriteToLog()
	}()

	if err := p.db.DB.Create(&NodeInfo{
		NodeID: p.NodeID,
		Uptime: time.Now().Sub(p.startAt) / time.Second,
		Load:   getSystemLoad(),
	}).Error; err != nil {
		return err
	}

	userTrafficLogs, err := p.getTraffic()
	if err != nil {
		return err
	}
	// onlineUsers = len(userTrafficLogs)
	onlineUsers = 0

	var uVals, dVals string
	var userIDs []uint

	for _, log := range userTrafficLogs {
		uplink := p.mulTrafficRate(log.Uplink)
		downlink := p.mulTrafficRate(log.Downlink)

		if log.Uplink+log.Downlink > 2048 {
			onlineUsers += 1
		}

		uplinkTotal += log.Uplink
		downlinkTotal += log.Downlink

		log.Traffic = bytefmt.ByteSize(uplink + downlink)
		p.db.DB.Create(&log.UserTrafficLog)

		userIDs = append(userIDs, log.UserID)
		uVals += fmt.Sprintf(" WHEN %d THEN u + %d", log.UserID, uplink)
		dVals += fmt.Sprintf(" WHEN %d THEN d + %d", log.UserID, downlink)
	}

	if onlineUsers > 0 {
		p.db.DB.Create(&NodeOnlineLog{
			NodeID:     p.NodeID,
			OnlineUser: onlineUsers,
		})
	}

	if uVals != "" && dVals != "" {
		p.db.DB.Table("user").
			Where("id in (?)", userIDs).
			Updates(map[string]interface{}{
				"u": gorm.Expr(fmt.Sprintf("CASE id %s END", uVals)),
				"d": gorm.Expr(fmt.Sprintf("CASE id %s END", dVals)),
				"t": time.Now().Unix(),
			})
	}

	addedUserCount, deletedUserCount, _ = p.syncUser()
	return nil
}

type userStatsLogs struct {
	UserTrafficLog
	UserPort int
}

func (p *Panel) getTraffic() (logs []userStatsLogs, err error) {
	// 加锁创建 userModels 的快照，避免网络 I/O 期间持有锁
	p.mu.Lock()
	usersSnapshot := make([]UserModel, len(p.userModels))
	copy(usersSnapshot, p.userModels)
	p.mu.Unlock()

	var downlink, uplink uint64
	for _, user := range usersSnapshot {
		downlink, err = p.statsServiceClient.getUserDownlink(user.Email)
		if err != nil {
			return
		}

		uplink, err = p.statsServiceClient.getUserUplink(user.Email)
		if err != nil {
			return
		}

		if uplink+downlink > 0 {
			logs = append(logs, userStatsLogs{
				UserTrafficLog: UserTrafficLog{
					UserID:   user.ID,
					Uplink:   uplink,
					Downlink: downlink,
					NodeID:   p.NodeID,
					Rate:     p.node.TrafficRate,
				},
				UserPort: user.Port,
			})
		}
	}

	return
}

func (p *Panel) mulTrafficRate(traffic uint64) uint64 {
	return uint64(p.node.TrafficRate * float64(traffic))
}

func (p *Panel) syncUser() (addedUserCount, deletedUserCount int, err error) {
	userModels, err := p.db.GetAllUsers(p.NodeID)
	if err != nil {
		return 0, 0, err
	}
	if len(userModels) == 0 && len(p.userModels) == 0 {
		return 0, 0, nil
	}

	// 加锁创建 userModels 的快照
	p.mu.Lock()
	usersSnapshot := make([]UserModel, len(p.userModels))
	copy(usersSnapshot, p.userModels)
	p.mu.Unlock()

	// Calculate addition users
	addUserModels := make([]UserModel, 0)
	for _, userModel := range userModels {
		if inUserModels(&userModel, usersSnapshot) {
			continue
		}
		addUserModels = append(addUserModels, userModel)
	}

	// Calculate deletion users
	delUserModels := make([]UserModel, 0)
	for _, userModel := range usersSnapshot {
		if inUserModels(&userModel, userModels) {
			continue
		}
		delUserModels = append(delUserModels, userModel)
	}

	// 通过在线会话数来限制用户
	var onlineSessions int64
	for _, user := range usersSnapshot {
		onlineSessions, err = p.statsServiceClient.getUserOnlineSessions(user.Email)
		if err != nil {
			return
		}
		if onlineSessions > p.IPLimit {
			if inUserModels(&user, delUserModels) {
				continue
			}
			delUserModels = append(delUserModels, user)
			newErrorf("[IP限制] 用户: %s, 当前在线IP数: %d, 阈值: %d", user.Email, onlineSessions, p.IPLimit).AtDebug().WriteToLog()
		}
	}

	// 预加载 Tags，严禁在内层循环重复调用 GetTags() 以防 Map 频繁分配引发 GC 抖动
	activeTags := p.UserConfig.GetTags()

	// Delete - 并发广播删除
	for _, userModel := range delUserModels {
		var wg sync.WaitGroup
		for _, tag := range activeTags {
			wg.Add(1)
			go func(t string) {
				defer wg.Done()
				if err := p.handlerServiceClient.RemoveUser(t, userModel.Email); err != nil {
					// 忽略 NotFound 错误，因为可能是用户根本就不存在
					if status.Code(err) != codes.NotFound && !strings.Contains(strings.ToLower(err.Error()), "not found") {
						newErrorf("Warning: failed to remove user %s from tag %s: %v", userModel.Email, t, err).AtWarning().WriteToLog()
					}
				}
			}(tag)
		}
		wg.Wait()

		// 只有完成网络调用后，才加锁清理内存状态
		p.mu.Lock()
		if i := findUserModelIndex(&userModel, p.userModels); i != -1 {
			p.userModels = append(p.userModels[:i], p.userModels[i+1:]...)
			deletedUserCount++
			newErrorf("Deleted user: id=%d, V2rayUUID=%s, Email=%s", userModel.ID, userModel.VmessID, userModel.Email).AtDebug().WriteToLog()
		}
		p.mu.Unlock()
	}

	// Add - 并发广播添加与事务性一致性防护
	for _, userModel := range addUserModels {
		var wg sync.WaitGroup
		var successCount atomic.Uint32
		var errCount atomic.Uint32

		for _, tag := range activeTags {
			u := p.convertUser(tag, userModel)
			if u == nil {
				newErrorf("skip add user to tag %s due to unsupported protocol or error, user: %#v", tag, userModel).AtWarning().WriteToLog()
				errCount.Add(1)
				continue
			}

			wg.Add(1)
			go func(t string, userObj *protocol.User) {
				defer wg.Done()
				err := p.handlerServiceClient.AddUser(t, userObj)
				if err != nil {
					// 捕获并识别 AlreadyExists，将其视为成功
					if status.Code(err) == codes.AlreadyExists || strings.Contains(strings.ToLower(err.Error()), "already exists") {
						successCount.Add(1)
					} else {
						newErrorf("Warning: failed to add user %s to tag %s: %v", userModel.Email, t, err).AtWarning().WriteToLog()
						errCount.Add(1)
					}
				} else {
					successCount.Add(1)
				}
			}(tag, u)
		}
		wg.Wait()

		// 只有所有 Tag 都成功（或已存在），才将其加入 p.userModels 追踪列表
		if successCount.Load() > 0 && errCount.Load() == 0 {
			p.mu.Lock()
			if !inUserModels(&userModel, p.userModels) {
				p.userModels = append(p.userModels, userModel)
				addedUserCount++
				newErrorf("Added user: id=%d, V2rayUUID=%s, Email=%s (to %d/%d tags)", userModel.ID, userModel.VmessID, userModel.Email, successCount.Load(), len(activeTags)).AtDebug().WriteToLog()
			}
			p.mu.Unlock()
		} else {
			newErrorf("Failed to add user %s fully (success: %d, err: %d), will retry next cycle", userModel.Email, successCount.Load(), errCount.Load()).AtWarning().WriteToLog()
			
			// 发生异常时，尝试回滚已成功添加的 Tag，防止产生半同步的孤立账号
			if successCount.Load() > 0 {
				newErrorf("Rolling back partially added user %s from tags", userModel.Email).AtWarning().WriteToLog()
				var rollbackWg sync.WaitGroup
				for _, tag := range activeTags {
					rollbackWg.Add(1)
					go func(t string) {
						defer rollbackWg.Done()
						_ = p.handlerServiceClient.RemoveUser(t, userModel.Email)
					}(tag)
				}
				rollbackWg.Wait()
			}
		}
	}

	return
}

func (p *Panel) convertUser(tag string, userModel UserModel) *protocol.User {
	userCfg := p.UserConfig
	inbound := getInboundConfigByTag(tag, p.v2rayConfig.InboundConfigs)
	if inbound == nil {
		return nil
	}

	var accountMsg *serial.TypedMessage

	switch inbound.Protocol {
	case "vless":
		accountMsg = serial.ToTypedMessage(&vless.Account{
			Id:   userModel.VmessID,
			Flow: userCfg.GetFlow(tag),
		})
	case "trojan":
		accountMsg = serial.ToTypedMessage(&trojan.Account{
			Password: userModel.VmessID,
		})
	case "vmess":
		accountMsg = serial.ToTypedMessage(&vmess.Account{
			Id:               userModel.VmessID,
			SecuritySettings: userCfg.securityConfig,
		})
	case "hysteria", "hysteria2":
		accountMsg = serial.ToTypedMessage(&account.Account{
			Auth: userModel.VmessID,
		})
	default:
		newErrorf("Warning: Unsupported protocol '%s' for user %s", inbound.Protocol, userModel.Email).AtWarning().WriteToLog()
		return nil
	}

	return &protocol.User{
		Level:   userCfg.Level,
		Email:   userModel.Email,
		Account: accountMsg,
	}
}

func findUserModelIndex(u *UserModel, userModels []UserModel) int {
	for i, user := range userModels {
		if user == *u {
			return i
		}
	}
	return -1
}

func inUserModels(u *UserModel, userModels []UserModel) bool {
	return findUserModelIndex(u, userModels) != -1
}

func getSystemLoad() string {
	stat, err := load.Avg()
	if err != nil {
		return "0.00 0.00 0.00"
	}

	return fmt.Sprintf("%.2f %.2f %.2f", stat.Load1, stat.Load5, stat.Load15)
}
