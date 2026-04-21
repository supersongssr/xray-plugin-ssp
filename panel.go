package ssrpanel

import (
	"fmt"
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
)

type Panel struct {
	*Config
	handlerServiceClient *HandlerServiceClient
	statsServiceClient   *StatsServiceClient
	db                   *DB
	userModels           []UserModel
	startAt              time.Time
	node                 *Node
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
		handlerServiceClient: NewHandlerServiceClient(gRPCConn, cfg.UserConfig.InboundTag),
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
	var downlink, uplink uint64
	for _, user := range p.userModels {
		downlink, err = p.statsServiceClient.getUserDownlink(user.Email)
		if err != nil {
			return
		}

		uplink, err = p.statsServiceClient.getUserUplink(user.Email)
		if err != nil {
			return
		}

		if uplink+downlink > 0 {
			if err != nil {
				return
			}

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
	if len(userModels) == 0 {
		return 0, 0, err
	}

	// Calculate addition users
	addUserModels := make([]UserModel, 0)
	for _, userModel := range userModels {
		if inUserModels(&userModel, p.userModels) {
			continue
		}

		addUserModels = append(addUserModels, userModel)
	}

	// Calculate deletion users
	delUserModels := make([]UserModel, 0)
	for _, userModel := range p.userModels {
		if inUserModels(&userModel, userModels) {
			continue
		}

		delUserModels = append(delUserModels, userModel)
	}

	// 通过在线会话数来限制用户
	var onlineSessions int64
	for _, user := range p.userModels { //遍历之前的userModels 就是上一次的用户数量. 用来统计用户的在线会话
		onlineSessions, err = p.statsServiceClient.getUserOnlineSessions(user.Email)
		// newErrorf("============= User email  : %s", user.Email).AtDebug().WriteToLog()
		// newErrorf("============= Online sessions: %d", onlineSessions).AtDebug().WriteToLog()
		if err != nil {
			return
		}
		if onlineSessions > p.IPLimit { //如果用户的在线会话数,大于了系统规定的数量. 就删除该用户.下次连接,再加入该用户.
			if inUserModels(&user, delUserModels) {
				continue // 如果在删除用户列表中,就跳过
			}
			delUserModels = append(delUserModels, user) //把该用户添加到删除用户列表中
			newErrorf("[IP限制] 用户: %s, 当前在线IP数: %d, 阈值: %d", user.Email, onlineSessions, p.IPLimit).AtDebug().WriteToLog()
		}
	}

	// Delete
	for _, userModel := range delUserModels {
		if i := findUserModelIndex(&userModel, p.userModels); i != -1 {
			p.userModels = append(p.userModels[:i], p.userModels[i+1:]...)
			if err = p.handlerServiceClient.DelUser(userModel.Email); err != nil {
				return
			}
			deletedUserCount++
			newErrorf("Deleted user: id=%d, VmessID=%s, Email=%s", userModel.ID, userModel.VmessID, userModel.Email).AtDebug().WriteToLog()
		}
	}

	// Add
	for _, userModel := range addUserModels {
		u := p.convertUser(userModel)
		if u == nil {
			newErrorf("skip add user due to unsupported protocol or error, user: %#v", userModel).AtWarning().WriteToLog()
			continue
		}
		if err = p.handlerServiceClient.AddUser(u); err != nil {
			if p.IgnoreEmptyVmessID {
				newErrorf("add user err \"%s\" user: %#v", err, userModel).AtWarning().WriteToLog()
				continue
			}
			fatal("add user err ", err, userModel)
		}
		p.userModels = append(p.userModels, userModel)
		addedUserCount++
		newErrorf("Added user: id=%d, VmessID=%s, Email=%s", userModel.ID, userModel.VmessID, userModel.Email).AtDebug().WriteToLog()
	}

	return
}

func (p *Panel) convertUser(userModel UserModel) *protocol.User {
	userCfg := p.UserConfig
	inbound := getInboundConfigByTag(p.UserConfig.InboundTag, p.v2rayConfig.InboundConfigs)
	if inbound == nil {
		return nil
	}

	var accountMsg *serial.TypedMessage

	switch inbound.Protocol {
	case "vless":
		accountMsg = serial.ToTypedMessage(&vless.Account{
			Id:   userModel.VmessID,
			Flow: userCfg.Flow,
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
